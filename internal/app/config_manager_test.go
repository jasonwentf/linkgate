package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func managerFixture(t *testing.T) (*ConfigManager, *AdminServer, *Config) {
	t.Helper()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://upstream.invalid"})
	cfg.DataDir = t.TempDir()
	cfg.Admin.Listen = freeAddress(t)
	cfg.Groups[0].Listen = freeAddress(t)
	cfg.applyDefaults()
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.DataDir, "config.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	e, store, events := testEngine(t, cfg)
	monitor := NewHealthMonitor(cfg, store, events, testLogger())
	m, err := NewConfigManager(path, cfg, e, store, monitor, events, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	a := NewAdminServer(cfg, e, store, events, path, testLogger())
	a.SetProber(monitor)
	m.SetAdmin(a)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, a, cfg
}

func adminRequest(a *AdminServer, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Linkgate-Token", a.Token())
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func expectCode(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("HTTP %d, want %d: %s", w.Code, want, w.Body.String())
	}
}
func assertPort(t *testing.T, addr string, listening bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if conn != nil {
		conn.Close()
	}
	if (err == nil) != listening {
		t.Fatalf("port %s listening=%v want %v (err %v)", addr, err == nil, listening, err)
	}
	if !listening {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("port not released: %v", err)
		}
		ln.Close()
	}
}

func TestReloadUpdatesAdminStoreEngineAndListeners(t *testing.T) {
	m, a, cfg := managerFixture(t)
	oldAddr := cfg.Groups[0].Listen
	raw, _ := cfg.Marshal()
	next, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	next.Groups[0].Listen = freeAddress(t)
	next.Groups[0].Upstreams = append(next.Groups[0].Upstreams, UpstreamConfig{Name: "added", URL: "http://added.invalid"})
	raw, _ = next.Marshal()
	if err := os.WriteFile(m.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	assertPort(t, oldAddr, false)
	assertPort(t, next.Groups[0].Listen, true)
	if a.cfg != m.cfg || m.engine.Config() != m.cfg || m.monitor.currentConfig() != m.cfg {
		t.Fatal("config pointers diverged")
	}
	expectCode(t, adminRequest(a, "POST", "/__linkgate/v1/toggle?group=test&upstream=added&enabled=false", ""), 200)
	status := adminRequest(a, "GET", "/__linkgate/v1/status", "")
	if !strings.Contains(status.Body.String(), `"name":"added"`) {
		t.Fatal(status.Body.String())
	}
	if _, err := os.Stat(m.path + ".bak"); !os.IsNotExist(err) {
		t.Fatal("reload created backup")
	}
}

func TestCRUDGroupsAndUpstreamsListenerLifecycle(t *testing.T) {
	m, a, _ := managerFixture(t)
	path := "/__linkgate/v1/groups"
	addr, newAddr := freeAddress(t), freeAddress(t)
	expectCode(t, adminRequest(a, "POST", path, fmt.Sprintf(`{"name":"new","listen":%q,"protocol":"http","comment":"service"}`, addr)), 201)
	assertPort(t, addr, true)
	upPath := path + "/new/upstreams"
	expectCode(t, adminRequest(a, "POST", upPath, `{"name":"direct","url":"http://example.invalid/base","priority":0,"enabled":false,"kind":"zt","comment":"route","host":"example.invalid","headers":{"X-Test":"test"},"max_body_bytes":100,"failure_statuses":[524],"probe":{"type":"tcp_connect","timeout":"50ms"},"connect_timeout":"1s","response_header_timeout":"2s"}`), 201)
	expectCode(t, adminRequest(a, "PUT", upPath, `{"name":"direct","enabled":true,"comment":"changed"}`), 200)
	u := findUpstream(m.cfg.Group("new"), "direct")
	if u.PriorityValue() != 0 || !u.IsEnabled() || u.Kind != "zt" || u.MaxBodyBytes != 100 || u.Comment != "changed" {
		t.Fatalf("patch lost fields: %+v", u)
	}
	expectCode(t, adminRequest(a, "PUT", path, fmt.Sprintf(`{"name":"new","listen":%q,"add_headers":{"X-Group":"yes"},"timeout_overrides":{"response_header_timeout":"3s"}}`, newAddr)), 200)
	assertPort(t, addr, false)
	assertPort(t, newAddr, true)
	expectCode(t, adminRequest(a, "DELETE", upPath, `{"name":"direct"}`), 200)
	if len(m.cfg.Group("new").Upstreams) != 0 {
		t.Fatal("upstream not deleted")
	}
	expectCode(t, adminRequest(a, "DELETE", path, `{"name":"new"}`), 200)
	assertPort(t, newAddr, false)
	bak, err := os.ReadFile(m.path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(bak, []byte("name: new")) {
		t.Fatal("backup missing prior generation")
	}
	for _, f := range []string{m.path, m.path + ".bak"} {
		info, _ := os.Stat(f)
		if info.Mode().Perm() != 0o600 {
			t.Fatal("config not private")
		}
	}
	// Last upstream and last group may be deleted; an empty group returns 503.
	expectCode(t, adminRequest(a, "DELETE", path, `{"name":"test"}`), 200)
	if len(m.cfg.Groups) != 0 {
		t.Fatal("last group not removed")
	}
}

func TestFailedApplyRollsBackAllResources(t *testing.T) {
	m, a, cfg := managerFixture(t)
	oldRaw, _ := os.ReadFile(m.path)
	bakPath := m.path + ".bak"
	os.WriteFile(bakPath, []byte("previous backup"), 0o600)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	newAddr := freeAddress(t)
	// The first socket is prepared successfully; the second fails. Neither may leak.
	raw, _ := cfg.Marshal()
	next, _ := Parse(raw)
	next.Groups = append(next.Groups, GroupConfig{Name: "prepared", Listen: newAddr}, GroupConfig{Name: "conflict", Listen: occupied.Addr().String()})
	raw, _ = next.Marshal()
	w := adminRequest(a, "PUT", "/__linkgate/v1/config", string(raw))
	expectCode(t, w, 409)
	current, _ := os.ReadFile(m.path)
	backup, _ := os.ReadFile(bakPath)
	if !bytes.Equal(current, oldRaw) || string(backup) != "previous backup" {
		t.Fatal("failed preflight modified disk")
	}
	if m.cfg != cfg || a.cfg != cfg || m.engine.Config() != cfg || m.monitor.currentConfig() != cfg {
		t.Fatal("failed preflight changed runtime")
	}
	assertPort(t, newAddr, false)
	assertPort(t, cfg.Groups[0].Listen, true)
	// Invalid external file is restored to the last accepted bytes, including comments.
	os.WriteFile(m.path, []byte("typo: invalid"), 0o600)
	if err := m.Reload(); err == nil {
		t.Fatal("invalid reload accepted")
	}
	current, _ = os.ReadFile(m.path)
	if !bytes.Equal(current, oldRaw) {
		t.Fatal("reload did not restore disk")
	}
	// Filesystem failure after socket preparation leaves runtime and old config untouched.
	blocked := filepath.Join(cfg.DataDir, "blocked.yaml")
	os.WriteFile(blocked, oldRaw, 0o600)
	os.Mkdir(blocked+".bak", 0o700)
	m.path = blocked
	next.Groups = next.Groups[:2]
	raw, _ = next.Marshal()
	if err := m.Save(raw); err == nil {
		t.Fatal("unwritable backup accepted")
	}
	assertPort(t, newAddr, false)
	current, _ = os.ReadFile(blocked)
	if !bytes.Equal(current, oldRaw) || m.cfg != cfg {
		t.Fatal("write failure changed config")
	}
	entries, _ := os.ReadDir(cfg.DataDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".linkgate-config-") {
			t.Fatal("staged file leaked")
		}
	}
}

func TestCRUDValidationAuthAndConcurrentUpdates(t *testing.T) {
	m, a, _ := managerFixture(t)
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/__linkgate/v1/groups", `{"name":"test","listen":"127.0.0.1:19000"}`, 409},
		{"PUT", "/__linkgate/v1/groups", `{"name":"missing"}`, 404},
		{"POST", "/__linkgate/v1/groups", `{"name":"tcp","listen":"127.0.0.1:19000","protocol":"tcp"}`, 400},
		{"PUT", "/__linkgate/v1/groups", `{"name":"test","timeout_overrides":{"typo":"1s"}}`, 400},
		{"PUT", "/__linkgate/v1/groups", `{"name":"test","upstreams":[]}`, 400},
		{"DELETE", "/__linkgate/v1/groups", `{"name":"test","listen":"bad"}`, 400},
		{"POST", "/__linkgate/v1/groups/test/upstreams", `{"name":"bad","url":"ftp://bad"}`, 400},
		{"GET", "/__linkgate/v1/groups", "", 405},
	} {
		expectCode(t, adminRequest(a, tc.method, tc.path, tc.body), tc.code)
	}
	r := httptest.NewRequest("POST", "/__linkgate/v1/groups", strings.NewReader(`{"name":"bad"}`))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	expectCode(t, w, 401)
	r.Header.Set("X-Linkgate-Token", a.Token())
	r.Header.Set("Origin", "https://evil.invalid")
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	expectCode(t, w, 403)
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := adminRequest(a, "POST", "/__linkgate/v1/groups/test/upstreams", fmt.Sprintf(`{"name":"u%d","url":"http://example.invalid"}`, i))
			codes <- w.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 201 {
			t.Fatalf("concurrent update HTTP %d", code)
		}
	}
	if len(m.cfg.Groups[0].Upstreams) != 13 {
		t.Fatal("lost concurrent edits")
	}
}

