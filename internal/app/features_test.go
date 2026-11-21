package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProbeResponseClassesAndConfiguredExpectation(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  string
		ok     bool
	}{
		{200, "healthy", true}, {204, "healthy", true}, {302, "healthy", true},
		{401, "auth", false}, {403, "auth", false}, {404, "notfound", false}, {503, "server_error", false}, {429, "other", false},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) }))
			defer up.Close()
			cfg := testConfig(UpstreamConfig{Name: "primary", URL: up.URL})
			_, store, events := testEngine(t, cfg)
			monitor := NewHealthMonitor(cfg, store, events, testLogger())
			r := monitor.ProbeNowResult("test", "primary")
			if !r.Reach || r.OK != tc.ok || r.Class != tc.class || r.HTTPStatus != tc.status {
				t.Fatalf("result %+v", r)
			}
			state := testState(t, store, cfg, "primary")
			if !state.Reach || state.Class != tc.class || state.HTTPStatus != tc.status {
				t.Fatalf("state %+v", state)
			}
			cfg.Health.Probe.ExpectStatus = tc.status
			r = monitor.ProbeNowResult("test", "primary")
			if !r.OK || r.Class != tc.class {
				t.Fatalf("expect_status changed transport class: %+v", r)
			}
		})
	}
}

func TestProbeHeadersProveReachEvenWhenBodyStalls(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: up.URL})
	cfg.Health.Timeout = Duration{100 * time.Millisecond}
	result := (httpHealthProbe{}).Run(context.Background(), cfg, &cfg.Groups[0], cfg.Groups[0].Upstreams[0], effectiveProbe(cfg, cfg.Groups[0].Upstreams[0]))
	if !result.Reach || result.Class != "auth" {
		t.Fatalf("%+v", result)
	}
}

