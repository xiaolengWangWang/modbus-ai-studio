// Package web 是 Linux Web 版：在网关或服务器上运行，用浏览器调试 Modbus 设备。
// 连接、读取表、写入验证、报文监视都在服务端进行，浏览器只负责显示和操作；多个浏览器看到的是同一份状态。
// 收发报文按天记进 SQLite（表结构与桌面版相同），可在页面上下载。
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modbus-ai-studio/internal/recorder"
)

//go:embed static
var static embed.FS

// Options 是 Web 服务的参数。
type Options struct {
	Version  string
	Password string             // 访问口令，不能为空
	Recorder *recorder.Recorder // 报文记录，为 nil 时不记录
	Secure   bool               // 通过 HTTPS 提供服务时为 true，登录 Cookie 只走 HTTPS
}

// Server 是 Web 服务。用 Handler 提供 HTTP，退出前调用 Close。
type Server struct {
	opts    Options
	auth    *auth
	hub     hub
	dev     *device
	packets packetRing
	logs    logRing
	dirty   atomic.Bool // 状态有变化，下一次推送时发给浏览器

	mu      sync.Mutex
	tables  []*table
	tableID int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 创建服务并开始向浏览器推送状态。
func New(opts Options) (*Server, error) {
	if opts.Password == "" {
		return nil, errors.New("web: 访问口令不能为空")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{opts: opts, auth: newAuth(opts.Password, opts.Secure), ctx: ctx, cancel: cancel}
	s.dev = &device{s: s, state: stateDisconnected}
	s.hub.clients = make(map[chan event]struct{})
	s.wg.Go(s.pump)
	return s, nil
}

// Close 停止全部读取表、断开连接。报文记录由调用方关闭。
func (s *Server) Close() {
	s.cancel()
	s.mu.Lock()
	for _, t := range s.tables {
		t.stop()
	}
	s.mu.Unlock()
	s.dev.disconnect()
	s.wg.Wait()
}

// Handler 返回全部页面和接口。除登录外的接口都要先登录。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	files, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(files))
	mux.HandleFunc("POST /api/login", s.auth.login)
	mux.HandleFunc("POST /api/logout", s.auth.logout)
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"loggedIn": s.auth.valid(r), "version": s.opts.Version})
	})
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth.require(h)) }
	api("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.state()) })
	api("GET /api/events", s.events)
	api("GET /api/packets", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.packets.since(0)) })
	api("POST /api/connect", s.handleConnect)
	api("POST /api/disconnect", s.handleDisconnect)
	api("POST /api/demo", s.handleDemo)
	api("POST /api/tables", s.handleAddTable)
	api("PUT /api/tables/{id}", s.handleUpdateTable)
	api("DELETE /api/tables/{id}", s.handleDeleteTable)
	api("POST /api/tables/{id}/pause", s.handlePauseTable)
	api("POST /api/pause-all", s.handlePauseAll)
	api("POST /api/write", s.handleWrite)
	api("POST /api/detect", s.handleDetect)
	api("GET /api/serial-ports", s.handleSerialPorts)
	api("GET /api/records", s.handleRecords)
	api("GET /api/records/{name}", s.handleDownload)
	return secure(mux)
}

// secure 加上安全相关的响应头，并拒绝跨站提交：写操作的 Origin 必须是本站。
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					writeError(w, http.StatusForbidden, "拒绝跨站请求")
					return
				}
			}
			// 写操作一律用 JSON（没有内容也发 {}）：浏览器跨站提交表单做不到这一点。
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "请求格式应为 JSON")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 登录 ----

const (
	cookieName = "modbus_ai_session"
	sessionTTL = 12 * time.Hour // 12 小时不操作需要重新登录
	maxFails   = 5              // 同一来源 5 分钟内最多输错 5 次
	failWindow = 5 * time.Minute
)

// failDelay 是口令输错后的等待，拖慢逐个试口令；测试时调小。
var failDelay = 500 * time.Millisecond

type auth struct {
	sum      [32]byte
	secure   bool
	mu       sync.Mutex
	sessions map[string]time.Time   // 令牌 → 过期时间
	fails    map[string][]time.Time // 来源 IP → 输错的时间
}

func newAuth(password string, secure bool) *auth {
	return &auth{sum: sha256.Sum256([]byte(password)), secure: secure, sessions: map[string]time.Time{}, fails: map[string][]time.Time{}}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ip, now := clientIP(r), time.Now()
	a.mu.Lock()
	recent := a.fails[ip][:0]
	for _, t := range a.fails[ip] {
		if now.Sub(t) < failWindow {
			recent = append(recent, t)
		}
	}
	a.fails[ip] = recent
	blocked := len(recent) >= maxFails
	a.mu.Unlock()
	if blocked {
		writeError(w, http.StatusTooManyRequests, "口令输错次数过多，请 5 分钟后再试")
		return
	}
	sum := sha256.Sum256([]byte(in.Password))
	if subtle.ConstantTimeCompare(sum[:], a.sum[:]) != 1 {
		a.mu.Lock()
		a.fails[ip] = append(a.fails[ip], now)
		a.mu.Unlock()
		time.Sleep(failDelay)
		writeError(w, http.StatusUnauthorized, "口令不对")
		return
	}
	token := rand.Text()
	a.mu.Lock()
	delete(a.fails, ip)
	for t, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, t)
		}
	}
	a.sessions[token] = now.Add(sessionTTL)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// valid 检查登录令牌，有效时顺延过期时间。
func (a *auth) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	a.sessions[c.Value] = time.Now().Add(sessionTTL)
	return true
}

func (a *auth) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.valid(r) {
			writeError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- JSON ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON 读 JSON 请求体。只接受 application/json：浏览器跨站提交表单做不到这一点。
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "请求格式应为 JSON")
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "请求内容不对："+err.Error())
		return false
	}
	return true
}

// ---- 推送 ----

type event struct {
	name string
	data []byte
}

// hub 把状态和报文推给所有打开的页面（Server-Sent Events）。页面处理不过来时丢弃，下一次状态会补齐。
type hub struct {
	mu      sync.Mutex
	clients map[chan event]struct{}
}

func (h *hub) subscribe() chan event {
	ch := make(chan event, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *hub) listening() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

func (h *hub) publish(name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- event{name, data}:
		default:
		}
	}
}

// changed 标记状态有变化，最多每 250 ms 推送一次。
func (s *Server) changed() { s.dirty.Store(true) }

func (s *Server) pump() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	last := s.packets.last()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
		if !s.hub.listening() {
			last = s.packets.last()
			continue
		}
		if s.dirty.Swap(false) {
			s.hub.publish("state", s.state())
		}
		if ps := s.packets.since(last); len(ps) > 0 {
			last = ps[len(ps)-1].Seq
			s.hub.publish("packets", ps)
		}
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "不支持推送")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no") // 经过 nginx 反向代理时不缓冲
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	send := func(name string, data []byte) bool {
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		flusher.Flush()
		return err == nil
	}
	if data, err := json.Marshal(s.state()); err != nil || !send("state", data) {
		return
	}
	// 带上页面已有的最后一条报文序号（since）时，先补发之后的报文：刚打开页面、推送断开重连期间的收发都不会漏。
	// 先订阅再取，与随后推送的报文可能重复，页面按序号去掉。
	if seq, err := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64); err == nil {
		if ps := s.packets.since(seq); len(ps) > 0 {
			if data, err := json.Marshal(ps); err != nil || !send("packets", data) {
				return
			}
		}
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case ev := <-ch:
			if !send(ev.name, ev.data) {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