func TestListenerReassignmentPreservesPortAndDispatch(t *testing.T) {
	m, a, cfg := managerFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ready") }))
	defer upstream.Close()
	addr := cfg.Groups[0].Listen
	err := m.Mutate(func(c *Config) error {
		c.Groups = []GroupConfig{{Name: "replacement", Listen: addr, Upstreams: []UpstreamConfig{{Name: "up", URL: upstream.URL}}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ready" {
		t.Fatalf("dispatch = %s", b)
	}
	expectCode(t, adminRequest(a, "GET", "/__linkgate/v1/status", ""), 200)
}

func TestUptimeAndEventDate(t *testing.T) {
	_, a, _ := managerFixture(t)
	a.startedAt = time.Now().Add(-72 * time.Hour).Round(0)
	w := adminRequest(a, "GET", "/__linkgate/v1/status", "")
	var status struct {
		Started time.Time `json:"started_at"`
		Uptime  int64     `json:"uptime_seconds"`
	}
	json.Unmarshal(w.Body.Bytes(), &status)
	if status.Uptime < 72*3600 || status.Uptime > 72*3600+1 {
		t.Fatalf("uptime %d", status.Uptime)
	}
	e := a.events.Add(Event{Message: "date"})
	b, _ := json.Marshal(e)
	if !strings.Contains(string(b), e.Time.Format("2006-01-02")) {
		t.Fatal("missing event date")
	}
	if NewEventLog(0).max != 2000 || Default().Log.RingSize != 2000 {
		t.Fatal("default capacity")
	}
}