func TestProbeUnreachableDialTimeoutTLSAndAPI(t *testing.T) {
	m, a, _ := managerFixture(t)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	for _, target := range []string{"http://" + freeAddress(t), up.URL} {
		if err := m.Mutate(func(cfg *Config) error { cfg.Groups[0].Upstreams[0].URL = target; return nil }); err != nil {
			t.Fatal(err)
		}
		w := adminRequest(a, "POST", "/__linkgate/v1/probe?group=test&upstream=primary", "")
		expectCode(t, w, 200)
		var r ProbeResult
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Reach || r.OK || r.Class != "unreachable" || r.HTTPStatus != 0 {
			t.Fatalf("%+v", r)
		}
	}
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer stall.Close()
	if err := m.Mutate(func(cfg *Config) error {
		cfg.Groups[0].Upstreams[0].URL = stall.URL
		cfg.Health.Timeout = Duration{20 * time.Millisecond}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(a, "POST", "/__linkgate/v1/probe?group=test&upstream=primary", "")
	expectCode(t, w, 200)
	var result ProbeResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.Reach || result.Class != "unreachable" {
		t.Fatalf("timeout %+v", result)
	}
	expectCode(t, adminRequest(a, "POST", "/__linkgate/v1/probe?group=test&upstream=missing", ""), 404)
}

func TestEventsGroupFilterBeforeLimitAndPageCursor(t *testing.T) {
	_, a, _ := managerFixture(t)
	a.events = NewEventLog(5)
	for _, group := range []string{"test", "other", "test", "other", "other", ""} {
		a.events.Add(Event{Group: group, Message: group})
	}
	w := adminRequest(a, "GET", "/__linkgate/v1/events?group=test&limit=1", "")
	expectCode(t, w, 200)
	var response struct {
		Events []Event `json:"events"`
		Last   uint64  `json:"last_seq"`
	}
	json.Unmarshal(w.Body.Bytes(), &response)
	if len(response.Events) != 1 || response.Events[0].Seq != 3 || response.Last != 3 {
		t.Fatalf("%+v", response)
	}
	w = adminRequest(a, "GET", "/__linkgate/v1/events?group=test&after=3", "")
	json.Unmarshal(w.Body.Bytes(), &response)
	if len(response.Events) != 0 || response.Last != 6 {
		t.Fatalf("%+v", response)
	}
	w = adminRequest(a, "GET", "/__linkgate/v1/events", "")
	json.Unmarshal(w.Body.Bytes(), &response)
	if len(response.Events) != 5 {
		t.Fatal("global events not retained")
	}
	expectCode(t, adminRequest(a, "POST", "/__linkgate/v1/events", ""), 405)
}

func TestSettingsStrictPatchPersistsAndApplies(t *testing.T) {
	m, a, cfg := managerFixture(t)
	before, _ := os.ReadFile(m.path)
	w := adminRequest(a, "GET", "/__linkgate/v1/settings", "")
	expectCode(t, w, 200)
	var response struct {
		Settings Settings       `json:"settings"`
		Fields   []SettingField `json:"fields"`
		Restart  map[string]any `json:"restart_required"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Settings.Log.RingSize != cfg.Log.RingSize || response.Restart["data_dir"] != cfg.DataDir {
		t.Fatalf("%+v", response)
	}
	for _, f := range response.Fields {
		if f.Path == "log.level" && f.Effect != "hot" {
			t.Fatal("level not hot")
		}
	}
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/settings", `{"log":{"ring_size":17,"level":"debug"},"health":{"interval":"2s","probe":{"path":"/ready","expect_status":404}},"retry":{"max_attempts":2},"buffer":{"max_concurrent":7},"http":{"connect_timeout":"750ms"}}`), 200)
	if m.cfg.Log.RingSize != 17 || m.events.max != 17 || m.cfg.Health.Probe.Path != "/ready" || m.cfg.Health.Probe.ExpectStatus != 404 || m.cfg.Health.Timeout != cfg.Health.Timeout || m.cfg.Retry.Mode != cfg.Retry.Mode || m.engine.group("test").httpCfg.ConnectTimeout.Duration != 750*time.Millisecond || m.engine.group("test").sem.limit != 7 {
		t.Fatal("settings did not patch/apply")
	}
	bak, _ := os.ReadFile(m.path + ".bak")
	if !bytes.Equal(bak, before) {
		t.Fatal("backup not exact")
	}
	loaded, err := Load(m.path)
	if err != nil || loaded.Log.Level != "debug" || loaded.Health.Probe.Path != "/ready" {
		t.Fatalf("persisted %+v %v", loaded, err)
	}
	accepted := m.cfg
	raw, _ := os.ReadFile(m.path)
	for _, body := range []string{
		`{"log":{"level":"trace"}}`, `{"log":{"ring_size":0}}`, `{"buffer":{"max_concurrent":-1}}`, `{"health":{"fail_threshold":0}}`,
		`{"health":{"probe":{"Path":"/ready"}}}`, `{"Health":{"interval":"1s"}}`, `{"log":{"level":"info","level":"error"}}`,
		`{"retry":{"max_attempts":null}}`, `{"http":{"connect_timeout":"-1s"}}`, `{"admin":{"listen":"bad"}}`, `{"log":{"format":"changed"}}`,
	} {
		expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/settings", body), 400)
	}
	now, _ := os.ReadFile(m.path)
	if accepted != m.cfg || !bytes.Equal(raw, now) {
		t.Fatal("invalid patch mutated state")
	}
	expectCode(t, adminRequest(a, "POST", "/__linkgate/v1/settings", `{}`), 405)
}

func TestLogLevelChangesExistingLoggerAtomically(t *testing.T) {
	cfg := Default()
	cfg.Log.File = t.TempDir() + "/log.txt"
	logger, closer, err := NewLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	derived := logger.With("group", "test")
	if derived.Enabled(context.Background(), -4) {
		t.Fatal("debug initially enabled")
	}
	setLoggerLevel(logger, "debug")
	if !derived.Enabled(context.Background(), -4) {
		t.Fatal("derived logger did not hot update")
	}
}

func TestStatsClientRequestsAndUALinesSurviveReload(t *testing.T) {
	m, a, _ := managerFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(503)
		} else {
			w.Write([]byte("ok"))
		}
	}))
	defer up.Close()
	if err := m.Mutate(func(cfg *Config) error {
		cfg.Retry.Backoff = Duration{}
		cfg.Groups[0].Upstreams = []UpstreamConfig{{Name: "dead", URL: "http://" + freeAddress(t)}, {Name: "live", URL: up.URL}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	request := func(path, ua string) {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("User-Agent", ua)
		m.engine.Handler("test").ServeHTTP(httptest.NewRecorder(), r)
	}
	request("/ok", "curl/8.0")
	request("/fail", "codex/1.0")
	w := adminRequest(a, "GET", "/__linkgate/v1/stats", "")
	expectCode(t, w, 200)
	var st struct {
		Services int            `json:"service_count"`
		Lines    int            `json:"line_count"`
		Events   int            `json:"event_count"`
		Groups   []ServiceStats `json:"groups"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.Services != 1 || st.Lines != 2 || st.Events < 1 || st.Groups[0].Total != 2 || st.Groups[0].Success != 1 || st.Groups[0].Failed != 1 || st.Groups[0].Failover != 2 || st.Groups[0].LastRequestAt.IsZero() {
		t.Fatalf("%+v", st)
	}
	if len(st.Groups[0].UALines) != 2 || st.Groups[0].UALines[0].Line != "live" {
		t.Fatalf("ua lines %+v", st.Groups[0].UALines)
	}
	old := m.cfg
	oldTable := m.store.View(old)
	if err := m.Mutate(func(cfg *Config) error { cfg.Groups[0].Comment = "retain"; return nil }); err != nil {
		t.Fatal(err)
	}
	m.store.RecordRequestEnd(old, "test", "curl", "live", true, false)
	if oldTable.services["test"].Total != 2 {
		t.Fatal("old generation written")
	}
	if got := m.store.ServiceStats(m.store.Snapshot(m.cfg))[0]; got.Total != 2 || len(got.UALines) != 2 {
		t.Fatalf("reload lost counters %+v", got)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.store.RecordRequestEnd(m.cfg, "test", "arbitrary-private-UA", "live", true, false)
		}()
	}
	wg.Wait()
	stats := m.store.ServiceStats(m.store.Snapshot(m.cfg))[0]
	if stats.Total != 52 || len(stats.UALines) != 3 || stats.UALines[2].UA != "other" || stats.UALines[2].Count != 50 {
		t.Fatalf("%+v", stats)
	}
}

func TestServiceOrderAndBulkLineOrderPersistence(t *testing.T) {
	m, a, _ := managerFixture(t)
	expectCode(t, adminRequest(a, "POST", "/__linkgate/v1/groups", fmt.Sprintf(`{"name":"second","listen":%q}`, freeAddress(t))), 201)
	expectCode(t, adminRequest(a, "POST", "/__linkgate/v1/groups/test/upstreams", `{"name":"backup","url":"http://example.invalid"}`), 201)
	oldRaw, _ := os.ReadFile(m.path)
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/groups/test/upstreams/order", `{"order":["backup","primary"]}`), 200)
	bak, _ := os.ReadFile(m.path + ".bak")
	if !bytes.Equal(bak, oldRaw) {
		t.Fatal("batch not one atomic commit")
	}
	if got := m.store.OrderedNames("test"); strings.Join(got, ",") != "backup,primary" {
		t.Fatal(got)
	}
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/ui/order", `{"sort_mode":"manual","service_order":["second","test"]}`), 200)
	loaded, err := Load(m.path)
	if err != nil || strings.Join(loaded.UI.ServiceOrder, ",") != "second,test" {
		t.Fatalf("%+v %v", loaded, err)
	}
	readOrder := func() string {
		w := adminRequest(a, "GET", "/__linkgate/v1/status", "")
		var st struct {
			Groups []GroupState `json:"groups"`
		}
		json.Unmarshal(w.Body.Bytes(), &st)
		return st.Groups[0].Name
	}
	if readOrder() != "second" {
		t.Fatal("manual not applied")
	}
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/ui/order", `{"sort_mode":"recent"}`), 200)
	m.store.RecordRequestStart(m.cfg, "test", time.Now())
	if readOrder() != "test" {
		t.Fatal("recent not applied")
	}
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/ui/order", `{"sort_mode":"added"}`), 200)
	if readOrder() != "second" {
		t.Fatal("added/config order not applied")
	}
	for _, body := range []string{`{"sort_mode":"alphabetic"}`, `{"service_order":["test","test"]}`, `{"service_order":["test"]}`, `{"Sort_mode":"manual"}`} {
		expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/ui/order", body), 400)
	}
	for _, body := range []string{`{"order":["backup","backup"]}`, `{"order":["primary"]}`, `{"Order":["backup","primary"]}`, `{"order":null}`} {
		expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/groups/test/upstreams/order", body), 400)
	}
	expectCode(t, adminRequest(a, "DELETE", "/__linkgate/v1/groups", `{"name":"second"}`), 200)
	if len(m.cfg.UI.ServiceOrder) != 1 || m.cfg.UI.ServiceOrder[0] != "test" {
		t.Fatal("delete left invalid service_order")
	}
}
