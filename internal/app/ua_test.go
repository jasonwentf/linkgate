package app

import (
	"fmt"
	"testing"
)

func TestUAClassification(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want string
	}{
		// AI harnesses and coding CLIs.
		{"zcode", "zcode/1.2.0", "zcode"},
		{"zcode mixed case", "ZCode/1.2 (darwin; arm64)", "zcode"},
		{"deepseek harness", "deepseek-harness/0.1.0", "dsh"},
		{"deepseek harness as dsh", "dsh/0.1.0", "dsh"},
		{"claude code", "claude-code/1.0.4", "claude-code"},
		{"claude cli", "claude-cli/0.9", "claude-code"},
		{"codex", "codex/1.0", "codex"},
		{"openai codex", "openai-codex/0.2", "codex"},
		{"opencode", "opencode/0.5.1", "opencode"},
		{"kiro", "kiro/1.0", "kiro"},
		{"devin", "devin/1.0", "devin"},
		{"kelivo", "kelivo/2.0", "kelivo"},
		// SillyTavern reports through node-fetch.
		{"node fetch", "node-fetch/1.0 (+https://github.com/bitinn/node-fetch)", "sillytavern"},
		{"sillytavern", "SillyTavern/1.12.0", "sillytavern"},
		// Browsers: full UAs must resolve despite shared fingerprints.
		{"chrome", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36", "chrome"},
		{"chrome ios", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/122.0.6261.69 Mobile/15E148 Safari/604.1", "chrome"},
		{"edge", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36 Edg/122.0.0.0", "edge"},
		{"firefox", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:123.0) Gecko/20100101 Firefox/123.0", "firefox"},
		{"firefox ios", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/123.0 Mobile/15E148 Safari/605.1.15", "firefox"},
		{"safari", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.3 Safari/605.1.15", "safari"},
		// HTTP libraries and CLI fetchers.
		{"curl", "curl/8.4.0", "curl"},
		{"wget", "Wget/1.21.4", "wget"},
		{"python requests", "python-requests/2.31.0", "python-requests"},
		{"python httpx", "python-httpx/0.27.0", "python"},
		{"python aiohttp", "Python/3.12 aiohttp/3.9.1", "python"},
		{"go http client", "Go-http-client/1.1", "go-http-client"},
		{"okhttp", "okhttp/4.12.0", "okhttp"},
		// Blank vs unrecognized are different buckets.
		{"empty", "", "unknown"},
		{"blank", "   \t ", "unknown"},
		{"unknown", "SomeRandomApp/2.0", "other"},
		{"mozilla only", "Mozilla/5.0", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uaClass(tc.ua); got != tc.want {
				t.Fatalf("uaClass(%q) = %q, want %q", tc.ua, got, tc.want)
			}
		})
	}
}

// TestUAClassCardinalityIsBounded proves unknown UAs never create new buckets.
func TestUAClassCardinalityIsBounded(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		seen[uaClass(fmt.Sprintf("private-client-%d/1.0", i))] = true
		if got := uaClass(fmt.Sprintf("Mozilla/5.0 private-agent-%d", i)); got != uaOther {
			t.Fatalf("unexpected class %q", got)
		}
	}
	if len(seen) != 1 || !seen[uaOther] {
		t.Fatalf("unknown UAs should collapse into %q, got %v", uaOther, seen)
	}
}

func TestUAClassesComeFromTable(t *testing.T) {
	allowed := map[string]bool{uaUnknown: true, uaOther: true}
	for _, rule := range uaClassRules {
		allowed[rule.Class] = true
	}
	for _, ua := range []string{"curl/8.0", "Mozilla/5.0 Chrome/1 Safari/1", "arbitrary/1"} {
		if class := uaClass(ua); !allowed[class] {
			t.Fatalf("class %q from %q is outside the rule table", class, ua)
		}
	}
}

func TestRecordEmptyUAAsUnknown(t *testing.T) {
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://127.0.0.1:1"})
	store := NewStore(cfg, Thresholds{})
	store.RecordRequestEnd(cfg, "test", "", "primary", true, false)
	st := store.ServiceStats(store.Snapshot(cfg))[0]
	if len(st.UALines) != 1 || st.UALines[0].UA != uaUnknown {
		t.Fatalf("empty UA should record as %q: %+v", uaUnknown, st.UALines)
	}
}
