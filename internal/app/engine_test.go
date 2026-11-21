package app

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(upstreams ...UpstreamConfig) *Config {
	cfg := Default()
	cfg.Health.Enabled = false
	cfg.Retry.Backoff = Duration{}
	cfg.HTTP.ResponseHeaderTimeout = Duration{2 * time.Second}
	cfg.Groups = []GroupConfig{{Name: "test", Listen: "127.0.0.1:18765", Upstreams: upstreams}}
	return cfg
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testEngine(t *testing.T, cfg *Config) (*Engine, *Store, *EventLog) {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(cfg, Thresholds{
		FailThreshold: cfg.Health.FailThreshold, RecoverThreshold: cfg.Health.RecoverThreshold,
	})
	events := NewEventLog(100)
	engine, err := NewEngine(cfg, store, events, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	return engine, store, events
}

func testState(t *testing.T, store *Store, cfg *Config, name string) UpstreamState {
	t.Helper()
	for _, group := range store.Snapshot(cfg) {
		for _, up := range group.Upstreams {
			if up.Name == name {
				return up
			}
		}
	}
	t.Fatalf("missing upstream %q", name)
	return UpstreamState{}
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for handler")
	}
}

func TestEngineDialFailureFailsOver(t *testing.T) {
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		io.WriteString(w, "backup response")
	}))
	defer backup.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("closed upstream unexpectedly reached")
	}))
	closed.Close()

	cfg := testConfig(
		UpstreamConfig{Name: "primary", URL: closed.URL, Priority: intPtr(10)},
		UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(20)},
	)
	engine, store, events := testEngine(t, cfg)
	recorder := httptest.NewRecorder()
	engine.Handler("test").ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("request body")))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "backup response" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if backupHits.Load() != 1 {
		t.Fatalf("backup hits = %d, want 1", backupHits.Load())
	}
	primaryState := testState(t, store, cfg, "primary")
	backupState := testState(t, store, cfg, "backup")
	if primaryState.RequestsFailed != 1 || primaryState.FailoversOut != 1 || primaryState.RequestsOK != 0 {
		t.Errorf("primary counters = %+v", primaryState)
	}
	if backupState.RequestsOK != 1 || backupState.RequestsFailed != 0 || backupState.SelectedTotal != 1 || backupState.SelectedAsBackup != 1 {
		t.Errorf("backup counters = %+v", backupState)
	}
	var failovers int
	for _, event := range events.Recent(0) {
		if event.Kind == "failover" {
			failovers++
			if event.Group != "test" || event.From != "primary" || event.To != "backup" {
				t.Errorf("failover event = %+v", event)
			}
		}
	}
	if failovers != 1 {
		t.Errorf("failover events = %d, want 1", failovers)
	}
}

func TestEngineResponseHeaderTimeoutDoesNotRetry(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		// Hold the connection open without sending headers until the test finishes.
		<-release
	}))
	defer primary.Close()
	defer close(release)
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backup.Close()
	cfg := testConfig(
		UpstreamConfig{Name: "primary", URL: primary.URL, Priority: intPtr(10)},
		UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(20)},
	)
	cfg.HTTP.ResponseHeaderTimeout = Duration{200 * time.Millisecond}
	engine, store, events := testEngine(t, cfg)
	recorder := httptest.NewRecorder()
	engine.Handler("test").ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("do not replay")))
	waitSignal(t, entered)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
	if backupHits.Load() != 0 {
		t.Errorf("backup hits = %d, request was replayed", backupHits.Load())
	}
	state := testState(t, store, cfg, "primary")
	if state.RequestsFailed != 1 || state.FailoversOut != 0 || !strings.Contains(state.LastRequestError, "timeout awaiting response headers") {
		t.Errorf("primary state = %+v", state)
	}
	for _, event := range events.Recent(0) {
		if event.Kind == "failover" {
			t.Errorf("unexpected failover: %+v", event)
		}
	}
}

func TestEngineRequestBodySpillsAndCleansUp(t *testing.T) {
	tempDir := t.TempDir()
	payload := bytes.Repeat([]byte{0x00, 0xff, 'a', '\n', '\r', 0x80, 'z', 0x7f}, (10<<20)/8)
	type received struct {
		body     []byte
		err      error
		files    int
		fileSize int64
		length   int64
	}
	result := make(chan received, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		observation := received{body: data, err: err, length: r.ContentLength}
		entries, dirErr := os.ReadDir(tempDir)
		if observation.err == nil {
			observation.err = dirErr
		}
		observation.files = len(entries)
		if len(entries) == 1 {
			info, statErr := entries[0].Info()
			if statErr == nil {
				observation.fileSize = info.Size()
			} else {
				observation.err = statErr
			}
		}
		result <- observation
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: upstream.URL})
	cfg.Buffer.TempDir = tempDir
	cfg.Buffer.SpillThreshold = 1024
	engine, _, _ := testEngine(t, cfg)
	recorder := httptest.NewRecorder()
	engine.Handler("test").ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(payload)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if !bytes.Equal(got.body, payload) {
		t.Errorf("upstream body differs: got %d bytes, want %d", len(got.body), len(payload))
	}
	if got.files != 1 || got.fileSize != int64(len(payload)) || got.length != int64(len(payload)) {
		t.Errorf("spill observation: files=%d size=%d content-length=%d", got.files, got.fileSize, got.length)
	}
	assertTempDirEmpty(t, tempDir)
}

func assertTempDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary body files leaked: %d", len(entries))
	}
}

func TestEngineRejectsOversizedBody(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()
	for _, unknownLength := range []bool{false, true} {
		name := "known length"
		if unknownLength {
			name = "unknown length"
		}
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(UpstreamConfig{Name: "primary", URL: upstream.URL})
			cfg.Buffer.TempDir = t.TempDir()
			cfg.Buffer.SpillThreshold = 1024
			cfg.Buffer.MaxRequestBytes = 128 << 10
			engine, store, _ := testEngine(t, cfg)
			req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(bytes.Repeat([]byte("x"), int(cfg.Buffer.MaxRequestBytes)+1)))
			if unknownLength {
				req.ContentLength = -1
			}
			recorder := httptest.NewRecorder()
			engine.Handler("test").ServeHTTP(recorder, req)
			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d, want 413", recorder.Code)
			}
			if hits.Load() != 0 || testState(t, store, cfg, "primary").RequestTotal() != 0 {
				t.Error("oversized request reached an upstream")
			}
			assertTempDirEmpty(t, cfg.Buffer.TempDir)
		})
	}
}

func TestEngineSSEFlushesBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		for _, chunk := range []string{"data: second\n\n", "data: third\n\n"} {
			io.WriteString(w, chunk)
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	defer unblock()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: upstream.URL})
	engine, _, _ := testEngine(t, cfg)
	proxyDone := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(proxyDone)
		engine.Handler("test").ServeHTTP(w, r)
	}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE response = %d %v", resp.StatusCode, resp.Header)
	}
	reader := bufio.NewReader(resp.Body)
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(reader, first); err != nil {
		t.Fatalf("first chunk did not arrive while upstream was blocked: %v", err)
	}
	if string(first) != "data: first\n\n" {
		t.Fatalf("first chunk = %q", first)
	}
	select {
	case <-upstreamDone:
		t.Fatal("upstream completed before the first chunk was read")
	default:
	}
	unblock()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "data: second\n\ndata: third\n\n" {
		t.Errorf("remaining chunks = %q", rest)
	}
	waitSignal(t, proxyDone)
	waitSignal(t, upstreamDone)
}

func TestEngineForwardsHeadersAndHost(t *testing.T) {
	for _, tc := range []struct {
		name           string
		host           string
		keepClientHost bool
	}{
		{name: "default"},
		{name: "explicit host", host: "upstream.example"},
		{name: "client host takes precedence", host: "ignored.example", keepClientHost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type received struct {
				host   string
				header http.Header
			}
			observed := make(chan received, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- received{r.Host, r.Header.Clone()}
				w.Header().Set("Connection", "X-Response-Hop")
				w.Header().Set("X-Response-Hop", "remove me")
				w.Header().Set("X-Response-Custom", "keep me")
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			cfg := testConfig(UpstreamConfig{
				Name: "primary", URL: upstream.URL, Host: tc.host, KeepClientHost: tc.keepClientHost,
				Headers: map[string]string{"CF-Access-Client-Id": "test-service-id", "CF-Access-Client-Secret": "test-service-secret"},
			})
			cfg.Groups[0].AddHeaders = map[string]string{"CF-Access-Client-Id": "group-id", "X-Group-Header": "group value"}
			engine, _, _ := testEngine(t, cfg)
			req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
			req.Host = "client.example"
			req.Header.Set("Authorization", "Bearer test-only")
			req.Header["X-Custom"] = []string{"opaque value", "second value"}
			req.Header.Set("CF-Access-Client-Id", "client-id")
			req.Header.Set("CF-Access-Client-Secret", "client-secret")
			req.Header["Connection"] = []string{"keep-alive, X-Request-Hop", "X-Second-Hop"}
			req.Header.Set("X-Request-Hop", "remove me")
			req.Header.Set("X-Second-Hop", "remove me too")
			hopHeaders := []string{
				"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
				"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
			}
			for _, name := range hopHeaders {
				if name != "Connection" {
					req.Header.Set(name, "remove me")
				}
			}
			originalHeaders := req.Header.Clone()
			recorder := httptest.NewRecorder()
			engine.Handler("test").ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			got := <-observed
			wantHost := strings.TrimPrefix(upstream.URL, "http://")
			if tc.host != "" {
				wantHost = tc.host
			}
			if tc.keepClientHost {
				wantHost = req.Host
			}
			if got.host != wantHost {
				t.Errorf("Host = %q, want %q", got.host, wantHost)
			}
			if !reflect.DeepEqual(got.header.Values("X-Custom"), originalHeaders.Values("X-Custom")) || got.header.Get("Authorization") != "Bearer test-only" {
				t.Errorf("business headers changed: %v", got.header)
			}
			for _, name := range hopHeaders {
				if got.header.Get(name) != "" {
					t.Errorf("hop-by-hop header %s reached upstream", name)
				}
			}
			for _, name := range []string{"X-Request-Hop", "X-Second-Hop"} {
				if got.header.Get(name) != "" {
					t.Errorf("Connection-nominated header %s reached upstream", name)
				}
			}
			if got.header.Get("CF-Access-Client-Id") != "test-service-id" || got.header.Get("CF-Access-Client-Secret") != "test-service-secret" || got.header.Get("X-Group-Header") != "group value" {
				t.Errorf("configured header overrides not applied: %v", got.header)
			}
			if recorder.Header().Get("Connection") != "" || recorder.Header().Get("X-Response-Hop") != "" || recorder.Header().Get("X-Response-Custom") != "keep me" {
				t.Errorf("response headers = %v", recorder.Header())
			}
			if !reflect.DeepEqual(req.Header, originalHeaders) {
				t.Error("client headers mutated in place")
			}
		})
	}
}

