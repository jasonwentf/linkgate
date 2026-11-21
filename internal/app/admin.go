package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"linkgate/internal/ui"
)

// AdminServer 提供本机管理接口与面板。
//
// 安全边界：只应监听回环地址；所有 /v1 接口都要求 X-Linkgate-Token，
// 写接口额外校验 Origin，避免浏览器里的恶意页面越权改配置。
type AdminServer struct {
	mu             sync.RWMutex
	cfg            *Config
	saved          *Config
	pendingRestart []string
	engine         *Engine
	store          *Store
	events         *EventLog
	manager        *ConfigManager
	gate           *sync.RWMutex
	cfgPath        string
	log            *slog.Logger
	token          string
	startedAt      time.Time
	version        string
	prober         Prober
}

// Prober 是即时探测能力（由 HealthMonitor 实现）。
type Prober interface {
	ProbeNowResult(group, name string) ProbeResult
}

func NewAdminServer(cfg *Config, engine *Engine, store *Store, events *EventLog,
	cfgPath string, logger *slog.Logger) *AdminServer {
	a := &AdminServer{
		cfg:       cfg,
		saved:     cfg,
		engine:    engine,
		store:     store,
		events:    events,
		gate:      &sync.RWMutex{},
		cfgPath:   cfgPath,
		log:       logger,
		startedAt: time.Now().Round(0),
		version:   Version,
	}
	a.token = loadOrCreateToken(cfg)
	return a
}

// SetVersion 让 main 把构建版本传给面板。
func (a *AdminServer) SetVersion(v string) {
	a.mu.Lock()
	a.version = v
	a.mu.Unlock()
}

// Token 返回管理令牌（托盘程序读取）。
func (a *AdminServer) Token() string { return a.token }

func loadOrCreateToken(cfg *Config) string {
	path := cfg.Admin.TokenFile
	if path == "" {
		path = filepath.Join(ExpandPath(cfg.DataDir), "admin.token")
	}
	path = ExpandPath(path)
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 16 {
			return s
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		_ = os.WriteFile(path, []byte(tok+"\n"), 0o600)
	}
	return tok
}

func (a *AdminServer) Update(cfg *Config) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

func (a *AdminServer) updateSaved(saved *Config, pending []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saved = saved
	a.pendingRestart = append([]string{}, pending...)
}

func (a *AdminServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/__linkgate/v1/health", a.handleHealth)
	mux.HandleFunc("/__linkgate/v1/status", a.auth(a.handleStatus))
	mux.HandleFunc("/__linkgate/v1/events", a.auth(a.handleEvents))
	mux.HandleFunc("/__linkgate/v1/settings", a.auth(a.handleSettings))
	mux.HandleFunc("/__linkgate/v1/stats", a.auth(a.handleStats))
	mux.HandleFunc("/__linkgate/v1/ui/order", a.auth(a.handleUIOrder))
	mux.HandleFunc("/__linkgate/v1/config", a.auth(a.handleConfig))
	mux.HandleFunc("/__linkgate/v1/groups", a.auth(a.handleGroups))
	mux.HandleFunc("/__linkgate/v1/groups/", a.auth(a.handleGroupUpstreams))
	mux.HandleFunc("/__linkgate/v1/probe", a.auth(a.handleProbe))
	mux.HandleFunc("/__linkgate/v1/toggle", a.auth(a.handleToggle))
	mux.HandleFunc("/__linkgate/v1/reload", a.auth(a.handleReload))
	mux.HandleFunc("/__linkgate/ui", a.handleUI)
	mux.HandleFunc("/", a.handleUI)
	return mux
}

func (a *AdminServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Linkgate-Token") != a.token && !a.cookieOK(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "缺少或错误的 X-Linkgate-Token"})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Origin") != "" {
			if !originMatchesHost(r.Header.Get("Origin"), r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "Origin 不被允许"})
				return
			}
		}
		next(w, r)
	}
}

