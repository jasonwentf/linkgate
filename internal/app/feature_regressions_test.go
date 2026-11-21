package app

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestEventIncrementalPagesDoNotSkipMatches(t *testing.T) {
	for _, stream := range []string{"", "state", "request"} {
		t.Run(stream, func(t *testing.T) {
			l := NewEventLog(20)
			kind := "health"
			if stream == "request" {
				kind = "request"
			}
			l.Add(Event{Kind: kind, Level: "info", Group: "test"})
			for i := 0; i < 4; i++ {
				l.Add(Event{Kind: kind, Level: "info", Group: "test"})
			}
			first, cursor := l.QueryStream("test", stream, kind, 1, 2)
			second, next := l.QueryStream("test", stream, kind, cursor, 2)
			var seqs []uint64
			for _, e := range append(first, second...) {
				seqs = append(seqs, e.Seq)
			}
			if !reflect.DeepEqual(seqs, []uint64{2, 3, 4, 5}) || cursor != 3 || next != 5 {
				t.Fatalf("pages=%v cursor=%d next=%d", seqs, cursor, next)
			}
		})
	}
}

func TestEventCursorTracksReturnedMatchNotUnrelatedTail(t *testing.T) {
	l := NewEventLog(20)
	for _, group := range []string{"test", "other", "test", "other", "test", "other"} {
		l.Add(Event{Kind: "health", Group: group})
	}
	page, cursor := l.QueryStream("test", "state", "health", 1, 1)
	if len(page) != 1 || page[0].Seq != 3 || cursor != 3 {
		t.Fatal(page, cursor)
	}
	page, cursor = l.QueryStream("test", "state", "health", cursor, 1)
	if len(page) != 1 || page[0].Seq != 5 || cursor != 5 {
		t.Fatal(page, cursor)
	}
	page, cursor = l.QueryStream("test", "state", "health", cursor, 1)
	if len(page) != 0 || cursor != 6 {
		t.Fatal("exhausted filter should advance past unrelated events", page, cursor)
	}
	page, cursor = l.QueryStream("test", "state", "health", 0, 2)
	if len(page) != 2 || page[0].Seq != 3 || page[1].Seq != 5 || cursor != 5 {
		t.Fatal("initial query should keep recent semantics", page, cursor)
	}
}

func TestRequestEventsNeverEvictLifecycleAtAnyLevel(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		t.Run(level, func(t *testing.T) {
			l := NewEventLog(4)
			for _, kind := range []string{"config", "health", "failover", "start"} {
				l.Add(Event{Kind: kind, Level: "info", Message: kind})
			}
			for i := 0; i < 20; i++ {
				l.Add(Event{Kind: "request", Level: level, Message: "request"})
			}
			l.Add(Event{Kind: "probe", Level: level})
			states, _ := l.QueryStream("", "state", "", 0, 20)
			requests, _ := l.QueryStream("", "request", "request", 0, 20)
			if len(states) != 4 || len(requests) != 3 {
				t.Fatal(states, requests)
			}
			for _, e := range states {
				if e.Kind == "request" || e.Kind == "probe" {
					t.Fatal("non-lifecycle event in state ring", e)
				}
			}
		})
	}
}

func TestProbeNeverParsesOrFollowsRedirectLocations(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1); w.WriteHeader(401) }))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, location := range []string{"http://%zz", target.URL, ""} {
			t.Run(fmt.Sprintf("%d/%s", status, location), func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", location)
					w.WriteHeader(status)
				}))
				defer up.Close()
				cfg := testConfig(UpstreamConfig{Name: "primary", URL: up.URL})
				_, store, events := testEngine(t, cfg)
				mon := NewHealthMonitor(cfg, store, events, testLogger())
				r := mon.ProbeNowResult("test", "primary")
				if !r.Reach || !r.OK || r.HTTPStatus != status || r.Class != "healthy" {
					t.Fatalf("redirect %+v", r)
				}
			})
		}
	}
	if targetHits.Load() != 0 {
		t.Fatal("probe followed redirect", targetHits.Load())
	}
}

