// Benchmark server, copied from arco-ws spike/b (engine B) with one change: TLS comes from this repo's
// crypto/tls (Go 1.27.3 crypto/tls plus the non-blocking layer) instead of lesismal/llib/std/crypto/tls.
// nbhttp's non-blocking epoll/kqueue engine serves HTTP and WebSocket from a few poller goroutines, with no
// goroutine per connection. TLS=1 serves TLS on the same port with a self-signed certificate generated at
// start. ARCO_TLS_NO_RELEASE=1 disables releasing idle read buffers (measurement only). One app from env.
package main

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lesismal/nbio/crypto/tls"
	"github.com/lesismal/nbio/nbhttp"
	"github.com/lesismal/nbio/nbhttp/websocket"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int64) int64 {
	n, err := strconv.ParseInt(env(key, ""), 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

type server struct {
	byKey    map[string]*Hub
	byID     map[string]*Hub
	upgrader *websocket.Upgrader
	conns    sync.Map // *Conn -> struct{}, for the ping sweep
}

// Low-memory nbio settings (recorded in docs/engine-spike.md).
const (
	readBufferSize = 4 << 10 // per poller read buffer (nbio default 64 KiB)
	maxLoad        = 60_000  // max online connections (nbio default 10k)
	httpKeepalive  = 30 * time.Second
	maxWriteBuffer = 64 << 10 // per connection bytes nbio may queue before closing a slow consumer
)

func main() {
	app := &App{
		ID:             env("APP_ID", "app-id"),
		Key:            env("APP_KEY", "app-key"),
		Secret:         env("APP_SECRET", "app-secret"),
		MaxConnections: envInt("APP_MAX_CONNECTIONS", 0),
		MaxMessageSize: envInt("APP_MAX_MESSAGE_SIZE", 10_000),
	}
	hub := newHub(app)
	s := &server{byKey: map[string]*Hub{app.Key: hub}, byID: map[string]*Hub{app.ID: hub}}

	u := websocket.NewUpgrader()
	u.CheckOrigin = func(*http.Request) bool { return true }
	u.KeepaliveTime = dropAfter
	u.MessageLengthLimit = int(app.MaxMessageSize)
	u.ReleasePayload = true
	u.OnMessage(func(ws *websocket.Conn, mt websocket.MessageType, data []byte) {
		c, _ := ws.Session().(*Conn)
		if c == nil {
			return
		}
		c.lastSeen.Store(time.Now().Unix())
		if mt == websocket.TextMessage {
			c.handle(data)
		}
	})
	u.SetPongHandler(func(ws *websocket.Conn, _ string) {
		if c, _ := ws.Session().(*Conn); c != nil {
			c.lastSeen.Store(time.Now().Unix())
		}
	})
	u.OnClose(func(ws *websocket.Conn, err error) {
		diagClose("ws", err)
		if c, _ := ws.Session().(*Conn); c != nil {
			s.conns.Delete(c)
			c.cleanup()
		}
	})
	s.upgrader = u

	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/{key}", s.websocket)
	mux.HandleFunc("POST /apps/{id}/events", s.api(s.events))
	mux.HandleFunc("POST /apps/{id}/batch_events", s.api(s.batchEvents))
	mux.HandleFunc("GET /apps/{id}/channels", s.api(s.channels))
	mux.HandleFunc("GET /apps/{id}/channels/{name}", s.api(s.channel))
	mux.HandleFunc("GET /apps/{id}/channels/{name}/users", s.api(s.users))
	mux.HandleFunc("GET /up", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	addr := ":" + env("PORT", "6001")
	conf := nbhttp.Config{
		Network:                      "tcp",
		Handler:                      mux,
		NPoller:                      runtime.GOMAXPROCS(0),
		ReadBufferSize:               readBufferSize,
		MaxLoad:                      maxLoad,
		KeepaliveTime:                httpKeepalive,
		MaxWriteBufferSize:           maxWriteBuffer,
		MaxWebsocketFramePayloadSize: int(app.MaxMessageSize),
		MaxHTTPBodySize:              int(app.MaxMessageSize) + 1,
		ReleaseWebsocketPayload:      true,
	}
	if env("TLS", "") == "1" {
		conf.AddrsTLS = []string{addr}
		conf.TLSConfig = &tls.Config{Certificates: []tls.Certificate{selfSigned()}, MinVersion: tls.VersionTLS12}
		tls.DisableBufferRelease = env("ARCO_TLS_NO_RELEASE", "") == "1"
	} else {
		conf.Addrs = []string{addr}
	}
	engine := nbhttp.NewEngine(conf)
	engine.OnClose(func(_ net.Conn, err error) { diagClose("conn", err) })
	u.Engine = engine
	if env("ARCO_DIAG", "") == "1" {
		go diagLoop()
	}
	if err := engine.Start(); err != nil {
		log.Fatal(err)
	}
	log.Printf("arco-nbio bench server listening on %s tls=%v pollers=%d (app %s)", addr, conf.TLSConfig != nil, conf.NPoller, app.ID)
	go s.sweep()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	engine.Stop()
}

func (s *server) websocket(w http.ResponseWriter, r *http.Request) {
	hub := s.byKey[r.PathValue("key")]
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if hub == nil {
		_ = ws.WriteMessage(websocket.TextMessage, frame("pusher:error", "", map[string]any{"code": 4001, "message": "App key not found"}))
		ws.Close()
		return
	}
	if n := hub.connections.Add(1); hub.app.MaxConnections > 0 && n > hub.app.MaxConnections {
		hub.connections.Add(-1)
		_ = ws.WriteMessage(websocket.TextMessage, frame("pusher:error", "", map[string]any{"code": 4004, "message": "Application is over connection quota"}))
		ws.Close()
		return
	}
	c := &Conn{ws: ws, hub: hub, socketID: newSocketID(), channels: map[string]struct{}{}}
	c.lastSeen.Store(time.Now().Unix())
	ws.SetSession(c)
	s.conns.Store(c, struct{}{})
	established, _ := json.Marshal(map[string]any{"socket_id": c.socketID, "activity_timeout": activityTimeout})
	c.enqueue(frame("pusher:connection_established", "", string(established)))
}

// sweep pings connections quiet for pingAfter; nbio's read deadline (KeepaliveTime) drops the dead ones.
func (s *server) sweep() {
	for range time.Tick(sweepEvery) {
		now := time.Now().Unix()
		s.conns.Range(func(k, _ any) bool {
			c := k.(*Conn)
			if time.Duration(now-c.lastSeen.Load())*time.Second > pingAfter {
				c.writeFrame(pingFrame)
			}
			return true
		})
	}
}

type apiHandler func(w http.ResponseWriter, r *http.Request, hub *Hub, body []byte)

// api checks the Pusher HTTP signature: HMAC-SHA256 over "METHOD\nPATH\nsorted query without auth_signature".
func (s *server) api(next apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hub := s.byID[r.PathValue("id")]
		if hub == nil {
			http.Error(w, `{"error":"Unknown app"}`, http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, hub.app.MaxMessageSize+1))
		if err != nil || int64(len(body)) > hub.app.MaxMessageSize {
			http.Error(w, `{"error":"Payload too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		if !validSignature(r, body, hub.app) {
			http.Error(w, `{"error":"Invalid signature"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		next(w, r, hub, body)
	}
}

func validSignature(r *http.Request, body []byte, app *App) bool {
	q := r.URL.Query()
	if q.Get("auth_key") != app.Key {
		return false
	}
	ts, err := strconv.ParseInt(q.Get("auth_timestamp"), 10, 64)
	if err != nil || abs(time.Now().Unix()-ts) > 600 {
		return false
	}
	if len(body) > 0 {
		sum := md5.Sum(body)
		if hex.EncodeToString(sum[:]) != q.Get("body_md5") {
			return false
		}
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		if k != "auth_signature" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strings.ToLower(k) + "=" + q.Get(k)
	}
	mac := hmac.New(sha256.New, []byte(app.Secret))
	mac.Write([]byte(r.Method + "\n" + r.URL.Path + "\n" + strings.Join(parts, "&")))
	return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(q.Get("auth_signature")))
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

type publishRequest struct {
	Name     string   `json:"name"`
	Data     string   `json:"data"`
	Channel  string   `json:"channel"`
	Channels []string `json:"channels"`
	SocketID string   `json:"socket_id"`
}

func (s *server) publish(hub *Hub, p publishRequest) {
	names := p.Channels
	if p.Channel != "" {
		names = append(names, p.Channel)
	}
	// socket_id exclusion ("toOthers") is not implemented in this prototype.
	for _, name := range names {
		hub.publish(name, frame(p.Name, name, p.Data), nil)
	}
}

func (s *server) events(w http.ResponseWriter, _ *http.Request, hub *Hub, body []byte) {
	var p publishRequest
	if json.Unmarshal(body, &p) != nil || p.Name == "" {
		http.Error(w, `{"error":"Invalid body"}`, http.StatusBadRequest)
		return
	}
	s.publish(hub, p)
	w.Write([]byte("{}"))
}

func (s *server) batchEvents(w http.ResponseWriter, _ *http.Request, hub *Hub, body []byte) {
	var b struct {
		Batch []publishRequest `json:"batch"`
	}
	if json.Unmarshal(body, &b) != nil {
		http.Error(w, `{"error":"Invalid body"}`, http.StatusBadRequest)
		return
	}
	for _, p := range b.Batch {
		s.publish(hub, p)
	}
	w.Write([]byte(`{"batch":[]}`))
}

func (s *server) channels(w http.ResponseWriter, r *http.Request, hub *Hub, _ []byte) {
	info := r.URL.Query().Get("info")
	out := map[string]map[string]int{}
	for name, ch := range hub.channelList(r.URL.Query().Get("filter_by_prefix")) {
		attrs := map[string]int{}
		if strings.Contains(info, "subscription_count") {
			attrs["subscription_count"] = ch.SubscriptionCount
		}
		if strings.Contains(info, "user_count") && ch.Presence {
			attrs["user_count"] = ch.UserCount
		}
		out[name] = attrs
	}
	json.NewEncoder(w).Encode(map[string]any{"channels": out})
}

func (s *server) channel(w http.ResponseWriter, r *http.Request, hub *Hub, _ []byte) {
	ch, ok := hub.channelList("")[r.PathValue("name")]
	res := map[string]any{"occupied": ok, "subscription_count": ch.SubscriptionCount}
	if ch.Presence {
		res["user_count"] = ch.UserCount
	}
	json.NewEncoder(w).Encode(res)
}

func (s *server) users(w http.ResponseWriter, r *http.Request, hub *Hub, _ []byte) {
	users := []map[string]string{}
	for _, id := range hub.users(r.PathValue("name")) {
		users = append(users, map[string]string{"id": id})
	}
	json.NewEncoder(w).Encode(map[string]any{"users": users})
}

// Diagnostics (ARCO_DIAG=1): every 2 s, log close errors by message, TLS
// handshakes in flight, goroutines and the longest scheduling delay seen by a
// 10 ms ticker (a stalled process shows up as a large delay).
var (
	diagMu   sync.Mutex
	diagErrs = map[string]int{}
)

func diagClose(kind string, err error) {
	msg := "nil"
	if err != nil {
		msg = err.Error()
	}
	diagMu.Lock()
	diagErrs[kind+": "+msg]++
	diagMu.Unlock()
}

func diagLoop() {
	tick := time.NewTicker(10 * time.Millisecond)
	report := time.NewTicker(2 * time.Second)
	last := time.Now()
	var maxDelay time.Duration
	for {
		select {
		case now := <-tick.C:
			if d := now.Sub(last) - 10*time.Millisecond; d > maxDelay {
				maxDelay = d
			}
			last = now
		case <-report.C:
			diagMu.Lock()
			errs := diagErrs
			diagErrs = map[string]int{}
			diagMu.Unlock()
			log.Printf("diag: handshakes=%d goroutines=%d maxTickDelay=%v closes=%v",
				tls.HandshakesInFlight(), runtime.NumGoroutine(), maxDelay, errs)
			maxDelay = 0
		}
	}
}
