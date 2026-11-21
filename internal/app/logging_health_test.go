package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestHealthCooldownAndRuntimeDisabledProbe(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(503) }))
	defer up.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: up.URL})
	cfg.Health.Enabled = true
	cfg.Health.FailThreshold = 1
	_, store, events := testEngine(t, cfg)
	m := NewHealthMonitor(cfg, store, events, testLogger())
	m.probeAll(context.Background())
	m.probeAll(context.Background())
	if hits.Load() != 1 || store.Status("test", "primary") != StatusUnhealthy {
		t.Fatal("cooldown did not suppress immediate retry")
	}
	store.SetEnabled("test", "primary", false)
	m.probeAll(context.Background())
	if hits.Load() != 1 {
		t.Fatal("disabled upstream still probed")
	}
}

func TestNewLoggerUsesRotation(t *testing.T) {
	cfg := Default()
	cfg.Log.File = filepath.Join(t.TempDir(), "kernel.log")
	cfg.Log.MaxSizeBytes = 256
	cfg.Log.MaxBackups = 3
	logger, closer, err := NewLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		logger.Info("rotation integration", "iteration", i)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.Log.File))
	if err != nil || len(entries) != 4 {
		t.Fatalf("logger did not rotate: files %d err %v", len(entries), err)
	}
}
