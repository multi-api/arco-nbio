package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lesismal/nbio/nbhttp/websocket"
)

const (
	activityTimeout = 30 // seconds, sent to clients; pusher-js pings after this much silence
	sweepEvery      = 30 * time.Second
	pingAfter       = 60 * time.Second
	dropAfter       = 150 * time.Second // nbio read deadline (Upgrader.KeepaliveTime), reset on every frame read
)

// Conn is one WebSocket client on nbio. It owns no goroutine: nbio's pollers parse frames and call onMessage,
// writers call writeFrame, which hands bytes to nbio's non-blocking writer (plain fd or nbio's TLS conn).
type Conn struct {
	ws       *websocket.Conn
	hub      *Hub
	socketID string
	lastSeen atomic.Int64
	closed   atomic.Bool

	mu       sync.Mutex
	channels map[string]struct{}
}

type wireEvent struct {
	Event   string `json:"event"`
	Channel string `json:"channel,omitempty"`
	Data    any    `json:"data,omitempty"`
	UserID  string `json:"user_id,omitempty"`
}

func frame(event, channel string, data any) []byte {
	b, _ := json.Marshal(wireEvent{Event: event, Channel: channel, Data: data})
	return b
}

func newSocketID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d.%d", binary.BigEndian.Uint32(b[:4])%1e9, binary.BigEndian.Uint32(b[4:])%1e9)
}

// compileText encodes one unmasked server text frame, so a broadcast is encoded once for every recipient.
func compileText(msg []byte) []byte {
	n := len(msg)
	var h []byte
	switch {
	case n < 126:
		h = []byte{0x81, byte(n)}
	case n <= 65535:
		h = []byte{0x81, 126, byte(n >> 8), byte(n)}
	default:
		h = []byte{0x81, 127, 0, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	return append(h, msg...)
}

var pingFrame = []byte{0x89, 0}

// writeFrame writes an already-encoded frame. nbio buffers what the socket cannot take at once (bounded by
// Config.MaxWriteBufferSize, past which nbio closes the connection), so this never blocks the caller.
func (c *Conn) writeFrame(wire []byte) {
	if c.closed.Load() {
		return
	}
	if _, err := c.ws.Conn.Write(wire); err != nil {
		c.close()
	}
}

func (c *Conn) enqueue(msg []byte) { c.writeFrame(compileText(msg)) }

func (c *Conn) close() { _ = c.ws.Close() }

// cleanup runs once from nbio's OnClose.
func (c *Conn) cleanup() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.mu.Lock()
	names := make([]string, 0, len(c.channels))
	for name := range c.channels {
		names = append(names, name)
	}
	c.channels = nil
	c.mu.Unlock()
	for _, name := range names {
		c.leave(name)
	}
	c.hub.connections.Add(-1)
}

func (c *Conn) handle(raw []byte) {
	var msg struct {
		Event   string          `json:"event"`
		Channel string          `json:"channel"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}

	switch {
	case msg.Event == "pusher:ping":
		c.enqueue(frame("pusher:pong", "", "{}"))
	case msg.Event == "pusher:subscribe":
		var d struct {
			Channel     string `json:"channel"`
			Auth        string `json:"auth"`
			ChannelData string `json:"channel_data"`
		}
		if json.Unmarshal(msg.Data, &d) == nil {
			c.subscribe(d.Channel, d.Auth, d.ChannelData)
		}
	case msg.Event == "pusher:unsubscribe":
		var d struct {
			Channel string `json:"channel"`
		}
		if json.Unmarshal(msg.Data, &d) == nil {
			c.mu.Lock()
			delete(c.channels, d.Channel)
			c.mu.Unlock()
			c.leave(d.Channel)
		}
	case strings.HasPrefix(msg.Event, "client-"):
		c.clientEvent(msg.Event, msg.Channel, msg.Data)
	}
}

func (c *Conn) subscribe(name, auth, channelData string) {
	app := c.hub.app
	if isPrivate(name) {
		toSign := c.socketID + ":" + name
		if isPresence(name) {
			toSign += ":" + channelData
		}
		mac := hmac.New(sha256.New, []byte(app.Secret))
		mac.Write([]byte(toSign))
		want := app.Key + ":" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(auth)) {
			c.subscriptionError(name, "Invalid signature")
			return
		}
	}

	var userID string
	var info json.RawMessage
	if isPresence(name) {
		var cd struct {
			UserID   json.RawMessage `json:"user_id"`
			UserInfo json.RawMessage `json:"user_info"`
		}
		if json.Unmarshal([]byte(channelData), &cd) != nil {
			c.subscriptionError(name, "Invalid channel_data")
			return
		}
		userID = rawID(cd.UserID) // accept "1" and 1, as Pusher does
		if userID == "" {
			c.subscriptionError(name, "Missing user_id")
			return
		}
		info = cd.UserInfo
	}

	c.mu.Lock()
	if c.channels == nil { // connection already closing
		c.mu.Unlock()
		return
	}
	c.channels[name] = struct{}{}
	c.mu.Unlock()

	presence, newMember := c.hub.subscribe(c, name, userID, info)
	if presence == nil {
		c.enqueue(frame("pusher_internal:subscription_succeeded", name, "{}"))
		return
	}
	c.enqueue(frame("pusher_internal:subscription_succeeded", name, string(presence)))
	if newMember {
		added, _ := json.Marshal(map[string]any{"user_id": userID, "user_info": info})
		c.hub.publish(name, frame("pusher_internal:member_added", name, string(added)), c)
	}
}

func (c *Conn) leave(name string) {
	if gone := c.hub.unsubscribe(c, name); gone != "" {
		removed, _ := json.Marshal(map[string]string{"user_id": gone})
		c.hub.publish(name, frame("pusher_internal:member_removed", name, string(removed)), nil)
	}
}

func (c *Conn) subscriptionError(name, reason string) {
	c.enqueue(frame("pusher:subscription_error", name, map[string]any{"type": "AuthError", "error": reason, "status": 401}))
}

// clientEvent forwards a whisper to the other subscribers. Only members of private/presence channels may send.
func (c *Conn) clientEvent(event, name string, data json.RawMessage) {
	if !isPrivate(name) || !c.hub.subscribed(c, name) {
		c.enqueue(frame("pusher:error", "", map[string]any{"code": 4301, "message": "Client events need a subscribed private or presence channel"}))
		return
	}
	ev := wireEvent{Event: event, Channel: name, Data: data}
	if isPresence(name) {
		ev.UserID = c.presenceUserID(name)
	}
	b, _ := json.Marshal(ev)
	c.hub.publish(name, b, c)
}

func (c *Conn) presenceUserID(name string) string {
	c.hub.mu.RLock()
	defer c.hub.mu.RUnlock()
	if ch := c.hub.channels[name]; ch != nil {
		return ch.conns[c]
	}
	return ""
}

func rawID(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
