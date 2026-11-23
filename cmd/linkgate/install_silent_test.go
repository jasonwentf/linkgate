package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readSilentInstallerSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Pascal/PowerShell 的真实执行由 CI 的安装器 E2E 检查；这里防止回退到只拷文件的接线。
func TestSilentInstallerTaskAndLaunchContract(t *testing.T) {
	iss := readSilentInstallerSource(t, "deploy/windows/linkgate.iss")
	for _, want := range []string{"if WizardSilent then Selected := SilentTaskSelected(Task)", "'/TASKS='", "HasTasks := True", "TaskListSelection(Tasks, Task, False)", "(Lowercase(Task) = 'autostart') or (Lowercase(Task) = 'tray')", "Check: CreateTrayIcon", "-OpenPanel false", "-OpenPanel true", "GetCustomSetupExitCode"} {
		if !strings.Contains(iss, want) {
			t.Fatalf("missing silent-install contract: %s", want)
		}
	}
	silentRuns, guiRuns := 0, 0
	for _, line := range strings.Split(iss, "\n") {
		if !strings.HasPrefix(line, "Filename:") {
			continue
		}
		if strings.Contains(line, "{code:SilentLaunchParameters}") {
			silentRuns++
			if strings.Contains(line, "postinstall") || strings.Contains(line, "skipifsilent") || !strings.Contains(line, "Check: WizardSilent") || !strings.Contains(line, "waituntilterminated") {
				t.Fatal("silent startup must run automatically, not as a skipped finish-page action")
			}
		} else if strings.Contains(line, "{code:LaunchParameters}") {
			guiRuns++
			if !strings.Contains(line, "postinstall") || !strings.Contains(line, "skipifsilent") {
				t.Fatal("GUI panel launch must remain opt-in on the finish page")
			}
		}
	}
	if silentRuns != 1 || guiRuns != 1 {
		t.Fatalf("silent runs=%d GUI runs=%d", silentRuns, guiRuns)
	}
	helper := readSilentInstallerSource(t, "deploy/windows/setup-helper.ps1")
	if !strings.Contains(helper, "$OpenPanel = 'false'") || !strings.Contains(helper, "-install-start=true") || !strings.Contains(helper, `"-install-open-panel=$OpenPanel"`) || strings.Contains(helper, "-install-open-panel=true") {
		t.Fatal("helper must separate process startup from browser opening")
	}
}

func TestSilentInstallerSmokeRunsBeforePublishing(t *testing.T) {
	workflow := readSilentInstallerSource(t, ".github/workflows/release-windows.yml")
	var cfg struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				ID   string `yaml:"id"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &cfg); err != nil {
		t.Fatal(err)
	}
	compile, smoke, publish := -1, -1, -1
	for i, step := range cfg.Jobs["installer"].Steps {
		if strings.Contains(step.Run, "& $iscc") {
			compile = i
		}
		if step.ID == "smoke" {
			smoke = i
			if !strings.Contains(step.Run, "pwsh -NoProfile -File deploy/windows/installer_smoke_test.ps1") || !strings.Contains(step.Run, "$LASTEXITCODE -ne 0") {
				t.Fatal("real E2E must run in a clean PowerShell process and enforce its exit code")
			}
		}
		if strings.Contains(step.Run, "gh release upload") {
			publish = i
		}
	}
	if compile < 0 || smoke <= compile || publish <= smoke {
		t.Fatalf("compile=%d smoke=%d publish=%d", compile, smoke, publish)
	}
	script := readSilentInstallerSource(t, "deploy/windows/installer_smoke_test.ps1")
	for _, want := range []string{"$env:GITHUB_ACTIONS -ne 'true'", "/VERYSILENT /NORESTART", "Principal.LogonType -ne 'S4U'", "install-state.json", "StatusCode -eq 200", "Linkgate Tray.lnk", "Get-LinkgateProcesses $installDir 'linkgate-tray'", "'/TASKS=autostart'", "'/TASKS=tray'", `'/TASKS=""'`, "defaults-after-overrides"} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing real-installer regression assertion: %s", want)
		}
	}
}
