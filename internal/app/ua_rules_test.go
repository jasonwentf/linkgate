package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func uaCandidateNames(e *Engine, g *groupRuntime, frozen map[string]lineSnapshot, size int64, ua string) []string {
	out := make([]string, 0)
	for _, u := range e.candidates(g, frozen, size, g.pinnedLine(ua)) {
		out = append(out, u.cfg.Name)
	}
	return out
}

func TestUAMatchModes(t *testing.T) {
	for _, tc := range []struct {
		match, ua string
		want      bool
	}{
		{"Chrome", "Mozilla/5.0 Chrome/131 Safari/537", true},
		{"chrome", "Firefox/132", false},
		{"exact:Codex/1", "CODEX/1", true},
		{"exact:Codex/1", "Codex/10", false},
		{"prefix:Claude-Code/", "CLAUDE-CODE/2", true},
		{"prefix:Claude-Code/", "wrapped claude-code/2", false},
		{"regex:(?i)^curl/[0-9]+$", "CURL/8", true},
		{"regex:^curl/[0-9]+$", "CURL/8", false},
		{"PREFIX:curl/", "curl/8", true},
	} {
		t.Run(tc.match+"-"+tc.ua, func(t *testing.T) {
			m, err := compileUAMatch(tc.match)
			if err != nil {
				t.Fatal(err)
			}
			if got := m.matches(tc.ua); got != tc.want {
				t.Fatalf("match=%q ua=%q got=%v want=%v", tc.match, tc.ua, got, tc.want)
			}
		})
	}
}

func TestUAPinningCandidateOrderAndFirstMatch(t *testing.T) {
	cfg := testConfig(
		UpstreamConfig{Name: "primary", URL: "http://primary.invalid", Priority: intPtr(10)},
		UpstreamConfig{Name: "second", URL: "http://second.invalid", Priority: intPtr(20)},
		UpstreamConfig{Name: "cf", URL: "http://cf.invalid", Priority: intPtr(30)},
	)
	cfg.Groups[0].UARules = []UARuleConfig{{Match: "Chrome", Line: "cf"}, {Match: "prefix:Mozilla/", Line: "second"}}
	e, _, _ := testEngine(t, cfg)
	g := e.group("test")
	for _, tc := range []struct {
		ua   string
		want []string
	}{
		{"Mozilla/5.0 Chrome/131", []string{"cf", "primary", "second"}},
		{"Mozilla/5.0 Firefox/132", []string{"second", "primary", "cf"}},
		{"curl/8", []string{"primary", "second", "cf"}},
	} {
		if got := uaCandidateNames(e, g, e.freezeLines(g), 0, tc.ua); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("ua=%q candidates=%v want=%v", tc.ua, got, tc.want)
		}
	}
}

