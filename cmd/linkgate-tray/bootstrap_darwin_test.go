//go:build darwin

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type darwinBootstrapFixture struct {
	tray                                  *tray
	state                                 *darwinBootstrapState
	kernel, config, agents, plist, marker string
	ops                                   darwinBootstrapOps
	live                                  darwinTrayAgent
	loaded                                bool
	calls                                 [][]string
	queries                               int
	now                                   time.Time
}

func newDarwinBootstrapFixture(t *testing.T, existing bool) *darwinBootstrapFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &darwinBootstrapFixture{kernel: filepath.Join(home, "Applications/Linkgate.app/Contents/MacOS/linkgate"),
		config: filepath.Join(home, "config.yaml"), agents: filepath.Join(home, "agents"),
		state: &darwinBootstrapState{}, loaded: existing, now: time.Now()}
	f.plist = filepath.Join(f.agents, "test.plist")
	f.marker = filepath.Join(home, ".test.install-state")
	f.live = darwinTrayAgent{Label: "test", ProgramArguments: []string{f.kernel, "-config", f.config}, PID: 123}
	f.tray = &tray{settings: localSettings{configPath: f.config}, cli: &client{}}
	for _, dir := range []string{filepath.Dir(f.kernel), f.agents} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.config, []byte("admin:\n  listen: 127.0.0.1:18766\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.ops = darwinBootstrapOps{read: os.ReadFile, health: func(*client) bool { return false },
		wait: func(*tray, string) error { return nil }, now: func() time.Time { return f.now }}
	f.ops.query = func(string) (darwinTrayAgent, bool, error) { f.queries++; return f.live, f.loaded, nil }
	f.ops.command = func(name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		if name == f.kernel {
			if err := os.WriteFile(f.plist, f.plistBytes(), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(f.marker, []byte("installed\n"), 0o600); err != nil {
				return nil, err
			}
		}
		f.loaded = true
		return nil, nil
	}
	if existing {
		if err := os.WriteFile(f.plist, f.plistBytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *darwinBootstrapFixture) plistBytes() []byte {
	return []byte(fmt.Sprintf(`<plist version="1.0"><dict><key>Label</key><string>test</string><key>ProgramArguments</key><array><string>%s</string><string>-config</string><string>%s</string></array></dict></plist>`, f.kernel, f.config))
}

func (f *darwinBootstrapFixture) ensure() (bool, error) {
	return ensureDarwinKernel(f.tray, f.state, f.kernel, "test", f.agents, f.ops)
}

func TestDarwinAdminDisabledNeverTouchesLaunchd(t *testing.T) {
	f := newDarwinBootstrapFixture(t, true)
	if err := os.WriteFile(f.config, []byte("admin:\n  disabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if started, err := f.ensure(); started || err == nil || !strings.Contains(err.Error(), "已禁用") {
			t.Fatalf("disabled: %v %v", started, err)
		}
		f.now = f.now.Add(time.Minute)
	}
	if len(f.calls) != 0 || f.queries != 0 {
		t.Fatal("disabled admin restarted service")
	}
}

func TestDarwinBootstrapInitializesOnlyOnce(t *testing.T) {
	f := newDarwinBootstrapFixture(t, false)
	if started, err := f.ensure(); !started || err != nil {
		t.Fatalf("init: %v %v", started, err)
	}
	for i := 0; i < 10; i++ {
		_, _ = f.ensure()
		f.now = f.now.Add(time.Minute)
	}
	if len(f.calls) != 1 || f.calls[0][0] != f.kernel || f.calls[0][3] != "-install" {
		t.Fatalf("reinstalled: %v", f.calls)
	}
	if f.state.failures != darwinRecoveryLimit || f.state.terminal == nil {
		t.Fatal("recovery has no upper bound")
	}
}

func TestDarwinFailedFirstInstallIsNotRepeated(t *testing.T) {
	f := newDarwinBootstrapFixture(t, false)
	f.ops.command = func(name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, args)
		return nil, errors.New("init failed")
	}
	for i := 0; i < 10; i++ {
		_, _ = f.ensure()
		f.now = f.now.Add(time.Minute)
	}
	if len(f.calls) != 1 || f.state.terminal == nil {
		t.Fatal("failed initialization retried")
	}
}

func TestDarwinUninstallIntentSurvivesTrayAndAppRestart(t *testing.T) {
	f := newDarwinBootstrapFixture(t, false)
	if _, err := f.ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.plist); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.marker, []byte("uninstalled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.loaded = false
	for _, state := range []*darwinBootstrapState{f.state, {}} {
		f.state = state
		if started, err := f.ensure(); started || err == nil || !strings.Contains(err.Error(), "已卸载") {
			t.Fatalf("uninstall: %v %v", started, err)
		}
	}
	if len(f.calls) != 1 {
		t.Fatal("uninstalled service installed again")
	}
}

func TestDarwinRemovedPlistNeverTriggersReinstall(t *testing.T) {
	f := newDarwinBootstrapFixture(t, true)
	_, _ = f.ensure()
	if err := os.Remove(f.plist); err != nil {
		t.Fatal(err)
	}
	f.loaded = false
	f.now = f.now.Add(time.Minute)
	if _, err := f.ensure(); err == nil {
		t.Fatal("removed plist ignored")
	}
	if len(f.calls) != 0 {
		t.Fatal("removed installation recreated")
	}
	f.state = &darwinBootstrapState{}
	if err := os.WriteFile(f.marker, []byte("installed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ensure(); err == nil || len(f.calls) != 0 {
		t.Fatal("persistent installed marker ignored")
	}
}

func TestDarwinRecoveryUsesOwnedLaunchdStateWithoutBootout(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "unloaded", true: "loaded-stopped"}[loaded], func(t *testing.T) {
			f := newDarwinBootstrapFixture(t, true)
			f.loaded, f.live.PID = loaded, 0
			if started, err := f.ensure(); !started || err != nil {
				t.Fatalf("recovery: %v %v", started, err)
			}
			want := "bootstrap"
			if loaded {
				want = "kickstart"
			}
			if len(f.calls) != 1 || f.calls[0][0] != "/bin/launchctl" || f.calls[0][1] != want {
				t.Fatalf("recovery: %v", f.calls)
			}
			for _, arg := range f.calls[0] {
				if arg == "-k" || arg == "bootout" || arg == "-install" {
					t.Fatal("destructive recovery")
				}
			}
		})
	}
}

