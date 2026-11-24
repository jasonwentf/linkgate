package main

import (
	"strings"
	"testing"
)

func TestWindowsRegistrationAndPersistentDiagnosticContract(t *testing.T) {
	windows := readSilentInstallerSource(t, "cmd/linkgate/install_windows.go")
	for _, want := range []string{"registerWindowsTaskWithFallback", "New-ScheduledTaskAction", "New-ScheduledTaskPrincipal", "-LogonType $env:LINKGATE_INSTALL_LOGON_TYPE -RunLevel Limited", "Register-ScheduledTask @parameters", "decodeWindowsOutput(out)", "RegistrationDiagnostic"} {
		if !strings.Contains(windows, want) {
			t.Fatalf("missing task fallback contract: %s", want)
		}
	}
	helper := readSilentInstallerSource(t, "deploy/windows/setup-helper.ps1")
	for _, want := range []string{"[Console]::OutputEncoding = [Text.Encoding]::UTF8", "RedirectStandardError = $true", "ReadToEndAsync()", "Save-InstallerError $message $ErrorFile $InstallDir", "'.linkgate\\install-errors'", "Set-Content -LiteralPath $destination -Encoding UTF8", "StandardErrorEncoding = [Text.Encoding]::UTF8"} {
		if !strings.Contains(helper, want) {
			t.Fatalf("missing complete/durable error contract: %s", want)
		}
	}
	iss := readSilentInstallerSource(t, "deploy/windows/linkgate.iss")
	for _, want := range []string{"PersistentErrorPath(ErrorFile)", "LoadStringsFromFile(Path, Lines)", "RecordHelperDiagnostic(ErrorFile)", "CheckRegistrationWarning", "关闭该窗口会停止内核"} {
		if !strings.Contains(iss, want) {
			t.Fatalf("missing installation diagnostic/prompt: %s", want)
		}
	}
	if strings.Contains(iss, "错误详情：' + ErrorFile") || strings.Contains(iss, "错误日志：' + ErrorFile") {
		t.Fatal("user prompt points to temporary deleted logs")
	}
	workflow := readSilentInstallerSource(t, ".github/workflows/release-windows.yml")
	if !strings.Contains(workflow, "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File deploy/windows/diagnostics_test.ps1") {
		t.Fatal("diagnostic regression must run with Windows PowerShell 5.1")
	}
}