func TestUAPinningPreservesEligibilityAndFallbackOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config, *Store)
		size int64
		want []string
	}{
		{"unhealthy-target-first", func(c *Config, s *Store) { s.RecordFailure(c, "test", "cf", "dial", 0) }, 0, []string{"cf", "primary", "second"}},
		{"other-unhealthy-last", func(c *Config, s *Store) { s.RecordFailure(c, "test", "primary", "dial", 0) }, 0, []string{"cf", "second", "primary"}},
		{"runtime-disabled", func(c *Config, s *Store) { s.SetEnabled("test", "cf", false) }, 0, []string{"primary", "second"}},
		{"config-disabled", func(c *Config, s *Store) {}, 0, []string{"primary", "second"}},
		{"body-limit", func(c *Config, s *Store) {}, 101, []string{"primary", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(
				UpstreamConfig{Name: "primary", URL: "http://primary.invalid", Priority: intPtr(10)},
				UpstreamConfig{Name: "second", URL: "http://second.invalid", Priority: intPtr(20)},
				UpstreamConfig{Name: "cf", URL: "http://cf.invalid", Priority: intPtr(30), MaxBodyBytes: 100},
			)
			cfg.Health.FailThreshold = 1
			cfg.Groups[0].UARules = []UARuleConfig{{Match: "Chrome", Line: "cf"}, {Match: "prefix:Chrome/", Line: "second"}}
			if tc.name == "config-disabled" {
				cfg.Groups[0].Upstreams[2].Enabled = boolPtr(false)
			}
			e, s, _ := testEngine(t, cfg)
			tc.edit(cfg, s)
			g := e.group("test")
			if got := uaCandidateNames(e, g, e.freezeLines(g), tc.size, "Chrome/131"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("candidates=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestUAPinningDialFailureFallsBack(t *testing.T) {
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "backup") }))
	defer backup.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("closed target reached") }))
	closed.Close()
	cfg := testConfig(
		UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(10)},
		UpstreamConfig{Name: "cf", URL: closed.URL, Priority: intPtr(20)},
	)
	cfg.Health.FailThreshold = 1
	cfg.Groups[0].UARules = []UARuleConfig{{Match: "Chrome", Line: "cf"}}
	e, s, ev := testEngine(t, cfg)
	s.RecordFailure(cfg, "test", "cf", "already down", 0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("transparent-body"))
	req.Header.Set("User-Agent", "Chrome/131")
	rec := httptest.NewRecorder()
	e.Handler("test").ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "backup" {
		t.Fatalf("response=%d %q", rec.Code, rec.Body.String())
	}
	if got := testState(t, s, cfg, "cf"); got.RequestsFailed != 2 || got.FailoversOut != 1 {
		t.Fatalf("pinned target was not tried first: %+v", got)
	}
	if got := s.ServiceStats(s.Snapshot(cfg))[0]; got.Total != 1 || got.Failover != 1 {
		t.Fatalf("fallback counters=%+v", got)
	}
	events, _ := ev.Query("test", 0, 100)
	found := false
	for _, event := range events {
		found = found || event.Kind == "failover" && event.From == "cf" && event.To == "backup"
	}
	if !found {
		t.Fatal("missing pinned-to-backup event")
	}
}

func TestUAPinningDoesNotFailoverAfterDelivery(t *testing.T) {
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		io.WriteString(w, "backup")
	}))
	defer backup.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer bad.Close()
	cfg := testConfig(
		UpstreamConfig{Name: "backup", URL: backup.URL, Priority: intPtr(10)},
		UpstreamConfig{Name: "cf", URL: bad.URL, Priority: intPtr(20)},
	)
	cfg.Groups[0].UARules = []UARuleConfig{{Match: "Chrome", Line: "cf"}}
	e, _, _ := testEngine(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("already delivered"))
	req.Header.Set("User-Agent", "Chrome/131")
	rec := httptest.NewRecorder()
	e.Handler("test").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || backupHits.Load() != 0 {
		t.Fatalf("status=%d backup hits=%d; pin must not bypass zero-byte safety", rec.Code, backupHits.Load())
	}
}

