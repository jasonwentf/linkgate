package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
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

	cancel context.CancelFunc
	done   chan struct{}
}

func NewHealthMonitor(cfg *Config, store *Store, events *EventLog, logger *slog.Logger) *HealthMonitor {
	return &HealthMonitor{cfg: cfg, store: store, events: events, log: logger, done: make(chan struct{})}
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
			if !u.IsEnabled() {
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

// ProbeNow 立即探测一条线路，供面板“测试”按钮使用。
func (m *HealthMonitor) ProbeNow(group, name string) (bool, time.Duration, string) {
	cfg := m.currentConfig()
	g := cfg.Group(group)
	if g == nil {
		return false, 0, "未知的线路组"
	}
	u := findUpstream(g, name)
	if u == nil {
		return false, 0, "未知的线路"
	}
	return m.probeOne(context.Background(), cfg, group, *u)
}

func (m *HealthMonitor) probeOne(ctx context.Context, cfg *Config, group string, u UpstreamConfig) (bool, time.Duration, string) {
	base, err := u.Base()
	if err != nil {
		m.store.RecordProbe(group, u.Name, false, 0, err.Error())
		return false, 0, err.Error()
	}
	path := u.HealthPath
	if path == "" {
		path = cfg.Health.ProbePath
	}
	if path == "" {
		path = "/health"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: path}
	req, err := http.NewRequestWithContext(ctx, cfg.Health.ProbeMethod, target.String(), nil)
	if err != nil {
		m.store.RecordProbe(group, u.Name, false, 0, err.Error())
		return false, 0, err.Error()
	}
	if u.Host != "" && !u.KeepClientHost {
		req.Host = u.Host
	}
	for k, v := range u.Headers {
		req.Header.Set(k, v)
	}

	timeout := cfg.Health.Timeout.Or(3 * time.Second)
	connectTO := u.ConnectTimeout.Or(cfg.HTTP.ConnectTimeout.Or(2 * time.Second))
	if connectTO > timeout {
		connectTO = timeout
	}
	dialer := &net.Dialer{Timeout: connectTO, KeepAlive: 15 * time.Second}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: u.TLSInsecureSkipVerify, ServerName: u.TLSServerName}, //nolint:gosec
			TLSHandshakeTimeout: timeout,
			DisableKeepAlives:   true,
		},
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		msg := err.Error()
		m.store.RecordProbe(group, u.Name, false, latency, msg)
		m.log.Debug("probe failed", "group", group, "upstream", u.Name, "err", err)
		return false, latency, msg
	}
	defer resp.Body.Close()
	// 逼出服务端真实响应：只拿到响应头不代表线路健康，
	// 读一个字节可以暴露“头 200 但正文卡住/被截断”的情况。
	_, readErr := resp.Body.Read(make([]byte, 1))
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		msg := "读取响应失败: " + readErr.Error()
		m.store.RecordProbe(group, u.Name, false, latency, msg)
		return false, latency, msg
	}

	ok := false
	if cfg.Health.ExpectStatus > 0 {
		ok = resp.StatusCode == cfg.Health.ExpectStatus
	} else {
		ok = resp.StatusCode >= 200 && resp.StatusCode < 400
	}
	prev := m.store.Status(group, u.Name)
	m.store.RecordProbe(group, u.Name, ok, latency, fmt.Sprintf("HTTP %d", resp.StatusCode))
	now := m.store.Status(group, u.Name)
	if now != prev {
		level := "info"
		kind := "health"
		if now == StatusUnhealthy {
			level = "warn"
		}
		m.events.Add(Event{Level: level, Kind: kind, Group: group, Upstream: u.Name,
			DurationMS: latency.Milliseconds(),
			Message:    fmt.Sprintf("线路状态 %s → %s（HTTP %d）", prev, now, resp.StatusCode)})
	}
	return ok, latency, ""
}
