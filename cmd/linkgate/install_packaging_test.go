package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWindowsPackagingContract(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Archives []struct {
			ID        string   `yaml:"id"`
			Formats   []string `yaml:"formats"`
			Overrides []struct {
				OS      string   `yaml:"goos"`
				Formats []string `yaml:"formats"`
			} `yaml:"format_overrides"`
			Files []yaml.Node `yaml:"files"`
		} `yaml:"archives"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Archives) != 2 {
		t.Fatalf("expected two platform archives: %d", len(cfg.Archives))
	}
	for _, archive := range cfg.Archives {
		formats := func(os string) string {
			for _, override := range archive.Overrides {
				if override.OS == os {
					return strings.Join(override.Formats, ",")
				}
			}
			return strings.Join(archive.Formats, ",")
		}
		if archive.ID == "windows-pkg" {
			if formats("windows") != "zip" || formats("darwin") != "none" || formats("linux") != "none" {
				t.Fatal("Windows archive includes another OS")
			}
		} else if archive.ID == "unix-pkg" {
			if formats("windows") != "none" || formats("darwin") != "tar.gz" || formats("linux") != "tar.gz" {
				t.Fatal("Unix archive includes Windows")
			}
		} else {
			t.Fatalf("unexpected archive %s", archive.ID)
		}
		for _, node := range archive.Files {
			source := node.Value
			if node.Kind == yaml.MappingNode {
				var file struct {
					Source string `yaml:"src"`
				}
				if err := node.Decode(&file); err != nil {
					t.Fatal(err)
				}
				source = file.Source
			}
			files, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(source)))
			if err != nil || len(files) == 0 {
				t.Fatalf("missing archive source %s", source)
			}
			for _, file := range files {
				if strings.HasSuffix(file, "_test.ps1") || strings.HasSuffix(file, "linkgate.iss") || strings.HasSuffix(file, "setup-helper.ps1") {
					t.Fatalf("developer/installer-only file included in ZIP: %s", file)
				}
				if archive.ID == "windows-pkg" && strings.HasSuffix(file, ".service") {
					t.Fatal("Unix service in Windows ZIP")
				}
				if archive.ID == "unix-pkg" && (strings.HasSuffix(file, ".ps1") || strings.HasSuffix(file, ".bat")) {
					t.Fatal("Windows script in Unix archive")
				}
			}
		}
	}
}

// 这里只检查向导的结构与关键契约；真正 Pascal/Inno 编译由 windows-latest 的 ISCC 执行。
func TestInnoInstallerStaticContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "windows", "linkgate.iss"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"PrivilegesRequired=lowest", `DefaultDirName={localappdata}\Linkgate`, "AppId={{", "OutputBaseFilename=Linkgate-Setup-{#MyAppVersion}-win64", "[Tasks]", `Name: "autostart"`, `Name: "tray"`, "立即启动 Linkgate 并打开面板", "DisableDirPage=yes", "UsePreviousAppDir=no", "CurUninstallStepChanged", "-Phase Uninstall", "if FileExists(ExpandConstant('{app}\\linkgate.exe'))", "PrepareToInstall", "ewWaitUntilTerminated", "CurStep = ssInstall", "DeinitializeSetup", "{code:TrayArguments}"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing Inno contract: %s", want)
		}
	}
	if strings.Contains(text, "\n[UninstallRun]\n") {
		t.Fatal("uninstall must have only one checked event entry")
	}
	if strings.Contains(text, "CurStep = ssPostInstall then begin") {
		t.Fatal("critical configuration must not run in the nonfatal post-install hook")
	}
	if strings.Contains(text, "unchecked") {
		t.Fatal("installer defaults must be checked")
	}
	icon := filepath.Join("..", "..", "cmd", "linkgate-tray", "icons", "tray-win.ico")
	if _, err := os.Stat(icon); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsInstallerTransactionContract(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	helper := read("deploy/windows/setup-helper.ps1")
	start := strings.Index(helper, "if ($Phase -eq 'Prepare')")
	end := strings.Index(helper, "} elseif ($Phase -eq 'Rollback')")
	if start < 0 || end <= start {
		t.Fatal("missing read-only preparation phase")
	}
	prepare := helper[start:end]
	if !strings.Contains(prepare, "New-LinkgateSnapshot") || strings.Contains(prepare, "Stop-LinkgateInstallation") || strings.Contains(prepare, "Remove-Item") || strings.Contains(prepare, "Unregister-ScheduledTask") {
		t.Fatal("preparation changed live startup state")
	}
	if !strings.Contains(helper, "Restore-LinkgateSnapshot $InstallDir $SnapshotDir") {
		t.Fatal("configuration failure lacks rollback")
	}
	transaction := read("deploy/windows/transaction.ps1")
	for _, want := range []string{"Export-ScheduledTask", "task.xml", "tray.lnk", "install-state.json", "TaskEnabled", "TaskRunning", "KernelRunning", "TrayRunning", "$state.SID -ne $sid", "$state.InstallDir -ne", "Stop-LinkgateInstallation $InstallDir -RemoveTask"} {
		if !strings.Contains(transaction, want) {
			t.Fatalf("missing transaction snapshot/restore field: %s", want)
		}
	}
	updater := read("deploy/windows/update.ps1")
	if strings.Index(updater, "New-LinkgateSnapshot") > strings.Index(updater, "Stop-LinkgateInstallation") || !strings.Contains(updater, "Restore-LinkgateSnapshot $InstallDir $backup") {
		t.Fatal("update migration is not transactional")
	}
	workflow := read(".github/workflows/release-windows.yml")
	if strings.Contains(workflow, "--sort=-version") || !strings.Contains(workflow, "LINKGATE_TRIGGER_REF") || !strings.Contains(workflow, "$published.Count -ne 1") || !strings.Contains(workflow, "release.published_at") {
		t.Fatal("release tag is guessed instead of matched to a published release")
	}
}
