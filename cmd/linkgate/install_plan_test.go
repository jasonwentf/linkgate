package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"linkgate/internal/app"
)

func TestWindowsTaskXML(t *testing.T) {
	const exe = `C:\用户 & apps\Linkgate\linkgate.exe`
	const config = `C:\用户 & apps\custom config.yaml`
	const dir = `C:\用户 & apps\Linkgate`
	for _, autostart := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "logon"}[autostart], func(t *testing.T) {
			text := windowsTaskXML("S-1-5-21-test", exe, config, dir, autostart)
			var task struct {
				Principals struct {
					Principal struct {
						UserID    string `xml:"UserId"`
						LogonType string
						RunLevel  string
					}
				}
				Triggers struct {
					LogonTrigger []struct {
						UserID string `xml:"UserId"`
					}
				}
				Settings struct {
					MultipleInstancesPolicy string
					ExecutionTimeLimit      string
					RestartOnFailure        struct {
						Interval string
						Count    int
					}
				}
				Actions struct {
					Exec struct {
						Command          string
						Arguments        string
						WorkingDirectory string
					}
				}
			}
			decoder := xml.NewDecoder(strings.NewReader(text))
			decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
				if strings.EqualFold(charset, "UTF-16") {
					return input, nil
				}
				return nil, fmt.Errorf("unexpected charset %s", charset)
			}
			if err := decoder.Decode(&task); err != nil {
				t.Fatal(err)
			}
			p := task.Principals.Principal
			if p.LogonType != "S4U" || p.UserID != "S-1-5-21-test" || p.RunLevel != "LeastPrivilege" {
				t.Fatalf("principal: %+v", p)
			}
			if (len(task.Triggers.LogonTrigger) == 1) != autostart {
				t.Fatal("logon trigger did not follow option")
			}
			if autostart && task.Triggers.LogonTrigger[0].UserID != p.UserID {
				t.Fatal("logon belongs to another user")
			}
			a := task.Actions.Exec
			if a.Command != exe || a.Arguments != `-config "`+config+`"` || a.WorkingDirectory != dir {
				t.Fatalf("escaping: %+v", a)
			}
			if task.Settings.RestartOnFailure.Count != 255 || task.Settings.RestartOnFailure.Interval != "PT1M" || task.Settings.ExecutionTimeLimit != "PT0S" || task.Settings.MultipleInstancesPolicy != "IgnoreNew" {
				t.Fatal("unexpected lifecycle settings")
			}
			if strings.Contains(text, "InteractiveToken") || strings.Contains(text, "Password") {
				t.Fatal("interactive/password task")
			}
		})
	}
}

func TestWindowsInstallIsolation(t *testing.T) {
	exe := `C:\Linkgate\linkgate.exe`
	if err := verifyWindowsTask(nil, "sid", exe); err != nil {
		t.Fatal(err)
	}
	task := &windowsTask{UserID: "sid", Execute: `c:/linkgate/LINKGATE.EXE`, ActionCount: 1}
	if err := verifyWindowsTask(task, "sid", exe); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []windowsTask{{UserID: "other", Execute: exe, ActionCount: 1}, {UserID: "sid", Execute: `C:\Downloads\linkgate.exe`, ActionCount: 1}, {UserID: "sid", Execute: exe, ActionCount: 2}} {
		if verifyWindowsTask(&invalid, "sid", exe) == nil {
			t.Fatalf("accepted foreign task: %+v", invalid)
		}
	}
	const config = `C:\用户\custom config.yaml`
	if running, err := classifyWindowsProcesses([]windowsProcess{{PID: 1, Path: exe}}, 1, exe, config); err != nil || running {
		t.Fatal("installer counted itself")
	}
	if running, err := classifyWindowsProcesses([]windowsProcess{{PID: 2, Path: exe, ConfigPath: config}}, 1, exe, config); err != nil || !running {
		t.Fatalf("healthy-instance candidate not reusable: %v", err)
	}
	for _, process := range []windowsProcess{{PID: 2, Path: exe, ConfigPath: `C:\other.yaml`}, {PID: 2, Path: exe}, {PID: 2, Path: `C:\Downloads\linkgate.exe`, ConfigPath: config}, {PID: 2}} {
		if _, err := classifyWindowsProcesses([]windowsProcess{process}, 1, exe, config); err == nil {
			t.Fatal("foreign/different-config/inaccessible process accepted")
		}
	}
	if sameWindowsPath("", "") {
		t.Fatal("unknown executable paths must not match")
	}
}

func TestWindowsProcessConfigArguments(t *testing.T) {
	const config = `C:\用户\custom config.yaml`
	for _, args := range [][]string{{"linkgate.exe", "-config", config}, {"linkgate.exe", "-config=" + config}, {"linkgate.exe", "--config", config}, {"linkgate.exe", "-config", "old.yaml", "-config=" + config}, {"linkgate.exe"}} {
		if got := configFromWindowsArgs(args, config); got != config {
			t.Fatalf("%v: %q", args, got)
		}
	}
	for _, args := range [][]string{nil, {"linkgate.exe", "-config"}, {"linkgate.exe", "-config="}} {
		if configFromWindowsArgs(args, config) != "" {
			t.Fatal("accepted unknown/empty process config")
		}
	}
}

func TestExampleSafeFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeExample(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Groups) != 0 {
		t.Fatal("first run must not probe fake upstreams")
	}
	if cfg.Log.File != "~/.linkgate/kernel.log" {
		t.Fatalf("log file: %s", cfg.Log.File)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "axonhub") || !strings.Contains(string(before), "192.0.2.1") {
		t.Fatal("non-neutral example")
	}
	if writeExample(path) == nil {
		t.Fatal("existing config overwritten")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("config changed")
	}
}

func TestReferenceExampleDisabled(t *testing.T) {
	cfg, err := app.Load(filepath.Join("..", "..", "configs", "linkgate.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.File == "" {
		t.Fatal("reference logging disabled")
	}
	for _, g := range cfg.Groups {
		for _, u := range g.Upstreams {
			if u.IsEnabled() {
				t.Fatalf("placeholder %s enabled", u.Name)
			}
		}
	}
}