func TestEngine5xxDoesNotFailOverOrMarkUnhealthy(t *testing.T) {
	var backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "backend unavailable")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backupHits.Add(1) }))
	defer backup.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: primary.URL, Priority: intPtr(10)}, UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(20)})
	engine, store, events := testEngine(t, cfg)
	for i := 0; i < cfg.Health.FailThreshold+1; i++ {
		recorder := httptest.NewRecorder()
		engine.Handler("test").ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != "backend unavailable" {
			t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
		}
	}
	state := testState(t, store, cfg, "primary")
	if state.Status == StatusUnhealthy || state.ConsecutiveFailures != 0 || state.FailoversOut != 0 {
		t.Errorf("HTTP 5xx counted as a line failure: %+v", state)
	}
	// 5xx 仍是一次失败的请求（面板要显示出来），但不影响线路健康。
	if state.RequestsFailed != uint64(cfg.Health.FailThreshold+1) || state.RequestsOK != 0 {
		t.Errorf("HTTP 5xx request counters = ok %d / failed %d", state.RequestsOK, state.RequestsFailed)
	}
	if backupHits.Load() != 0 {
		t.Errorf("backup hits = %d", backupHits.Load())
	}
	for _, event := range events.Recent(0) {
		if event.Kind == "failover" {
			t.Errorf("unexpected failover: %+v", event)
		}
	}
}

func TestEngineUnhealthyPrimarySelectsBackup(t *testing.T) {
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { primaryHits.Add(1) }))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "backup") }))
	defer backup.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: primary.URL, Priority: intPtr(10)}, UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(20)})
	engine, store, _ := testEngine(t, cfg)
	for i := 0; i < cfg.Health.FailThreshold; i++ {
		store.RecordFailure(store.Config(), "test", "primary", "dial failed", 0)
	}
	recorder := httptest.NewRecorder()
	engine.Handler("test").ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "backup" || primaryHits.Load() != 0 {
		t.Fatalf("response = %d %q, primary hits = %d", recorder.Code, recorder.Body.String(), primaryHits.Load())
	}
	state := testState(t, store, cfg, "backup")
	if state.SelectedTotal != 1 || state.SelectedAsBackup != 1 {
		t.Errorf("backup selection counters = %+v", state)
	}
}

func TestEngineNoFailoverWhenNoFurtherAttempt(t *testing.T) {
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backupHits.Add(1) }))
	defer backup.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	for _, tc := range []struct {
		name        string
		mode        string
		maxAttempts int
		withBackup  bool
	}{
		{name: "only upstream", mode: "connect", maxAttempts: 3},
		{name: "retry disabled", mode: "none", maxAttempts: 3, withBackup: true},
		{name: "attempt limit", mode: "connect", maxAttempts: 1, withBackup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(UpstreamConfig{Name: "primary", URL: closed.URL, Priority: intPtr(10)})
			if tc.withBackup {
				cfg.Groups[0].Upstreams = append(cfg.Groups[0].Upstreams, UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(20)})
			}
			cfg.Retry.Mode = tc.mode
			cfg.Retry.MaxAttempts = tc.maxAttempts
			engine, store, events := testEngine(t, cfg)
			recorder := httptest.NewRecorder()
			engine.Handler("test").ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", recorder.Code)
			}
			if backupHits.Load() != 0 {
				t.Errorf("backup hits = %d, want 0", backupHits.Load())
			}
			state := testState(t, store, cfg, "primary")
			if state.RequestsFailed != 1 || state.FailoversOut != 0 {
				t.Errorf("no switch occurred, counters = %+v", state)
			}
			for _, event := range events.Recent(0) {
				if event.Kind == "failover" {
					t.Errorf("no switch occurred, event = %+v", event)
				}
			}
		})
	}
}