func TestPendingStartupExternalEditCannotHotApply(t *testing.T) {
	for _, entry := range []string{"reload", "save"} {
		t.Run(entry, func(t *testing.T) {
			m, a, cfg := managerFixture(t)
			staged := freeAddress(t)
			expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/settings", fmt.Sprintf(`{"admin":{"listen":%q}}`, staged)), 200)
			before, _ := os.ReadFile(m.path)
			backup, _ := os.ReadFile(m.path + ".bak")
			next, err := Parse(before)
			if err != nil {
				t.Fatal(err)
			}
			changed := freeAddress(t)
			next.Admin.Listen = changed
			raw, err := next.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if entry == "reload" {
				if err := os.WriteFile(m.path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				err = m.Reload()
			} else {
				err = m.Save(raw)
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.cfg.Admin.Listen != cfg.Admin.Listen || m.saved.Admin.Listen != changed || !reflect.DeepEqual(m.PendingRestart(), []string{"admin.listen"}) {
				t.Fatal("pending startup edit was not saved without applying")
			}
			after, _ := os.ReadFile(m.path)
			afterBak, _ := os.ReadFile(m.path + ".bak")
			if !bytes.Equal(raw, after) {
				t.Fatal("updated pending value not persisted")
			}
			if entry == "reload" && !bytes.Equal(backup, afterBak) {
				t.Fatal("reload modified backup")
			}
			if entry == "save" && !bytes.Equal(before, afterBak) {
				t.Fatal("save did not back up previous pending configuration")
			}
			assertPort(t, cfg.Admin.Listen, true)
			assertPort(t, staged, false)
			assertPort(t, changed, false)
			if err := m.Mutate(func(c *Config) error { c.Groups[0].Comment = "keep pending"; return nil }); err != nil {
				t.Fatal(err)
			}
			if m.saved.Admin.Listen != changed || m.cfg.Admin.Listen != cfg.Admin.Listen {
				t.Fatal("CRUD dropped or applied pending address")
			}
			updated := freeAddress(t)
			if err := m.Mutate(func(c *Config) error { c.Admin.Listen = updated; return nil }); err != nil {
				t.Fatal(err)
			}
			if m.saved.Admin.Listen != updated || m.cfg.Admin.Listen != cfg.Admin.Listen {
				t.Fatal("CRUD startup edit did not remain pending")
			}
			if err := m.Mutate(func(c *Config) error { c.Admin.Listen = cfg.Admin.Listen; return nil }); err != nil {
				t.Fatal(err)
			}
			if len(m.PendingRestart()) != 0 {
				t.Fatal("CRUD did not cancel restored pending field")
			}
		})
	}
}

func TestCancelOnePendingFieldLeavesOthersPending(t *testing.T) {
	m, a, cfg := managerFixture(t)
	staged := freeAddress(t)
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/settings", fmt.Sprintf(`{"admin":{"listen":%q},"log":{"format":"json"}}`, staged)), 200)
	next, err := Load(m.path)
	if err != nil {
		t.Fatal(err)
	}
	next.Log.Format = cfg.Log.Format
	next.Groups[0].Comment = "hot edit"
	raw, err := next.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Admin.Listen != cfg.Admin.Listen || m.saved.Admin.Listen != staged || m.cfg.Groups[0].Comment != "hot edit" || !reflect.DeepEqual(m.PendingRestart(), []string{"admin.listen"}) {
		t.Fatal("canceling one pending resource affected another", m.PendingRestart())
	}
	next, err = Load(m.path)
	if err != nil {
		t.Fatal(err)
	}
	next.Admin.Listen = cfg.Admin.Listen
	raw, err = next.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Save(raw); err != nil {
		t.Fatal(err)
	}
	if len(m.PendingRestart()) != 0 {
		t.Fatal("restoring active resources did not clear pending")
	}
}
