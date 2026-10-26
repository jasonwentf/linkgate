package app

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHealthMonitorProbeThresholdsAndOverrides(t *testing.T) {
	var responseStatus atomic.Int32
	responseStatus.Store(http.StatusNotFound)
	type request struct{ method, path, host, accessID string }
	requests := make(chan request, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- request{r.Method, r.URL.Path, r.Host, r.Header.Get("CF-Access-Client-Id")}
		w.WriteHeader(int(responseStatus.Load()))
	}))
	defer upstream.Close()
	cfg := testConfig(UpstreamConfig{
		Name: "primary", URL: upstream.URL, Host: "probe.example", HealthPath: "/ready",
		Headers: map[string]string{"CF-Access-Client-Id": "test-probe-id"},
	})
	cfg.Health.ProbeMethod = http.MethodHead
	cfg.Health.FailThreshold = 3
	cfg.Health.RecoverThreshold = 2
	_, store, events := testEngine(t, cfg)
	monitor := NewHealthMonitor(cfg, store, events, testLogger())
	probe := func(wantOK bool, wantStatus HealthStatus) {
		t.Helper()
		ok, _, msg := monitor.ProbeNow("test", "primary")
		if ok != wantOK {
			t.Fatalf("probe ok = %v, want %v (message %q)", ok, wantOK, msg)
		}
		if status := store.Status("test", "primary"); status != wantStatus {
			t.Fatalf("probe state = %s, want %s", status, wantStatus)
		}
		got := <-requests
		if got.method != http.MethodHead || got.path != "/ready" || got.host != "probe.example" || got.accessID != "test-probe-id" {
			t.Errorf("probe request = %+v", got)
		}
	}
	probe(false, StatusUnknown)
	probe(false, StatusUnknown)
	probe(false, StatusUnhealthy)
	responseStatus.Store(http.StatusNoContent)
	probe(true, StatusUnhealthy)
	probe(true, StatusHealthy)
	state := testState(t, store, cfg, "primary")
	if state.RequestTotal() != 0 || state.LastProbeAt.IsZero() || state.LastProbeError != "" || state.ProbeSuccesses != 2 || state.ConsecutiveFailures != 0 {
		t.Errorf("probe state = %+v", state)
	}
	healthEvents := 0
	for _, event := range events.Recent(0) {
		if event.Kind == "health" {
			healthEvents++
		}
	}
	if healthEvents != 2 {
		t.Errorf("health transition events = %d, want 2", healthEvents)
	}
}
