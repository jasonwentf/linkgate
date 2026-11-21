package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminGroupAddressSwapIsAtomic(t *testing.T) {
	m, _, cfg := managerFixture(t)
	adminAddr, groupAddr := cfg.Admin.Listen, cfg.Groups[0].Listen
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "swapped") }))
	defer up.Close()
	if err := m.Mutate(func(c *Config) error {
		c.Admin.Listen = groupAddr
		c.Groups[0].Listen = adminAddr
		c.Groups[0].Upstreams[0].URL = up.URL
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ addr, path, want string }{
		{groupAddr, "/__linkgate/v1/health", `"ok":true`},
		{adminAddr, "/health", "swapped"},
	} {
		resp, err := http.Get("http://" + tc.addr + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("swap HTTP %d: %s", resp.StatusCode, b)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Fatalf("addr %s path %s served %q, want %q", tc.addr, tc.path, b, tc.want)
		}
	}
	l := m.listeners.active[adminAddr].handler.(*HTTPListener)
	if l.server.WriteTimeout != 0 || l.target.Load().admin {
		t.Fatal("proxy inherited admin write timeout")
	}
}
