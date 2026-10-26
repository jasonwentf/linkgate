package app

import (
	"strings"
	"testing"
)

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	const valid = `version: 1
admin:
  disabled: true
retry:
  mode: connect
groups:
  - name: first
    listen: "127.0.0.1:18765"
    upstreams:
      - name: primary
        url: "http://upstream.invalid:8080"
  - name: second
    listen: "127.0.0.1:18766"
    upstreams:
      - name: primary
        url: "http://upstream.invalid:8080"
`
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatalf("valid control configuration rejected: %v", err)
	}
	cases := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{"duplicate group names", "name: second", "name: first", "group 名称重复"},
		{"duplicate listen ports", "127.0.0.1:18766", "127.0.0.1:18765", "冲突"},
		{"invalid retry mode", "mode: connect", "mode: always", "retry.mode"},
		{"unsupported URL scheme", "http://upstream.invalid:8080", "ftp://upstream.invalid", "http/https"},
		{"missing URL host", "http://upstream.invalid:8080", "http:///missing-host", "主机名"},
		{"missing URL hostname with port", "http://upstream.invalid:8080", "http://:8080", "主机名"},
		{"malformed URL", "http://upstream.invalid:8080", "http://%zz", "url 解析失败"},
		{"non-numeric URL port", "http://upstream.invalid:8080", "http://upstream.invalid:bad", "url 解析失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(valid, tc.old, tc.replacement, 1)
			_, err := Parse([]byte(input))
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Parse error = %v, want error containing %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateRejectsAdminPortCollision(t *testing.T) {
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://upstream.invalid"})
	cfg.Groups[0].Listen = cfg.Admin.Listen
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("Validate error = %v, want admin listen collision", err)
	}
}
