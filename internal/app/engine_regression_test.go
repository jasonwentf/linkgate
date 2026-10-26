package app

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 回归测试：上游已经完整读完 POST 之后才出现 TLS 层错误时，绝不能换线路重发。
// 这是评审实测复现过的 P0：按错误类型判断会把 TLS 畸形记录误当成“连接没建立”。
func TestNoFailoverAfterRequestBodyDelivered(t *testing.T) {
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		io.WriteString(w, "backup")
	}))
	defer backup.Close()

	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // 先把请求体完整吃掉
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream does not support hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// 在已建立的 TLS 连接上直接写明文垃圾：客户端会拿到畸形 TLS record
		if tc, ok := conn.(*tls.Conn); ok {
			raw := tc.NetConn()
			_, _ = raw.Write([]byte("NOT-A-TLS-RECORD\r\n\r\n"))
		}
	}))
	defer bad.Close()

	cfg := testConfig(
		UpstreamConfig{Name: "bad", URL: bad.URL, TLSInsecureSkipVerify: true, Priority: 1},
		UpstreamConfig{Name: "backup", URL: backup.URL, Priority: 2},
	)
	engine, _, _ := testEngine(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "http://linkgate.test/v1/chat/completions",
		strings.NewReader(strings.Repeat("x", 1024)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.Handler("test").ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := backupHits.Load(); got != 0 {
		t.Fatalf("backup received %d requests; request must not be resent after delivery", got)
	}
}

// 回归测试：对端接受 TCP 但从不读取时，上传必须在 idle 超时后被打断，而不是永久挂住。
func TestUploadStallAbortsWithoutFailover(t *testing.T) {
	var mu sync.Mutex
	var conns []net.Conn
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c) // 接受但永不读取
			mu.Unlock()
		}
	}()

	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		io.WriteString(w, "backup")
	}))
	defer backup.Close()

	cfg := testConfig(
		UpstreamConfig{Name: "stalled", URL: "http://" + ln.Addr().String(), Priority: 1},
		UpstreamConfig{Name: "backup", URL: backup.URL, Priority: 2},
	)
	cfg.HTTP.UploadIdleTimeout = Duration{200 * time.Millisecond}
	cfg.HTTP.ResponseHeaderTimeout = Duration{30 * time.Second}
	engine, _, _ := testEngine(t, cfg)

	body := strings.Repeat("y", 8<<20) // 远大于内核 socket 缓冲
	req := httptest.NewRequest(http.MethodPost, "http://linkgate.test/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		engine.Handler("test").ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request hung past upload idle timeout")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := backupHits.Load(); got != 0 {
		t.Fatalf("backup received %d requests; stalled upload must not fail over", got)
	}
}

// 运行时停用的线路既不能参与选择，也不能因为“兜底”被再次选中。
func TestRuntimeDisabledUpstreamIsNeverSelected(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		io.WriteString(w, "primary")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		io.WriteString(w, "backup")
	}))
	defer backup.Close()

	cfg := testConfig(
		UpstreamConfig{Name: "primary", URL: primary.URL, Priority: 1},
		UpstreamConfig{Name: "backup", URL: backup.URL, Priority: 2},
	)
	engine, store, _ := testEngine(t, cfg)
	store.SetEnabled("test", "primary", false)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://linkgate.test/health", nil)
		engine.Handler("test").ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	if got := primaryHits.Load(); got != 0 {
		t.Fatalf("disabled upstream served %d requests", got)
	}
	if got := backupHits.Load(); got != 3 {
		t.Fatalf("backup served %d requests, want 3", got)
	}
}
