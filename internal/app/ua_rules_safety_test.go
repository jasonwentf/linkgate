package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUAPinningRespectsRetryLimits(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("closed target reached") }))
	closed.Close()
	for _, tc := range []struct {
		name  string
		retry RetryConfig
	}{
		{"none", RetryConfig{Mode: "none", MaxAttempts: 3}},
		{"single-attempt", RetryConfig{Mode: "connect", MaxAttempts: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				io.WriteString(w, "backup")
			}))
			defer backup.Close()
			cfg := testConfig(
				UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(10)},
				UpstreamConfig{Name: "cf", URL: closed.URL, Priority: intPtr(20)},
			)
			cfg.Retry = tc.retry
			cfg.Groups[0].UARules = []UARuleConfig{{Match: "Chrome", Line: "cf"}}
			e, s, _ := testEngine(t, cfg)
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("synthetic"))
			req.Header.Set("User-Agent", "Chrome/131")
			rec := httptest.NewRecorder()
			e.Handler("test").ServeHTTP(rec, req)
			if rec.Code != http.StatusBadGateway || hits.Load() != 0 {
				t.Fatalf("pin bypassed retry limits: status=%d backup hits=%d", rec.Code, hits.Load())
			}
			if got := s.ServiceStats(s.Snapshot(cfg))[0]; got.Failover != 0 {
				t.Fatalf("no fallback occurred, but failover=%d", got.Failover)
			}
		})
	}
}

func TestUAPinningDoesNotFailoverOnHTTPError(t *testing.T) {
	var hits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "backup")
	}))
	defer backup.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "upstream-error")
	}))
	defer bad.Close()
	cfg := testConfig(
		UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(10)},
		UpstreamConfig{Name: "cf", URL: bad.URL, Priority: intPtr(20), FailureStatuses: []int{503}},
	)
	cfg.Groups[0].UARules = []UARuleConfig{{Match: "Chrome", Line: "cf"}}
	e, _, _ := testEngine(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("synthetic"))
	req.Header.Set("User-Agent", "Chrome/131")
	rec := httptest.NewRecorder()
	e.Handler("test").ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "upstream-error" || hits.Load() != 0 {
		t.Fatalf("pin retried a delivered HTTP error: status=%d body=%q backup hits=%d", rec.Code, rec.Body.String(), hits.Load())
	}
}
