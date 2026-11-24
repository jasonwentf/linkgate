//go:build windows

package main

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"linkgate/internal/app"
)

func TestWindowsDiagnosticOutputEncoding(t *testing.T) {
	const text = "错误：拒绝访问\r\n详细中文信息"
	if decodeWindowsOutput([]byte(text)) != text {
		t.Fatal("UTF-8 output changed")
	}
	gbk, err := hex.DecodeString("b4edcef3a3babedcbef8b7c3ceca0d0acfeacfb8d6d0cec4d0c5cfa2")
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeWindowsCodepage(gbk, 936); got != text {
		t.Fatalf("CP936 output garbled: %q", got)
	}
}

func TestWindowsNativeCommandLine(t *testing.T) {
	const config = `C:\用户 & space\config.yaml`
	for _, command := range []string{`"C:\Linkgate\linkgate.exe" -config "` + config + `"`, `C:\Linkgate\linkgate.exe -config="` + config + `"`} {
		args, err := parseWindowsCommandLine(command)
		if err != nil {
			t.Fatal(err)
		}
		if got := configFromWindowsArgs(args, "default.yaml"); got != config {
			t.Fatalf("%s: %s", command, got)
		}
	}
	if _, err := parseWindowsCommandLine(""); err == nil {
		t.Fatal("accepted unreadable command line")
	}
}

func TestUnmarshalWindowsStateThroughCLIXML(t *testing.T) {
	clean := `{"TrayTarget":"","SID":"S-1-5-21-x","Processes":[],"Task":null}`
	var want windowsState
	if err := unmarshalWindowsState([]byte(clean), &want); err != nil {
		t.Fatal(err)
	}
	for _, wrap := range []string{
		"#< CLIXML\r\n" + clean,
		"#< CLIXML\r\n" + clean + "\r\n<Objs Version=\"1.1.0.1\"></Objs>",
		clean + "<Objs></Objs>",
	} {
		var got windowsState
		if err := unmarshalWindowsState([]byte(wrap), &got); err != nil || got.SID != want.SID {
			t.Fatalf("%q: %v %+v", wrap, err, got)
		}
	}
}

func TestWindowsHealthyInstanceConfig(t *testing.T) {
	const config = `C:\用户\custom config.yaml`
	path := config
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Linkgate-Token") != "test-only-token" {
			t.Error("missing local token")
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"config_path":%q}`, path)
	}))
	defer server.Close()
	cfg := app.Default()
	cfg.Admin.Listen = strings.TrimPrefix(server.URL, "http://")
	cfg.Admin.TokenFile = filepath.Join(t.TempDir(), "test.token")
	if err := os.WriteFile(cfg.Admin.TokenFile, []byte("test-only-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if !healthyWindowsKernel(cfg, config) {
		t.Fatal("same-config healthy instance not reusable")
	}
	path = `C:\other.yaml`
	if healthyWindowsKernel(cfg, config) {
		t.Fatal("different runtime config accepted")
	}
	path = config
	status = http.StatusUnauthorized
	if healthyWindowsKernel(cfg, config) {
		t.Fatal("unauthenticated/unhealthy instance accepted")
	}
	cfg.Admin.Disabled = true
	if healthyWindowsKernel(cfg, config) {
		t.Fatal("health cannot be inferred with admin disabled")
	}
}
