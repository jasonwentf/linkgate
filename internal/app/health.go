package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HealthMonitor 周期性地探测每条线路。
//
// 注意：主动探针只能回答“这条线路现在能不能完成一次 HTTP 往返”。
// 它无法覆盖“TCP 连着但对端不响应”的极端情况——那类问题靠真实请求的超时
// 和被动失败计数来兜底，所以 engine 里的失败回写同样会改变健康状态。
type HealthMonitor struct {
	mu     sync.RWMutex
	cfg    *Config
	store  *Store
	events *EventLog
	log    *slog.Logger

	gate   *sync.RWMutex
	cancel context.CancelFunc
	done   chan struct{}
}

func NewHealthMonitor(cfg *Config, store *Store, events *EventLog, logger *slog.Logger) *HealthMonitor {
	return &HealthMonitor{cfg: cfg, store: store, events: events, log: logger, done: make(chan struct{}), gate: &sync.RWMutex{}}
}

func (m *HealthMonitor) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	defer close(m.done)

	// 即使启动时健康检查是关闭的，也要保留调度循环：热加载打开后要能生效。
	m.probeAll(ctx)

	for {
		cfg := m.currentConfig()
		interval := cfg.Health.Interval.Or(15 * time.Second)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			m.probeAll(ctx)
		}
	}
}

func (m *HealthMonitor) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-m.done:
	case <-time.After(3 * time.Second):
	}
}

func (m *HealthMonitor) Update(cfg *Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

func (m *HealthMonitor) currentConfig() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *HealthMonitor) probeAll(ctx context.Context) {
	cfg := m.currentConfig()
	if !cfg.Health.Enabled {
		return
	}
	var wg sync.WaitGroup
	for gi := range cfg.Groups {
		g := &cfg.Groups[gi]
		for ui := range g.Upstreams {
			u := g.Upstreams[ui]
			if !u.IsEnabled() || !m.store.Enabled(g.Name, u.Name) {
				continue
			}
			// 已判定不可用的线路，按 cooldown 放慢探测，避免无谓流量。
			if m.store.Status(g.Name, u.Name) == StatusUnhealthy {
				if m.recentProbe(g.Name, u.Name, cfg.Health.Cooldown.Or(20*time.Second)) {
					continue
				}
			}
			wg.Add(1)
			go func(group string, up UpstreamConfig) {
				defer wg.Done()
				m.probeOne(ctx, cfg, group, up)
			}(g.Name, u)
		}
	}
	wg.Wait()
}

func (m *HealthMonitor) recentProbe(group, name string, within time.Duration) bool {
	m.gate.RLock()
	defer m.gate.RUnlock()
	snap := m.store.Snapshot(m.currentConfig())
	for _, g := range snap {
		if g.Name != group {
			continue
		}
		for _, u := range g.Upstreams {
			if u.Name == name {
				return !u.LastProbeAt.IsZero() && time.Since(u.LastProbeAt) < within
			}
		}
	}
	return false
}

// ProbeResult separates transport reachability from the configured health expectation.
// Class describes the HTTP response, independently of OK (expect_status).
type ProbeResult struct {
	OK         bool   `json:"ok"`
	Reach      bool   `json:"reach"`
	Class      string `json:"class"`
	HTTPStatus int    `json:"http_status"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error"`
	latency    time.Duration
}

func unreachableProbe(latency time.Duration, msg string) ProbeResult {
	return ProbeResult{Class: "unreachable", LatencyMS: latency.Milliseconds(), latency: latency, Error: redactURLText(msg)}
}

func responseClass(code int) string {
	switch {
	case code >= 200 && code < 400:
		return "healthy"
	case code == 401 || code == 403:
		return "auth"
	case code == 404:
		return "notfound"
	case code >= 500 && code < 600:
		return "server_error"
	default:
		return "other"
	}
}

// ProbeNow retains the original Go caller contract; the API uses the detailed result.
func (m *HealthMonitor) ProbeNow(group, name string) (bool, time.Duration, string) {
	r := m.ProbeNowResult(group, name)
	return r.OK, r.latency, r.Error
}

func (m *HealthMonitor) ProbeNowResult(group, name string) ProbeResult {
	cfg := m.currentConfig()
	g := cfg.Group(group)
	if g == nil {
		return unreachableProbe(0, "未知的线路组")
	}
	u := findUpstream(g, name)
	if u == nil {
		return unreachableProbe(0, "未知的线路")
	}
	return m.probeOne(context.Background(), cfg, group, *u)
}

// Probe dispatch is independent from the forwarding engine.
// HTTP tests application reachability; tcp_connect tests only a dial.
type healthProbe interface {
	Run(context.Context, *Config, *GroupConfig, UpstreamConfig, ProbeConfig) ProbeResult
}
type httpHealthProbe struct{}
type tcpConnectProbe struct{}

func effectiveProbe(cfg *Config, u UpstreamConfig) ProbeConfig {
	p := cfg.Health.Probe
	if p.Type == "" {
		p.Type = "http"
	}
	if p.Path == "" {
		p.Path = cfg.Health.ProbePath
	}
	if p.Method == "" {
		p.Method = cfg.Health.ProbeMethod
	}
	if p.Timeout.Duration == 0 {
		p.Timeout = cfg.Health.Timeout
	}
	if p.ExpectStatus == 0 {
		p.ExpectStatus = cfg.Health.ExpectStatus
	}
	if u.HealthPath != "" {
		p.Path = u.HealthPath
	}
	if u.Probe.Type != "" {
		p.Type = u.Probe.Type
	}
	if u.Probe.Path != "" {
		p.Path = u.Probe.Path
	}
	if u.Probe.Method != "" {
		p.Method = u.Probe.Method
	}
	if u.Probe.Timeout.Duration > 0 {
		p.Timeout = u.Probe.Timeout
	}
	if u.Probe.ExpectStatus != 0 {
		p.ExpectStatus = u.Probe.ExpectStatus
	}
	return p
}

