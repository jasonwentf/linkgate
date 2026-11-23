package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchemaStrictPriorityProtocolAndExample(t *testing.T) {
	const raw = `version: 1
admin:
  disabled: true
groups:
  - name: test
    listen: 127.0.0.1:19000
    upstreams:
      - name: zero
        url: http://example.invalid
        priority: 0
        kind: custom-route
      - name: omitted
        url: http://example.invalid
`
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Groups[0].Protocol != "http" || cfg.Groups[0].Upstreams[0].PriorityValue() != 0 || cfg.Groups[0].Upstreams[1].PriorityValue() != 20 {
		t.Fatal("schema defaults")
	}
	for _, input := range []string{
		strings.Replace(raw, "priority: 0", "priorty: 0", 1),
		strings.Replace(raw, "kind: custom-route", "probe:\n          typo: http", 1),
		raw + "---\nversion: 1\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatal("unknown fields/multiple docs accepted")
		}
	}
	tcp := strings.Replace(raw, "name: test", "name: test\n    protocol: tcp", 1)
	if _, err := Parse([]byte(tcp)); err == nil || !strings.Contains(err.Error(), "暂未支持") {
		t.Fatalf("tcp error %v", err)
	}
	b, err := os.ReadFile("../../configs/linkgate.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	example, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	cf := findUpstream(&example.Groups[0], "cloudflare")
	if cf == nil || cf.MaxBodyBytes <= 0 || len(cf.FailureStatuses) != 1 || cf.FailureStatuses[0] != 524 {
		t.Fatal("CF guard missing")
	}
}

func TestProbePrefixHeadersAndGroupTimeout(t *testing.T) {
	paths := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		if r.Header.Get("X-Group") != "yes" {
			t.Error("probe missing group headers")
		}
		select {
		case <-time.After(100 * time.Millisecond):
			io.WriteString(w, "ready")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: upstream.URL + "/base/", HealthPath: "/ready"})
	cfg.Groups[0].AddHeaders = map[string]string{"X-Group": "yes"}
	cfg.Groups[0].TimeoutOverrides.ResponseHeaderTimeout = Duration{20 * time.Millisecond}
	_, store, events := testEngine(t, cfg)
	m := NewHealthMonitor(cfg, store, events, testLogger())
	ok, latency, msg := m.ProbeNow("test", "primary")
	if ok || latency > time.Second || !strings.Contains(msg, "timeout awaiting response headers") {
		t.Fatalf("group timeout not applied: %v %s %s", ok, latency, msg)
	}
	if path := <-paths; path != "/base/ready" {
		t.Fatalf("probe path = %s", path)
	}
	// Upstream timeout takes precedence over group.
	cfg.Groups[0].Upstreams[0].ResponseHeaderTimeout = Duration{time.Second}
	ok, _, msg = m.ProbeNow("test", "primary")
	if !ok {
		t.Fatal(msg)
	}
}

func TestTCPConnectProbeNoHTTPRequired(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
		close(done)
	}()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://" + ln.Addr().String() + "/ignored", Probe: ProbeConfig{Type: "tcp_connect"}})
	_, store, events := testEngine(t, cfg)
	m := NewHealthMonitor(cfg, store, events, testLogger())
	if ok, _, msg := m.ProbeNow("test", "primary"); !ok {
		t.Fatal(msg)
	}
	waitSignal(t, done)
	if store.Status("test", "primary") != StatusHealthy {
		t.Fatal("TCP probe not recorded")
	}
}

func TestClientCancelBeforeHeadersDoesNotFailLine(t *testing.T) {
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer upstream.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: upstream.URL})
	e, store, events := testEngine(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { e.Handler("test").ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	waitSignal(t, entered)
	cancel()
	waitSignal(t, done)
	if testState(t, store, cfg, "primary").RequestsFailed != 0 {
		t.Fatal("client cancel marked failure")
	}
	for _, event := range events.Recent(0) {
		if event.Level == "error" {
			t.Fatal("client cancel logged ERROR")
		}
	}
}

func TestCompressionAndHeaderWarningWired(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: upstream.URL})
	cfg.HTTP.DisableCompression = boolPtr(false)
	cfg.HTTP.ResponseHeaderWarnAfter = Duration{10 * time.Millisecond}
	e, store, events := testEngine(t, cfg)
	if e.group("test").upstreams[0].transport.DisableCompression {
		t.Fatal("compression field ignored")
	}
	e.Handler("test").ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	warn := false
	for _, event := range events.Recent(0) {
		if event.Level == "warn" && strings.Contains(event.Message, "仍未收到响应头") {
			warn = true
		}
	}
	if !warn || testState(t, store, cfg, "primary").RequestsOK != 1 {
		t.Fatal("warning missing or changed success")
	}
}

func TestGlobalConcurrencySurvivesReload(t *testing.T) {
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		active.Add(-1)
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: upstream.URL})
	cfg.Buffer.GlobalMaxConcurrent = 1
	cfg.Groups = append(cfg.Groups, GroupConfig{Name: "second", Listen: "127.0.0.1:19001", Upstreams: cfg.Groups[0].Upstreams})
	e, _, _ := testEngine(t, cfg)
	done := make(chan struct{}, 2)
	go func() {
		e.Handler("test").ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		done <- struct{}{}
	}()
	waitSignal(t, entered)
	if err := e.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	go func() {
		e.Handler("second").ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		done <- struct{}{}
	}()
	select {
	case <-entered:
		t.Fatal("global limit bypassed by reload/new group")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		waitSignal(t, done)
	}
	if maxActive.Load() != 1 {
		t.Fatal("concurrent budget exceeded")
	}
}

func TestLogRotationBoundedAndEventResize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.log")
	w, err := newRotatingWriter(path, 20, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := w.Write([]byte("1234567890\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("log files %d want 4", len(entries))
	}
	for _, e := range entries {
		info, _ := e.Info()
		if info.Size() > 20 {
			t.Fatal("rotation bounds")
		}
		// Windows 的 FileMode 不表示 ACL，不能用 POSIX 权限位验隐私。
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatal("rotation permissions")
		}
	}
	l := NewEventLog(4)
	for i := 0; i < 6; i++ {
		l.Add(Event{Message: "test"})
	}
	l.Resize(2)
	recent := l.Recent(0)
	if len(recent) != 2 || recent[0].Seq != 5 || l.LastSeq() != 6 {
		t.Fatal("resize lost sequence")
	}
}
