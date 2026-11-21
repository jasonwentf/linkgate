package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"linkgate/internal/app"
)

func TestVersionDefaultAndLdflagsInjection(t *testing.T) {
	if app.Version != "0.2.1" || version != app.Version {
		t.Fatalf("default main=%s app=%s", version, app.Version)
	}
	bin := filepath.Join(t.TempDir(), "linkgate")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version=9.8.7 -X linkgate/internal/app.Version=9.8.7", "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	output, err := exec.Command(bin, "-version").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "linkgate 9.8.7 ") {
		t.Fatalf("version: %v %s", err, output)
	}
	cfg := app.Default()
	cfg.DataDir = t.TempDir()
	cfg.Admin.TokenFile = filepath.Join(cfg.DataDir, "admin.token")
	if err := os.WriteFile(cfg.Admin.TokenFile, []byte("test-only-version-token"), 0600); err != nil {
		t.Fatal(err)
	}
	admin := app.NewAdminServer(cfg, nil, app.NewStore(cfg, app.Thresholds{FailThreshold: 1, RecoverThreshold: 1}), app.NewEventLog(10), "isolated.yaml", nil)
	admin.SetVersion("9.8.7")
	r := httptest.NewRequest(http.MethodGet, "/__linkgate/v1/status", nil)
	r.Header.Set("X-Linkgate-Token", admin.Token())
	w := httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, r)
	var st struct {
		Version string `json:"version"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.Version != "9.8.7" {
		t.Fatal(w.Body.String())
	}
}