func (m *HealthMonitor) probeOne(ctx context.Context, cfg *Config, group string, u UpstreamConfig) ProbeResult {
	p := effectiveProbe(cfg, u)
	var probe healthProbe = httpHealthProbe{}
	if p.Type == "tcp_connect" {
		probe = tcpConnectProbe{}
	}
	g := cfg.Group(group)
	if g == nil {
		return unreachableProbe(0, "未知线路组")
	}
	result := probe.Run(ctx, cfg, g, u, p)
	result.Error = redactURLText(result.Error)
	// A probe started before reload must not update a different generation.
	m.gate.RLock()
	defer m.gate.RUnlock()
	if cfg != m.currentConfig() {
		return result
	}
	prev := m.store.Status(group, u.Name)
	m.store.RecordProbeResult(group, u.Name, result)
	now := m.store.Status(group, u.Name)
	if prev != now {
		level := "info"
		if now == StatusUnhealthy {
			level = "warn"
		}
		m.events.Add(Event{Level: level, Kind: "health", Group: group, Upstream: u.Name, DurationMS: result.LatencyMS, Status: result.HTTPStatus,
			Message: fmt.Sprintf("线路状态 %s → %s（%s，%s）", prev, now, p.Type, result.Class)})
	}
	if !result.OK {
		m.log.Debug("probe unhealthy", "group", group, "upstream", u.Name, "reach", result.Reach, "class", result.Class, "error", result.Error)
	}
	return result
}

func (httpHealthProbe) Run(ctx context.Context, cfg *Config, g *GroupConfig, u UpstreamConfig, p ProbeConfig) ProbeResult {
	base, err := u.Base()
	if err != nil {
		return unreachableProbe(0, err.Error())
	}
	path := p.Path
	if path == "" {
		path = "/health"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: strings.TrimSuffix(base.Path, "/") + path}
	method := p.Method
	if method == "" {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return unreachableProbe(0, err.Error())
	}
	if u.Host != "" && !u.KeepClientHost {
		req.Host = u.Host
	}
	for k, v := range g.AddHeaders {
		req.Header.Set(k, v)
	}
	for k, v := range u.Headers {
		req.Header.Set(k, v)
	}
	h := cfg.EffectiveHTTP(g)
	timeout := p.Timeout.Or(3 * time.Second)
	connect := minDuration(u.ConnectTimeout.Or(h.ConnectTimeout.Or(2*time.Second)), timeout)
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: connect}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: u.TLSInsecureSkipVerify, ServerName: u.TLSServerName}, //nolint:gosec
		TLSHandshakeTimeout:   minDuration(h.TLSHandshakeTimeout.Or(5*time.Second), timeout),
		ResponseHeaderTimeout: minDuration(u.ResponseHeaderTimeout.Or(h.ResponseHeaderTimeout.Or(timeout)), timeout),
		DisableKeepAlives:     true, DisableCompression: compressionDisabled(h)}
	defer tr.CloseIdleConnections()
	// Client.Do parses Location before invoking CheckRedirect, so even
	// ErrUseLastResponse can lose a received 3xx response to a malformed URL.
	// One RoundTrip never follows or parses redirects; the context bounds it.
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	req = req.WithContext(ctx)
	start := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return unreachableProbe(time.Since(start), err.Error())
	}
	defer resp.Body.Close()
	// Response headers alone prove reachability. Reading a body would falsely
	// report unreachable when an otherwise valid HTTP endpoint streams or stalls.
	latency := time.Since(start)
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	if p.ExpectStatus > 0 {
		ok = resp.StatusCode == p.ExpectStatus
	}
	result := ProbeResult{OK: ok, Reach: true, Class: responseClass(resp.StatusCode), HTTPStatus: resp.StatusCode, LatencyMS: latency.Milliseconds(), latency: latency}
	if !ok {
		result.Error = fmt.Sprintf("HTTP %d（未满足健康期望）", resp.StatusCode)
	}
	return result
}

func (tcpConnectProbe) Run(ctx context.Context, cfg *Config, g *GroupConfig, u UpstreamConfig, p ProbeConfig) ProbeResult {
	base, err := u.Base()
	if err != nil {
		return unreachableProbe(0, err.Error())
	}
	port := base.Port()
	if port == "" {
		port = "80"
		if base.Scheme == "https" {
			port = "443"
		}
	}
	h := cfg.EffectiveHTTP(g)
	timeout := minDuration(p.Timeout.Or(3*time.Second), u.ConnectTimeout.Or(h.ConnectTimeout.Or(2*time.Second)))
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(base.Hostname(), port))
	latency := time.Since(start)
	if err != nil {
		return unreachableProbe(latency, err.Error())
	}
	_ = conn.Close()
	return ProbeResult{OK: true, Reach: true, Class: "healthy", LatencyMS: latency.Milliseconds(), latency: latency}
}
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
