package app

import (
	"encoding/json"
	"testing"
)

func TestPanelReadOnlyMetadataAndStructuredConfig(t *testing.T) {
	_, a, _ := managerFixture(t)
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/groups", `{"name":"test","comment":"service note","strip_prefix":"/api","timeout_overrides":{"stream_idle_timeout":"75s"}}`), 200)
	expectCode(t, adminRequest(a, "PUT", "/__linkgate/v1/groups/test/upstreams", `{"name":"primary","kind":"zt","headers":{"X-Preview":"value"},"probe":{"type":"tcp_connect","timeout":"50ms"}}`), 200)

	w := adminRequest(a, "GET", "/__linkgate/v1/status", "")
	expectCode(t, w, 200)
	var st struct {
		Groups []GroupState `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	g := st.Groups[0]
	if g.Comment != "service note" || g.Protocol != "http" || g.Upstreams[0].Kind != "zt" {
		t.Fatalf("missing display metadata: %+v", g)
	}

	w = adminRequest(a, "GET", "/__linkgate/v1/config", "")
	expectCode(t, w, 200)
	var cfg struct {
		YAML   string        `json:"yaml"`
		Groups []GroupConfig `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.YAML == "" || len(cfg.Groups) != 1 {
		t.Fatal("config response lost YAML or structured groups")
	}
	gcfg := cfg.Groups[0]
	if gcfg.StripPrefix != "/api" || gcfg.TimeoutOverrides.StreamIdleTimeout.String() != "1m15s" || gcfg.Upstreams[0].Headers["X-Preview"] != "value" || gcfg.Upstreams[0].Probe.Type != "tcp_connect" {
		t.Fatalf("structured editor missing advanced configuration: %+v", gcfg)
	}
}