func TestUAPinningRetainsRulesDuringBuffering(t *testing.T) {
	m, a, _ := managerFixture(t)
	makeUp := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, label)
		}))
	}
	primary, cf := makeUp("primary"), makeUp("cf")
	defer primary.Close()
	defer cf.Close()
	if err := m.Mutate(func(c *Config) error {
		c.Groups[0].Upstreams = []UpstreamConfig{{Name: "primary", URL: primary.URL}, {Name: "cf", URL: cf.URL}}
		c.Groups[0].UARules = []UARuleConfig{{Match: "Chrome", Line: "cf"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := m.engine.Handler("test")
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", pr)
	req.Header.Set("User-Agent", "Chrome/131")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	if _, err := pw.Write([]byte("buffering")); err != nil {
		t.Fatal(err)
	}
	// A runtime disable after entry cannot refilter the frozen eligible line once
	// this generation is retired, and successive commits cannot replace its rule.
	m.store.SetEnabled("test", "cf", false)
	for i := 0; i < 2; i++ {
		expectCode(t, adminRequest(a, http.MethodPut, "/__linkgate/v1/groups",
			`{"name":"test","ua_rules":[{"match":"Chrome","line":"primary"}]}`), http.StatusOK)
	}
	pw.Close()
	waitSignal(t, done)
	if rec.Code != http.StatusOK || rec.Body.String() != "cf" {
		t.Fatalf("old request=%d %q, want its frozen cf pin", rec.Code, rec.Body.String())
	}
	fresh := httptest.NewRequest(http.MethodGet, "/", nil)
	fresh.Header.Set("User-Agent", "Chrome/131")
	next := httptest.NewRecorder()
	m.engine.Handler("test").ServeHTTP(next, fresh)
	if next.Body.String() != "primary" {
		t.Fatalf("new generation did not apply new rule: %q", next.Body.String())
	}
}

func TestUAPinningFrozenEligibilityDoesNotReviveUnavailableTarget(t *testing.T) {
	for _, unavailable := range []string{"disabled", "unhealthy"} {
		t.Run(unavailable, func(t *testing.T) {
			m, _, _ := managerFixture(t)
			if err := m.Mutate(func(c *Config) error {
				c.Health.FailThreshold = 1
				c.Groups[0].Upstreams = append(c.Groups[0].Upstreams, UpstreamConfig{Name: "cf", URL: "http://cf.invalid"})
				c.Groups[0].UARules = []UARuleConfig{{Match: "Chrome", Line: "cf"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if unavailable == "disabled" {
				m.store.SetEnabled("test", "cf", false)
			} else {
				m.store.RecordFailure(m.cfg, "test", "cf", "dial", 0)
			}
			g := m.engine.group("test")
			frozen := m.engine.freezeLines(g)
			for i := 0; i < 2; i++ {
				if err := m.Mutate(func(c *Config) error { c.Groups[0].Comment = fmt.Sprint(i); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if got := uaCandidateNames(m.engine, g, frozen, 0, "Chrome/131"); !reflect.DeepEqual(got, []string{"primary"}) {
				t.Fatalf("pin revived frozen unavailable target: %v", got)
			}
		})
	}
}

func TestUARulesValidationAndStrictJSON(t *testing.T) {
	m, a, _ := managerFixture(t)
	path := "/__linkgate/v1/groups"
	expectCode(t, adminRequest(a, http.MethodPut, path,
		`{"name":"test","ua_rules":[{"match":"Chrome","line":"primary","comment":"valid"}]}`), http.StatusOK)
	generation := m.cfg
	raw, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(m.path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	for _, rules := range []string{
		`[{"match":"","line":"primary"}]`,
		`[{"match":"   ","line":"primary"}]`,
		`[{"match":"exact:","line":"primary"}]`,
		`[{"match":"prefix:  ","line":"primary"}]`,
		`[{"match":"regex:[","line":"primary"}]`,
		`[{"match":"Chrome\n","line":"primary"}]`,
		`[{"match":null,"line":"primary"}]`,
		`[{"match":123,"line":"primary"}]`,
		`[{"line":"primary"}]`,
		`[{"match":"Chrome"}]`,
		`[{"match":"Chrome","line":"missing"}]`,
		`[{"match":"Chrome","line":null}]`,
		`[{"match":"Chrome","line":"primary"},{"match":"chrome","line":"primary"}]`,
		`[{"match":"Chrome","line":"primary"},{"match":"Chrome","line":"primary"}]`,
		`[{"Match":"Chrome","line":"primary"}]`,
		`[{"match":"Chrome","Line":"primary"}]`,
		`[{"match":"Chrome","line":"primary","Comment":"x"}]`,
		`[{"match":"Chrome","line":"primary","extra":true}]`,
		`[{"match":"Chrome","match":"curl","line":"primary"}]`,
		`[{"match":"Chrome","line":"primary","line":"primary"}]`,
		`[{"match":"Chrome","line":"primary","comment":"x","comment":"y"}]`,
		`[null]`, `null`, `{}`, `"Chrome"`,
	} {
		t.Run(rules, func(t *testing.T) {
			expectCode(t, adminRequest(a, http.MethodPut, path, `{"name":"test","ua_rules":`+rules+`}`), http.StatusBadRequest)
			current, err := os.ReadFile(m.path)
			if err != nil {
				t.Fatal(err)
			}
			backup, err := os.ReadFile(m.path + ".bak")
			if err != nil {
				t.Fatal(err)
			}
			if m.cfg != generation || !bytes.Equal(current, raw) || !bytes.Equal(backup, bak) {
				t.Fatal("invalid rule changed live generation, config or .bak")
			}
		})
	}
	for _, body := range []string{
		`{"name":"test","UA_rules":[]}`,
		`{"name":"test","ua_ruleſ":[]}`,
		`{"name":"test","ua_rules":[],"ua_rules":[]}`,
	} {
		expectCode(t, adminRequest(a, http.MethodPut, path, body), http.StatusBadRequest)
	}
	// YAML uses KnownFields plus the same semantic validation.
	for _, fragment := range []string{
		"[{match: '', line: primary}]", "[{match: Chrome, line: missing}]",
		"[{match: Chrome, line: primary}, {match: chrome, line: primary}]",
		"[{match: Chrome, Line: primary}]",
	} {
		if _, err := Parse([]byte("groups:\n  - name: test\n    listen: 127.0.0.1:18765\n    upstreams: [{name: primary, url: 'http://primary.invalid'}]\n    ua_rules: " + fragment + "\n")); err == nil {
			t.Fatalf("invalid YAML rule accepted: %s", fragment)
		}
	}
	// Never print invalid regex patterns in errors (regexp errors include raw input).
	w := adminRequest(a, http.MethodPut, path, `{"name":"test","ua_rules":[{"match":"regex:[http://user:synthetic-secret@host/?token=synthetic-token","line":"primary"}]}`)
	expectCode(t, w, http.StatusBadRequest)
	if strings.Contains(w.Body.String(), "synthetic-secret") || strings.Contains(w.Body.String(), "synthetic-token") {
		t.Fatal("invalid match leaked its value")
	}
}

func TestUARulesCRUDPersistenceAndSnapshots(t *testing.T) {
	m, a, _ := managerFixture(t)
	path := "/__linkgate/v1/groups"
	original, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	rules := []UARuleConfig{{Match: "Chrome", Line: "primary", Comment: "browser"}, {Match: "prefix:curl/", Line: "primary"}}
	body, _ := json.Marshal(map[string]any{"name": "test", "ua_rules": rules})
	expectCode(t, adminRequest(a, http.MethodPut, path, string(body)), http.StatusOK)
	backup, err := os.ReadFile(m.path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup, original) {
		t.Fatal("successful rule update did not preserve prior YAML")
	}
	disk, err := Load(m.path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(disk.Groups[0].UARules, rules) {
		t.Fatal("rules not persisted")
	}
	// An unrelated patch must preserve rules; neither snapshot shares the config slice.
	expectCode(t, adminRequest(a, http.MethodPut, path, `{"name":"test","comment":"unrelated"}`), http.StatusOK)
	for _, endpoint := range []string{"status", "stats", "config"} {
		w := adminRequest(a, http.MethodGet, "/__linkgate/v1/"+endpoint, "")
		expectCode(t, w, http.StatusOK)
		var out struct {
			Groups []struct {
				UARules []UARuleConfig `json:"ua_rules"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Groups) != 1 || !reflect.DeepEqual(out.Groups[0].UARules, rules) {
			t.Fatalf("%s rules missing", endpoint)
		}
	}
	snapshot := m.store.Snapshot(m.cfg)
	stats := m.store.ServiceStats(snapshot)
	snapshot[0].UARules[0].Match = "mutated-snapshot"
	stats[0].UARules[0].Match = "mutated-stats"
	if m.cfg.Groups[0].UARules[0].Match != "Chrome" {
		t.Fatal("snapshot aliased live config")
	}
	// Deleting a referenced line is rejected; clear its rules explicitly first.
	expectCode(t, adminRequest(a, http.MethodDelete, path+"/test/upstreams", `{"name":"primary"}`), http.StatusBadRequest)
	expectCode(t, adminRequest(a, http.MethodPut, path, `{"name":"test","ua_rules":[]}`), http.StatusOK)
	w := adminRequest(a, http.MethodGet, "/__linkgate/v1/status", "")
	if !strings.Contains(w.Body.String(), `"ua_rules":[]`) {
		t.Fatal("empty rules should be an array for UI")
	}
	expectCode(t, adminRequest(a, http.MethodDelete, path+"/test/upstreams", `{"name":"primary"}`), http.StatusOK)
	full := fmt.Sprintf(`{"name":"full-pin","listen":%q,"upstreams":[{"name":"cf","url":"http://cf.invalid"}],"ua_rules":[{"match":"Chrome","line":"cf"}]}`, freeAddress(t))
	expectCode(t, adminRequest(a, http.MethodPost, path, full), http.StatusCreated)
	if len(m.cfg.Group("full-pin").UARules) != 1 {
		t.Fatal("POST did not create rules")
	}
}
