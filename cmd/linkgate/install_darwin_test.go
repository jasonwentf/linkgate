//go:build darwin

package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type darwinInstallFixture struct {
	home, config, binary, agents, plist, state string
	ops                                        darwinInstallOps
	loaded                                     bool
	live                                       darwinAgentIdentity
	calls                                      [][]string
}

func newDarwinInstallFixture(t *testing.T, existing bool) *darwinInstallFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &darwinInstallFixture{home: home, config: filepath.Join(home, "settings & test", "config.yaml"),
		binary: filepath.Join(home, "Applications", "Linkgate & Test.app", "Contents", "MacOS", "linkgate"),
		agents: filepath.Join(home, "agents"), loaded: existing}
	f.plist = filepath.Join(f.agents, "test.plist")
	f.state = filepath.Join(filepath.Dir(f.config), ".test.install-state")
	f.live = darwinAgentIdentity{Label: "test", ProgramArguments: []string{f.binary, "-config", f.config}, PID: 123}
	for _, dir := range []string{filepath.Dir(f.binary), filepath.Dir(f.config), f.agents} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.binary, []byte("test binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.ops = darwinInstallOps{read: os.ReadFile, write: writeDarwinFile, remove: os.Remove}
	f.ops.query = func(string) (darwinAgentIdentity, bool, error) { return f.live, f.loaded, nil }
	f.ops.command = func(name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		if name == f.binary && args[len(args)-1] == "-init" {
			return nil, os.WriteFile(f.config, []byte("keep config"), 0o600)
		}
		if name == "/bin/launchctl" {
			if args[0] == "bootout" {
				f.loaded = false
			}
			if args[0] == "bootstrap" {
				f.loaded = true
			}
		}
		return nil, nil
	}
	if existing {
		if err := os.WriteFile(f.config, []byte("keep config"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := append(darwinAgentPlist(f.binary, f.config, f.home, "test", filepath.Join(f.home, "old.log")), '\n')
		if err := os.WriteFile(f.plist, old, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.state, []byte("installed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *darwinInstallFixture) install(uninstall bool) error {
	return installDarwinAgent(f.config, f.binary, f.home, "test", f.agents, uninstall, f.ops)
}

func TestDarwinFirstInstallInitializesChecksWithoutBootout(t *testing.T) {
	f := newDarwinInstallFixture(t, false)
	if err := f.install(false); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || f.calls[0][3] != "-init" || f.calls[1][3] != "-check" || f.calls[2][1] != "bootstrap" {
		t.Fatalf("calls: %v", f.calls)
	}
	data, err := os.ReadFile(f.plist)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := decodeDarwinAgent(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDarwinOwner(identity, f.binary, f.config, "test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Linkgate &amp; Test.app") {
		t.Fatal("XML escaping missing")
	}
	if st, err := os.Stat(f.plist); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("plist permissions")
	}
	if marker, err := os.ReadFile(f.state); err != nil || string(marker) != "installed\n" {
		t.Fatal("missing installed state")
	}
}

func TestDarwinExistingInstallRejectsOtherBinaryOrConfig(t *testing.T) {
	for _, foreign := range []string{"binary", "config", "live"} {
		t.Run(foreign, func(t *testing.T) {
			f := newDarwinInstallFixture(t, true)
			if foreign == "live" {
				f.live.ProgramArguments[0] = filepath.Join(f.home, "foreign-linkgate")
			} else {
				binary, config := f.binary, f.config
				if foreign == "binary" {
					binary = filepath.Join(f.home, "foreign-linkgate")
				}
				if foreign == "config" {
					config = filepath.Join(f.home, "foreign.yaml")
				}
				if err := os.WriteFile(f.plist, darwinAgentPlist(binary, config, f.home, "test", "/tmp/log"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(f.plist)
			for _, uninstall := range []bool{false, true} {
				if err := f.install(uninstall); err == nil || !strings.Contains(err.Error(), "其他安装") {
					t.Fatalf("conflict: %v", err)
				}
			}
			after, _ := os.ReadFile(f.plist)
			if len(f.calls) != 0 || !f.loaded || string(before) != string(after) {
				t.Fatal("foreign install changed")
			}
		})
	}
}

func TestDarwinCheckFailurePreservesRunningInstall(t *testing.T) {
	f := newDarwinInstallFixture(t, true)
	before, _ := os.ReadFile(f.plist)
	f.ops.command = func(name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, args)
		return nil, errors.New("invalid config")
	}
	if err := f.install(false); err == nil {
		t.Fatal("accepted invalid config")
	}
	after, _ := os.ReadFile(f.plist)
	if !f.loaded || string(before) != string(after) || len(f.calls) != 1 || f.calls[0][2] != "-check" {
		t.Fatal("stopped before validation")
	}
}

func TestDarwinInstallRollsBackWriteAndBootstrapFailures(t *testing.T) {
	for _, failure := range []string{"write", "bootstrap", "state"} {
		t.Run(failure, func(t *testing.T) {
			f := newDarwinInstallFixture(t, true)
			before, _ := os.ReadFile(f.plist)
			command, write := f.ops.command, f.ops.write
			failed := false
			f.ops.write = func(path string, data []byte, mode os.FileMode) error {
				if !failed && ((failure == "write" && path == f.plist) || (failure == "state" && path == f.state)) {
					failed = true
					return errors.New("injected write failure")
				}
				return write(path, data, mode)
			}
			f.ops.command = func(name string, args ...string) ([]byte, error) {
				if failure == "bootstrap" && !failed && name == "/bin/launchctl" && args[0] == "bootstrap" {
					failed = true
					return nil, errors.New("injected bootstrap failure")
				}
				return command(name, args...)
			}
			if err := f.install(false); err == nil || !strings.Contains(err.Error(), "已恢复") {
				t.Fatalf("rollback: %v", err)
			}
			after, _ := os.ReadFile(f.plist)
			marker, _ := os.ReadFile(f.state)
			if !f.loaded || string(after) != string(before) || string(marker) != "installed\n" {
				t.Fatal("old service or files not restored")
			}
		})
	}
}

func TestDarwinUninstallRecordsIntentKeepsConfigAndRollsBackFailure(t *testing.T) {
	for _, failRemove := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failRemove], func(t *testing.T) {
			f := newDarwinInstallFixture(t, true)
			remove := f.ops.remove
			f.ops.remove = func(path string) error {
				if path == f.plist && failRemove {
					return errors.New("injected removal failure")
				}
				return remove(path)
			}
			err := f.install(true)
			if (err != nil) != failRemove {
				t.Fatalf("uninstall: %v", err)
			}
			config, _ := os.ReadFile(f.config)
			marker, _ := os.ReadFile(f.state)
			want := "uninstalled\n"
			if failRemove {
				want = "installed\n"
			}
			if string(config) != "keep config" || string(marker) != want || f.loaded != failRemove {
				t.Fatal("uninstall state incorrect")
			}
		})
	}
}

func TestDarwinRejectsUnstablePathsAndSymlinkEscapes(t *testing.T) {
	f := newDarwinInstallFixture(t, false)
	if err := darwinStableBinary(f.binary, f.home); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/Volumes/Linkgate/Linkgate.app/Contents/MacOS/linkgate", "/private/var/folders/x/AppTranslocation/y/Linkgate.app/Contents/MacOS/linkgate", filepath.Join(f.home, "temporary/linkgate")} {
		if err := darwinStableBinary(path, f.home); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	escape := filepath.Join(f.home, "Applications", "Escaped.app")
	if err := os.Symlink(filepath.Join(f.home, "temporary"), escape); err != nil {
		t.Fatal(err)
	}
	real, err := darwinRealPath(filepath.Join(escape, "Contents/MacOS/linkgate"))
	if err != nil {
		t.Fatal(err)
	}
	if err := darwinStableBinary(real, f.home); err == nil {
		t.Fatal("accepted symlink escape")
	}
}

func TestDarwinExplicitWindowsFlagsAreRejected(t *testing.T) {
	oldFlags, oldOptions := flag.CommandLine, windowsInstall
	t.Cleanup(func() { flag.CommandLine, windowsInstall = oldFlags, oldOptions })
	windowsInstall.start, windowsInstall.dryrun = true, false
	for _, arg := range []string{"-dryrun", "-install-start=false", "-install-start=true", "-install-autostart=false", "-install-tray=false", "-install-open-panel"} {
		flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
		for _, name := range []string{"dryrun", "install-start", "install-autostart", "install-tray", "install-open-panel"} {
			flag.CommandLine.Bool(name, false, "")
		}
		if err := flag.CommandLine.Parse([]string{arg}); err != nil {
			t.Fatal(err)
		}
		if err := darwinInstallFlagError(); err == nil || !strings.Contains(err.Error(), "不支持") {
			t.Fatalf("accepted %s", arg)
		}
	}
}

func TestDarwinConditionalInitializationPreservesExistingAndUninstalledState(t *testing.T) {
	f := newDarwinInstallFixture(t, true)
	f.ops.initialOnly = true
	before, _ := os.ReadFile(f.plist)
	if err := f.install(false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.plist)
	if len(f.calls) != 0 || !f.loaded || string(before) != string(after) {
		t.Fatal("conditional initialization replaced an existing service")
	}
	if err := os.Remove(f.plist); err != nil {
		t.Fatal(err)
	}
	f.loaded = false
	if err := os.WriteFile(f.state, []byte("uninstalled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.install(false); err == nil || len(f.calls) != 0 {
		t.Fatal("conditional initialization ignored uninstall intent")
	}
}

func TestDarwinPermanentWriteFailureStillRestartsUnchangedOldPlist(t *testing.T) {
	f := newDarwinInstallFixture(t, true)
	write := f.ops.write
	f.ops.write = func(path string, data []byte, mode os.FileMode) error {
		if path == f.plist {
			return errors.New("read-only agent directory")
		}
		return write(path, data, mode)
	}
	if err := f.install(false); err == nil || !strings.Contains(err.Error(), "已恢复") || !f.loaded {
		t.Fatalf("unchanged old service not restored: %v", err)
	}
}

func TestDarwinLaunchctlListParsesActualSemicolonArraySyntax(t *testing.T) {
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
	identity, err := decodeDarwinList([]byte(sample))
	if err != nil || identity.PID != 321 || identity.Label != "test" || len(identity.ProgramArguments) != 3 || identity.ProgramArguments[2] != "/tmp/config\"quoted.yaml" {
		t.Fatalf("list syntax: %v", err)
	}
	_, loaded, err := queryDarwinAgent("test", func(string, ...string) ([]byte, error) { return []byte(sample), nil })
	if err != nil || !loaded {
		t.Fatal("query did not use list parser")
	}
	for _, invalid := range []string{
		strings.Replace(sample, `"PID" = 321;`, `"PID" = false;`, 1),
		strings.Replace(sample, `"-config";`, `2;`, 1),
		strings.Replace(sample, `"Label" = "test";`, `"Label" = "test"; "Label" = "other";`, 1),
		sample + "garbage",
	} {
		if _, err := decodeDarwinList([]byte(invalid)); err == nil {
			t.Fatal("accepted ambiguous list output")
		}
	}
}

func TestDarwinProductionLabelCannotBeChangedByEnvironment(t *testing.T) {
	t.Setenv("LINKGATE_LAUNCHD_LABEL", "foreign-label")
	t.Setenv("LINKGATE_LAUNCHAGENTS_DIR", t.TempDir())
	if darwinLaunchdLabel != "dev.linkgate.kernel" || darwinLaunchagentsDir != "" {
		t.Fatal("environment changed production dependencies")
	}
}
