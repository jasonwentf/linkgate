package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsPendingRestartPersistsWithoutApplyingAndSurvivesCRUD(t *testing.T) {
	m, a, cfg := managerFixture(t)
	before, _ := os.ReadFile(m.path)
	newAdmin := freeAddress(t)
	newDir := filepath.Join(t.TempDir(), "pending-data")
	newFile := filepath.Join(t.TempDir(), "pending.log")
	body := fmt.Sprintf(`{"data_dir":%q,"admin":{"listen":%q,"token_file":%q},"log":{"file":%q,"format":"json","max_backups":4,"max_size_bytes":9999,"level":"debug"},"health":{"interval":"1s"}}`, newDir, newAdmin, filepath.Join(newDir, "token"), newFile)
	w := adminRequest(a, "PUT", "/__linkgate/v1/settings", body)
	expectCode(t, w, 200)
	if m.cfg.Admin.Listen != cfg.Admin.Listen || m.cfg.DataDir != cfg.DataDir || m.cfg.Log.File != cfg.Log.File || m.cfg.Log.Format != cfg.Log.Format || m.cfg.Log.Level != "debug" || m.cfg.Health.Interval.String() != "1s" {
		t.Fatal("staged resource applied or hot setting ignored")
	}
	assertPort(t, cfg.Admin.Listen, true)
	assertPort(t, newAdmin, false)
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatal("pending log was opened")
	}
	if _, err := os.Stat(newDir); !os.IsNotExist(err) {
		t.Fatal("pending data dir created")
	}
	loaded, err := Load(m.path)
	if err != nil || loaded.Admin.Listen != newAdmin || loaded.DataDir != newDir || loaded.Log.File != newFile {
		t.Fatalf("disk %+v %v", loaded, err)
	}
	if len(m.PendingRestart()) != 7 {
		t.Fatal(m.PendingRestart())
	}
	bak, _ := os.ReadFile(m.path + ".bak")
	if !bytes.Equal(before, bak) {
		t.Fatal("wrong backup")
	}
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/groups", `{"name":"test","comment":"later edit"}`), 200)
	loaded, _ = Load(m.path)
	if loaded.Admin.Listen != newAdmin || loaded.Log.File != newFile || loaded.Group("test").Comment != "later edit" {
		t.Fatal("CRUD erased staged resources")
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Admin.Listen != cfg.Admin.Listen || m.cfg.Log.File != cfg.Log.File {
		t.Fatal("reload applied pending")
	}
	w = adminRequest(a, "GET", "/__linkgate/v1/settings", "")
	var st struct {
		Settings Settings `json:"settings"`
		Active   Settings `json:"active_settings"`
		Pending  []string `json:"pending_restart"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.Settings.Admin.Listen != newAdmin || st.Active.Admin.Listen != cfg.Admin.Listen || len(st.Pending) != 7 {
		t.Fatalf("%+v", st)
	}
	active := settingsOf(m.cfg)
	b, _ := json.Marshal(active)
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/settings", string(b)), 200)
	if len(m.PendingRestart()) != 0 {
		t.Fatal("pending not cleared after restoring active")
	}
}

func TestPendingSettingsDiskFailureKeepsRuntimeAndSavedConfig(t *testing.T) {
	m, a, _ := managerFixture(t)
	before := m.cfg
	saved := m.saved
	raw, _ := os.ReadFile(m.path)
	if err := os.Mkdir(m.path+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(a, "PUT", "/__linkgate/v1/settings", `{"log":{"format":"json","level":"debug"}}`)
	expectCode(t, w, 500)
	after, _ := os.ReadFile(m.path)
	if m.cfg != before || m.saved != saved || !bytes.Equal(raw, after) || len(m.PendingRestart()) != 0 {
		t.Fatal("failed disk transaction partially applied")
	}
}

func TestEventStreamsPreserveLifecycleDuringRequestFlood(t *testing.T) {
	l := NewEventLog(3)
	l.Add(Event{Kind: "health", Group: "quiet", Level: "warn", Message: "state"})
	for i := 0; i < 100; i++ {
		l.Add(Event{Kind: "request", Group: "busy", Level: "info", Message: "request"})
	}
	states, seq := l.QueryStream("quiet", "state", "", 0, 10)
	requests, _ := l.QueryStream("", "request", "", 0, 10)
	if len(states) != 1 || states[0].Message != "state" || seq != 1 || len(requests) != 3 {
		t.Fatal(states, requests, seq)
	}
	l.Resize(1)
	states, _ = l.QueryStream("", "state", "", 0, 10)
	requests, _ = l.QueryStream("", "request", "", 0, 10)
	if len(states) != 1 || len(requests) != 1 {
		t.Fatal("resize lost lifecycle")
	}
	_, a, _ := managerFixture(t)
	a.events = l
	w := adminRequest(a, "GET", "/__linkgate/v1/events?stream=state&group=quiet&kind=health", "")
	expectCode(t, w, 200)
	expectCode(t, adminRequest(a, "GET", "/__linkgate/v1/events?stream=invalid", ""), 400)
}

func TestUAUndeliveredAndIdentityReset(t *testing.T) {
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://" + freeAddress(t)})
	e, s, _ := testEngine(t, cfg)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("User-Agent", "zcode/1")
	e.Handler("test").ServeHTTP(httptest.NewRecorder(), r)
	st := s.ServiceStats(s.Snapshot(cfg))[0]
	if len(st.UALines) != 1 || st.UALines[0].Line != "" || st.UALines[0].Count != 1 || st.UALines[0].Failed != 1 {
		t.Fatalf("%+v", st)
	}
	s.RecordRequestEnd(cfg, "test", "curl", "primary", true, false)
	raw, _ := cfg.Marshal()
	next, _ := Parse(raw)
	next.Groups[0].Upstreams[0].URL = "http://example.invalid/new"
	s.Load(next, configThresholds(next), s)
	st = s.ServiceStats(s.Snapshot(next))[0]
	if len(st.UALines) != 1 || st.UALines[0].Line != "" {
		t.Fatalf("identity carried stale flow %+v", st)
	}
}