func TestDarwinRecoveryStopsAfterThreeFailures(t *testing.T) {
	f := newDarwinBootstrapFixture(t, true)
	f.loaded = false
	f.ops.command = func(name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, args)
		return nil, errors.New("failed bootstrap")
	}
	for i := 0; i < 10; i++ {
		_, _ = f.ensure()
		f.now = f.now.Add(time.Minute)
	}
	if len(f.calls) != darwinRecoveryLimit || f.queries != darwinRecoveryLimit || !strings.Contains(f.state.terminal.Error(), "自动恢复已停止") {
		t.Fatal("unbounded recovery")
	}
}

func TestDarwinTrayRejectsForeignOwner(t *testing.T) {
	f := newDarwinBootstrapFixture(t, true)
	f.live.ProgramArguments[0] = filepath.Join(filepath.Dir(f.config), "foreign")
	if _, err := f.ensure(); err == nil || !strings.Contains(err.Error(), "其他安装") {
		t.Fatal("foreign live service accepted")
	}
	if len(f.calls) != 0 {
		t.Fatal("foreign service changed")
	}
}

func TestDarwinStableBundleRejectsTemporaryLocations(t *testing.T) {
	home := t.TempDir()
	for _, path := range []string{"/Volumes/Linkgate/Linkgate.app/Contents/MacOS/linkgate", "/private/var/folders/x/AppTranslocation/y/Linkgate.app/Contents/MacOS/linkgate", filepath.Join(home, "Linkgate.app/Contents/MacOS/linkgate")} {
		if stableDarwinBundle(path, home) {
			t.Fatalf("accepted %s", path)
		}
	}
	path, err := trayRealPath(filepath.Join(home, "Applications/Linkgate.app/Contents/MacOS/linkgate"))
	if err != nil || !stableDarwinBundle(path, home) {
		t.Fatalf("stable location rejected: %v", err)
	}
}

func TestBundledKernelOnlyInsideApp(t *testing.T) {
	if got := bundledKernel("/Applications/Linkgate.app/Contents/MacOS/linkgate-tray"); got != "/Applications/Linkgate.app/Contents/MacOS/linkgate" {
		t.Fatal(got)
	}
	if bundledKernel("/tmp/linkgate-tray") != "" {
		t.Fatal("standalone mistaken for app")
	}
}

func TestDarwinHealthDoesNotRequireToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__linkgate/v1/health" || r.Header.Get("X-Linkgate-Token") != "" {
			t.Error("unexpected health request")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	if !kernelReachable(&client{base: server.URL, http: &http.Client{Timeout: time.Second}}) {
		t.Fatal("responding server treated as dead")
	}
}

func TestStandaloneDarwinTrayNeverInstallsKernel(t *testing.T) {
	started, err := bootstrapDarwinKernel(&tray{})
	if err != nil || started {
		t.Fatalf("standalone: %v %v", started, err)
	}
}

func TestDarwinTrayLaunchctlListParsesSemicolonArraySyntax(t *testing.T) {
	sample := `{
"Label" = "test";
"OnDemand" = false;
"LastExitStatus" = -15;
"PID" = 321;
"ProgramArguments" = (
"/Applications/Linkgate Test;.app/Contents/MacOS/linkgate";
"-config";
"/tmp/config\"quoted.yaml";
);
};`
	identity, err := decodeDarwinTrayList([]byte(sample))
	if err != nil || identity.PID != 321 || identity.Label != "test" || len(identity.ProgramArguments) != 3 {
		t.Fatalf("list syntax: %v", err)
	}
	_, loaded, err := queryDarwinTrayAgent("test", func(string, ...string) ([]byte, error) { return []byte(sample), nil })
	if err != nil || !loaded {
		t.Fatal("query did not use list parser")
	}
	for _, invalid := range []string{
		strings.Replace(sample, `"PID" = 321;`, `"PID" = false;`, 1),
		strings.Replace(sample, `"-config";`, `2;`, 1),
		strings.Replace(sample, `"Label" = "test";`, `"Label" = "test"; "Label" = "other";`, 1),
		sample + "garbage",
	} {
		if _, err := decodeDarwinTrayList([]byte(invalid)); err == nil {
			t.Fatal("accepted ambiguous list output")
		}
	}
}

func TestSettingsRefreshPreservesAdminOverride(t *testing.T) {
	t.Setenv("LINKGATE_TOKEN", "temporary-test-token")
	s := loadSettings(filepath.Join(t.TempDir(), "missing.yaml"), "127.0.0.1:18766")
	if fresh := loadSettings(s.configPath, s.adminOverride); fresh.adminAddr != "127.0.0.1:18766" {
		t.Fatal("admin override lost")
	}
}
