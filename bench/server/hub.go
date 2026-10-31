package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// App is one tenant. In production it would come from the DynamoDB projection; here from env.
type App struct {
	ID             string
	Key            string
	Secret         string
	MaxConnections int64 // 0 = unlimited
	MaxMessageSize int64 // bytes, client frames and HTTP bodies
}

type member struct {
	info    json.RawMessage
	sockets int // one user may have several tabs open
}

type channel struct {
	conns   map[*Conn]string // value: presence user id ("" on other channels)
	members map[string]*member
}

// Hub holds the channels of one app.
type Hub struct {
	app         *App
	connections atomic.Int64

	mu       sync.RWMutex
	channels map[string]*channel
}

func newHub(app *App) *Hub {
	return &Hub{app: app, channels: map[string]*channel{}}
}

func isPresence(name string) bool { return strings.HasPrefix(name, "presence-") }

func isPrivate(name string) bool {
	return strings.HasPrefix(name, "private-") || isPresence(name)
}

// subscribe adds c to the channel. For presence channels it returns the presence payload for c and whether the
// user is new to the channel (so member_added must go to the others).
func (h *Hub) subscribe(c *Conn, name, userID string, info json.RawMessage) (presence []byte, newMember bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := h.channels[name]
	if ch == nil {
		ch = &channel{conns: map[*Conn]string{}}
		if isPresence(name) {
			ch.members = map[string]*member{}
		}
		h.channels[name] = ch
	}
	if _, already := ch.conns[c]; already {
		return h.presencePayload(ch), false
	}
	ch.conns[c] = userID

	if ch.members == nil {
		return nil, false
	}
	m := ch.members[userID]
	if m == nil {
		m = &member{info: info}
		ch.members[userID] = m
		newMember = true
	}
	m.sockets++
	return h.presencePayload(ch), newMember
}

// unsubscribe removes c and reports the user id that left the presence channel, if its last socket left.
func (h *Hub) unsubscribe(c *Conn, name string) (goneUserID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := h.channels[name]
	if ch == nil {
		return ""
	}
	userID, ok := ch.conns[c]
	if !ok {
		return ""
	}
	delete(ch.conns, c)
	if ch.members != nil {
		if m := ch.members[userID]; m != nil {
			m.sockets--
			if m.sockets == 0 {
				delete(ch.members, userID)
				goneUserID = userID
			}
		}
	}
	if len(ch.conns) == 0 {
		delete(h.channels, name)
	}
	return goneUserID
}

// presencePayload is the subscription_succeeded data for a presence channel. Caller holds the lock.
func (h *Hub) presencePayload(ch *channel) []byte {
	ids := make([]string, 0, len(ch.members))
	hash := make(map[string]json.RawMessage, len(ch.members))
	for id, m := range ch.members {
		ids = append(ids, id)
		if m.info == nil {
			hash[id] = json.RawMessage("null")
		} else {
			hash[id] = m.info
		}
	}
	sort.Strings(ids)
	out, _ := json.Marshal(map[string]any{"presence": map[string]any{"ids": ids, "hash": hash, "count": len(ids)}})
	return out
}

// publish sends a pre-encoded frame to every connection on the channel except `except`.
func (h *Hub) publish(name string, frame []byte, except *Conn) int {
	h.mu.RLock()
	ch := h.channels[name]
	if ch == nil {
		h.mu.RUnlock()
		return 0
	}
	targets := make([]*Conn, 0, len(ch.conns))
	for c := range ch.conns {
		if c != except {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	wire := compileText(frame) // one WebSocket frame for every recipient
	for _, c := range targets {
		c.writeFrame(wire)
	}
	return len(targets)
}

func (h *Hub) subscribed(c *Conn, name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ch := h.channels[name]
	if ch == nil {
		return false
	}
	_, ok := ch.conns[c]
	return ok
}

type channelInfo struct {
	SubscriptionCount int
	UserCount         int
	Presence          bool
}

func (h *Hub) channelList(prefix string) map[string]channelInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := map[string]channelInfo{}
	for name, ch := range h.channels {
		if strings.HasPrefix(name, prefix) {
			out[name] = channelInfo{len(ch.conns), len(ch.members), ch.members != nil}
		}
	}
	return out
}

func (h *Hub) users(name string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := []string{}
	if ch := h.channels[name]; ch != nil {
		for id := range ch.members {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