// cookieOK 只在本机直接打开面板时生效：导航请求（Sec-Fetch-Site 为 none 或缺失）
// 会被种一个 SameSite=Strict 的 cookie，省去手工复制令牌。
func (a *AdminServer) cookieOK(r *http.Request) bool {
	c, err := r.Cookie("linkgate_token")
	if err != nil || c.Value != a.token {
		return false
	}
	if !isLoopback(r.RemoteAddr) {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "none", "same-origin":
		return true
	}
	return false
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originMatchesHost 按请求实际的 Host 判断同源，而不是按配置里的监听地址。
// 监听地址写成 :8766 时，用局域网 IP 打开面板也必须能用。
func originMatchesHost(origin, host string) bool {
	if origin == "" || host == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	oh, _, err1 := splitHostPort(u.Host)
	hh, _, err2 := splitHostPort(host)
	if err1 != nil || err2 != nil {
		return false
	}
	return isLoopbackHost(oh) && isLoopbackHost(hh)
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i <= 0 {
		return "", "", fmt.Errorf("bad addr %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}

func (a *AdminServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	ver := a.version
	a.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": ver,
		"uptime":  a.uptimeSeconds(),
	})
}

func (a *AdminServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
		return
	}
	a.gate.RLock()
	defer a.gate.RUnlock()
	a.mu.RLock()
	cfg := a.cfg
	ver := a.version
	pending := append([]string{}, a.pendingRestart...)
	a.mu.RUnlock()

	type status struct {
		Version        string       `json:"version"`
		StartedAt      time.Time    `json:"started_at"`
		UptimeSec      int64        `json:"uptime_seconds"`
		Config         string       `json:"config_path"`
		Groups         []GroupState `json:"groups"`
		LastSeq        uint64       `json:"last_seq"`
		UI             UIConfig     `json:"ui"`
		PendingRestart []string     `json:"pending_restart"`
	}
	groups := a.store.Snapshot(cfg)
	sortServiceStates(groups, cfg.UI)
	writeJSON(w, http.StatusOK, status{
		Version:        ver,
		StartedAt:      a.startedAt,
		UptimeSec:      a.uptimeSeconds(),
		Config:         a.cfgPath,
		Groups:         groups,
		UI:             cfg.UI,
		PendingRestart: pending,
		LastSeq:        a.events.LastSeq(),
	})
}

func (a *AdminServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": "只支持 GET"})
		return
	}
	after := parseUint(r.URL.Query().Get("after"))
	limit := int(parseUint(r.URL.Query().Get("limit")))
	stream := r.URL.Query().Get("stream")
	if stream != "" && stream != "state" && stream != "request" {
		writeJSON(w, 400, map[string]any{"error": "stream 只支持 state/request"})
		return
	}
	evs, last := a.events.QueryStream(r.URL.Query().Get("group"), stream, r.URL.Query().Get("kind"), after, limit)
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "last_seq": last})
}

func (a *AdminServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.gate.RLock()
		defer a.gate.RUnlock()
		b, err := os.ReadFile(ExpandPath(a.cfgPath))
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"path": a.cfgPath, "yaml": "", "error": redactURLText(err.Error())})
			return
		}
		// Forms use the accepted structured configuration; YAML remains the disk view.
		a.mu.RLock()
		groups := a.cfg.Groups
		a.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"path": a.cfgPath, "yaml": string(b), "groups": groups})
	case http.MethodPut, http.MethodPost:
		body, err := readConfigBody(r)
		if err != nil {
			writeJSON(w, apiErrorStatus(err), map[string]any{"error": redactURLText(err.Error())})
			return
		}
		if err := a.manager.Save(body); err != nil {
			writeJSON(w, apiErrorStatus(err), map[string]any{"error": redactURLText(err.Error())})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/PUT"})
	}
}

func (a *AdminServer) uptimeSeconds() int64 {
	n := int64(time.Now().Round(0).Sub(a.startedAt).Seconds())
	if n < 0 {
		return 0
	}
	return n
}

func (a *AdminServer) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	group := r.URL.Query().Get("group")
	name := r.URL.Query().Get("upstream")
	a.mu.RLock()
	p := a.prober
	a.mu.RUnlock()
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "探针未就绪"})
		return
	}
	a.gate.RLock()
	exists := a.exists(group, name)
	a.gate.RUnlock()
	if !exists {
		writeJSON(w, 404, map[string]any{"error": "没有这条线路"})
		return
	}
	result := p.ProbeNowResult(group, name)
	result.Error = redactURLText(result.Error)
	writeJSON(w, http.StatusOK, result)
}

func (a *AdminServer) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	group := r.URL.Query().Get("group")
	name := r.URL.Query().Get("upstream")
	a.gate.RLock()
	defer a.gate.RUnlock()
	if !a.exists(group, name) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "没有这条线路：" + group + "/" + name})
		return
	}
	enabled := r.URL.Query().Get("enabled") != "false"
	a.store.SetEnabled(group, name, enabled)
	a.events.Add(Event{Level: "info", Kind: "config", Group: group, Upstream: name,
		Message: fmt.Sprintf("线路运行时%s（重启后恢复配置文件设置）", map[bool]string{true: "启用", false: "停用"}[enabled])})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": enabled})
}

// SetProber 注册探针实现。
func (a *AdminServer) SetProber(p Prober) {
	a.mu.Lock()
	a.prober = p
	a.mu.Unlock()
}

func (a *AdminServer) exists(group, name string) bool {
	a.mu.RLock()
	cfg := a.cfg
	a.mu.RUnlock()
	g := cfg.Group(group)
	if g == nil {
		return false
	}
	return findUpstream(g, name) != nil
}

// handleReload 只重新读取配置文件并应用，不写盘，也不产生 .bak。
// 托盘上的「重新加载配置」走这个接口。
func (a *AdminServer) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	if err := a.manager.Reload(); err != nil {
		writeJSON(w, apiErrorStatus(err), map[string]any{"error": redactURLText(err.Error())})
		return
	}
	a.events.Add(Event{Level: "info", Kind: "config", Message: "重新读取配置文件并应用"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/__linkgate/ui" && r.URL.Path != "/__linkgate/ui/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if isLoopback(r.RemoteAddr) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "none":
			http.SetCookie(w, &http.Cookie{
				Name:     "linkgate_token",
				Value:    a.token,
				Path:     "/__linkgate/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
		}
	}
	b, err := ui.IndexHTML()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func parseUint(s string) uint64 {
	var v uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return v
		}
		v = v*10 + uint64(c-'0')
	}
	return v
}
