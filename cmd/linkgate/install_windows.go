//go:build windows

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"linkgate/internal/app"
)

// 所有系统工具都隐藏窗口，路径通过环境传递，不拼进 PowerShell 源码。
func windowsCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func decodeWindowsOutput(out []byte) string {
	if utf8.Valid(out) {
		return string(out)
	}
	kernel := syscall.NewLazyDLL("kernel32.dll")
	codepage, _, _ := kernel.NewProc("GetConsoleOutputCP").Call()
	if codepage == 0 || codepage == 65001 {
		codepage, _, _ = kernel.NewProc("GetOEMCP").Call()
	}
	return decodeWindowsCodepage(out, codepage)
}

func decodeWindowsCodepage(out []byte, codepage uintptr) string {
	if len(out) == 0 {
		return ""
	}
	convert := syscall.NewLazyDLL("kernel32.dll").NewProc("MultiByteToWideChar")
	count, _, _ := convert.Call(codepage, 0, uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)), 0, 0)
	if count == 0 {
		return string(out)
	}
	text := make([]uint16, count)
	written, _, _ := convert.Call(codepage, 0, uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)), uintptr(unsafe.Pointer(&text[0])), count)
	if written == 0 {
		return string(out)
	}
	return syscall.UTF16ToString(text)
}

func windowsPowerShell(script string, values map[string]string) ([]byte, error) {
	u := utf16.Encode([]rune("$ErrorActionPreference = 'Stop'; [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); " + script))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	cmd := windowsCommand("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.Env = append(os.Environ(), "LINKGATE_INSTALL_SELF="+strconv.Itoa(os.Getpid()))
	for key, value := range values {
		cmd.Env = append(cmd.Env, "LINKGATE_INSTALL_"+key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Windows 操作失败: %w\n%s", err, decodeWindowsOutput(out))
	}
	return out, nil
}

type windowsState struct {
	SID        string           `json:"SID"`
	Task       *windowsTask     `json:"Task"`
	Processes  []windowsProcess `json:"Processes"`
	TrayTarget string           `json:"TrayTarget"`
}

func inspectWindowsInstall() (windowsState, error) {
	out, err := windowsPowerShell(`
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$task = $null
try { $t = Get-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' -ErrorAction Stop } catch {
    if ($_.CategoryInfo.Category -ne 'ObjectNotFound') { throw }; $t = $null
}
if ($null -ne $t) {
    $owner = $t.Principal.UserId
    if ($owner -ne $sid) {
        try { $owner = (New-Object Security.Principal.NTAccount($owner)).Translate([Security.Principal.SecurityIdentifier]).Value } catch { throw '无法确认计划任务所有者，拒绝修改。' }
    }
    $task = @{ UserID = $owner; ActionCount = @($t.Actions).Count; Execute = $t.Actions[0].Execute; Arguments = $t.Actions[0].Arguments; WorkingDirectory = $t.Actions[0].WorkingDirectory; LogonType = [string]$t.Principal.LogonType; Autostart = (@($t.Triggers).Count -gt 0); Enabled = ($t.State -ne 'Disabled') }
}
$processes = @(Get-CimInstance Win32_Process -Filter "Name='linkgate.exe'" | Where-Object { $_.ProcessId -ne [int]$env:LINKGATE_INSTALL_SELF } | ForEach-Object { @{ PID = [int]$_.ProcessId; Path = [string]$_.ExecutablePath; CommandLine = [string]$_.CommandLine } })
$trayTarget = ''
$shortcut = Join-Path ([Environment]::GetFolderPath('Startup')) 'Linkgate Tray.lnk'
if (Test-Path -LiteralPath $shortcut) { $trayTarget = (New-Object -ComObject WScript.Shell).CreateShortcut($shortcut).TargetPath }
# -NonInteractive 的 powershell.exe 会把管道输出序列化成 CLIXML；
# JSON 必须经 [Console]::WriteLine 直写 stdout 绕开序列化。
[Console]::WriteLine((@{ SID = $sid; Task = $task; Processes = $processes; TrayTarget = $trayTarget } | ConvertTo-Json -Depth 5 -Compress))
`, nil)
	var state windowsState
	if err == nil {
		err = unmarshalWindowsState(out, &state)
	}
	if err == nil {
		for i := range state.Processes {
			p := &state.Processes[i]
			args, parseErr := parseWindowsCommandLine(p.CommandLine)
			if parseErr != nil {
				continue
			}
			config := configFromWindowsArgs(args, app.DefaultPath())
			if config == "" {
				continue
			}
			config = app.ExpandPath(config)
			if !filepath.IsAbs(config) {
				if state.Task == nil || !sameWindowsPath(state.Task.Execute, p.Path) || state.Task.WorkingDirectory == "" {
					continue
				}
				config = filepath.Join(state.Task.WorkingDirectory, config)
			}
			p.ConfigPath = filepath.Clean(config)
		}
	}
	return state, err
}

// stripWindowsCLIXML 在 -NonInteractive 下 PowerShell 可能把输出包成 CLIXML
// （#< CLIXML 头 + <Objs> 包装 + progress 记录）。JSON 本体是第一个 '{' 起的
// 完整值，交给 json.Decoder 读取首个 JSON 值，忽略其后的 CLIXML 残余。
func stripWindowsCLIXML(out []byte) []byte {
	start := bytes.IndexByte(out, '{')
	if start < 0 {
		return out
	}
	return out[start:]
}

func unmarshalWindowsState(out []byte, state *windowsState) error {
	return json.NewDecoder(bytes.NewReader(stripWindowsCLIXML(out))).Decode(state)
}

func parseWindowsCommandLine(commandLine string) ([]string, error) {
	if commandLine == "" {
		return nil, fmt.Errorf("无法读取进程命令行")
	}
	text, err := syscall.UTF16PtrFromString(commandLine)
	if err != nil {
		return nil, err
	}
	var count int32
	argv, err := syscall.CommandLineToArgv(text, &count)
	if err != nil {
		return nil, err
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(unsafe.Pointer(argv))))
	if count < 1 || count > 8192 {
		return nil, fmt.Errorf("进程参数数量异常")
	}
	args := make([]string, count)
	for i := range args {
		args[i] = syscall.UTF16ToString(argv[i][:])
	}
	return args, nil
}

func registerWindowsTask(xmlPath string, values map[string]string, autostart bool) (windowsRegistrationResult, error) {
	values["AUTOSTART"] = strconv.FormatBool(autostart)
	return registerWindowsTaskWithFallback(func(method windowsRegistrationMethod) error {
		if method == windowsRegisterXML {
			out, err := windowsCommand("schtasks.exe", "/Create", "/TN", "Linkgate", "/XML", xmlPath, "/F").CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w\n%s", err, decodeWindowsOutput(out))
			}
			return nil
		}
		values["LOGON_TYPE"] = "S4U"
		if method == windowsRegisterInteractive {
			values["LOGON_TYPE"] = "Interactive"
		}
		_, err := windowsPowerShell(`
try {
    $existing = $null
    try { $existing = Get-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' -ErrorAction Stop } catch {
        if ($_.CategoryInfo.Category -ne 'ObjectNotFound') { throw }
    }
    if ($null -ne $existing) {
        $owner = $existing.Principal.UserId
        if ($owner -ne $env:LINKGATE_INSTALL_SID) { $owner = (New-Object Security.Principal.NTAccount($owner)).Translate([Security.Principal.SecurityIdentifier]).Value }
        if ($owner -ne $env:LINKGATE_INSTALL_SID -or @($existing.Actions).Count -ne 1 -or $existing.Actions[0].Execute -ne $env:LINKGATE_INSTALL_EXE) { throw '任务属于其他用户或目录，拒绝覆盖。' }
    }
    $action = New-ScheduledTaskAction -Execute $env:LINKGATE_INSTALL_EXE -Argument ('-config "' + $env:LINKGATE_INSTALL_CONFIG + '"') -WorkingDirectory $env:LINKGATE_INSTALL_DIR
    $principal = New-ScheduledTaskPrincipal -UserId $env:LINKGATE_INSTALL_SID -LogonType $env:LINKGATE_INSTALL_LOGON_TYPE -RunLevel Limited
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 255 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew
    $parameters = @{ TaskName = 'Linkgate'; TaskPath = '\'; Action = $action; Principal = $principal; Settings = $settings; Force = $true }
    if ($env:LINKGATE_INSTALL_AUTOSTART -eq 'true') { $parameters.Trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:LINKGATE_INSTALL_SID }
    Register-ScheduledTask @parameters | Out-Null
    $registered = Get-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' -ErrorAction Stop
    if ([string]$registered.Principal.LogonType -ne $env:LINKGATE_INSTALL_LOGON_TYPE) { throw '注册后的任务登录类型不符合预期。' }
} catch {
    [Console]::Error.WriteLine($_.Exception.ToString())
    exit 1
}
`, values)
		return err
	})
}

func retainWindowsRegistrationDiagnostic(result windowsRegistrationResult) string {
	text := result.Diagnostics
	if windowsInteractiveTask(result.LogonType) {
		text += "\n" + windowsInteractiveWarning
	}
	if text == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".linkgate", "install-errors")
	if os.MkdirAll(dir, 0700) != nil {
		return ""
	}
	path := filepath.Join(dir, "registration-"+time.Now().UTC().Format("20060102-150405.000000000")+".log")
	if os.WriteFile(path, []byte(text), 0600) != nil {
		return ""
	}
	return path
}

func runInstall(configPath string, uninstall bool) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	directory := filepath.Dir(executable)
	configPath, err = filepath.Abs(app.ExpandPath(configPath))
	if err != nil {
		return err
	}
	if windowsInstall.dryrun {
		if uninstall {
			fmt.Println("只读计划：核对任务所有者与程序路径 → 停任务 → 注销任务 → 删除本安装的托盘启动项 → 停同目录内核/托盘；保留配置与日志")
		} else {
			fmt.Println("只读计划：检查旧实例 → 缺配置则 -init → -check → 检查端口 → 注册 S4U 任务 → 按选项创建托盘启动项/立即启动")
			fmt.Println(windowsTaskXML("S-1-5-21-当前用户", executable, configPath, directory, windowsInstall.autostart))
		}
		return nil
	}
	state, err := inspectWindowsInstall()
	if err != nil {
		return fmt.Errorf("无法检查已有 Linkgate 安装: %w", err)
	}
	if err := verifyWindowsTask(state.Task, state.SID, executable); err != nil {
		return err
	}
	values := map[string]string{"EXE": executable, "DIR": directory, "CONFIG": configPath, "TRAY": filepath.Join(directory, "linkgate-tray.exe"), "SID": state.SID}
	if uninstall {
		return uninstallWindows(state, values)
	}
	// 同目录、同配置的健康实例可补做启动项与托盘；其他实例仍拒绝。
	running, err := classifyWindowsProcesses(state.Processes, os.Getpid(), executable, configPath)
	if err != nil {
		return err
	}
	if state.TrayTarget != "" && !sameWindowsPath(state.TrayTarget, values["TRAY"]) {
		return fmt.Errorf("Linkgate 托盘启动项属于其他安装目录，请先从原安装目录卸载；未做修改")
	}
	if windowsInstall.tray {
		if _, err := os.Stat(values["TRAY"]); err != nil {
			return fmt.Errorf("找不到托盘程序，请使用完整 Windows 安装包: %w", err)
		}
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := writeExample(configPath); err != nil {
			return fmt.Errorf("初始化配置失败: %w", err)
		}
	} else if err != nil {
		return err
	}
	out, err := windowsCommand(executable, "-check", "-config", configPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("配置校验失败，请修正配置后重试: %w\n%s", err, decodeWindowsOutput(out))
	}
	cfg, err := app.Load(configPath)
	if err != nil {
		return err
	}
	if running {
		if state.Task != nil && (!supportedWindowsTask(state.Task.LogonType) || !state.Task.Enabled || state.Task.Autostart != windowsInstall.autostart || !sameWindowsPath(state.Task.Arguments, `-config "`+configPath+`"`)) {
			return fmt.Errorf("当前后台任务设置不同，请先停止内核再迁移任务或修改自启设置；未修改运行中的实例")
		}
		if !healthyWindowsKernel(cfg, configPath) {
			return fmt.Errorf("同目录内核尚未健康或实际配置不匹配，请先停止旧实例后重试；未修改启动项")
		}
	} else if err := checkWindowsPorts(cfg); err != nil {
		return err
	}
	xmlFile, err := os.CreateTemp("", "linkgate-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(xmlFile.Name())
	// schtasks /XML 只认 UTF-16：文件按 UTF-16LE + BOM 写。
	xmlText := windowsTaskXML(state.SID, executable, configPath, directory, windowsInstall.autostart)
	utf16 := utf16.Encode([]rune(xmlText))
	buf := make([]byte, 2+len(utf16)*2)
	buf[0], buf[1] = 0xFF, 0xFE
	for i, c := range utf16 {
		binary.LittleEndian.PutUint16(buf[2+i*2:], c)
	}
	_, writeErr := xmlFile.Write(buf)
	closeErr := xmlFile.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	registration := windowsRegistrationResult{LogonType: "S4U"}
	if running && state.Task != nil {
		registration.LogonType = state.Task.LogonType
	}
	if !running || state.Task == nil {
		registration, err = registerWindowsTask(xmlFile.Name(), values, windowsInstall.autostart)
		if err != nil {
			return err
		}
	}
	if registration.Diagnostics != "" {
		fmt.Printf("首选任务注册失败，已回退使用 %s。原始诊断：\n%s\n", registration.Method, registration.Diagnostics)
	}
	if windowsInteractiveTask(registration.LogonType) {
		fmt.Println(windowsInteractiveWarning)
	}
	diagnosticPath := retainWindowsRegistrationDiagnostic(registration)
	if diagnosticPath != "" {
		fmt.Println("任务注册诊断已保留：", diagnosticPath)
	}
	values["LOGON_TYPE"] = registration.LogonType
	values["REGISTRATION_METHOD"] = string(registration.Method)
	values["REGISTRATION_DIAGNOSTIC"] = diagnosticPath
	values["CREATE_TRAY"] = strconv.FormatBool(windowsInstall.tray)
	values["WITH_TRAY"] = strconv.FormatBool(windowsInstall.tray)
	values["AUTOSTART"] = strconv.FormatBool(windowsInstall.autostart)
	_, err = windowsPowerShell(`
$startup = [Environment]::GetFolderPath('Startup')
$shortcut = Join-Path $startup 'Linkgate Tray.lnk'
$ws = New-Object -ComObject WScript.Shell
if ($env:LINKGATE_INSTALL_CREATE_TRAY -eq 'true' -and $env:LINKGATE_INSTALL_AUTOSTART -eq 'true') {
    $sc = $ws.CreateShortcut($shortcut)
    $sc.TargetPath = $env:LINKGATE_INSTALL_TRAY
    $sc.Arguments = '-config "' + $env:LINKGATE_INSTALL_CONFIG + '"'
    $sc.WorkingDirectory = $env:LINKGATE_INSTALL_DIR
    $sc.IconLocation = $env:LINKGATE_INSTALL_TRAY + ',0'
    $sc.Save()
} elseif (Test-Path -LiteralPath $shortcut) {
    $sc = $ws.CreateShortcut($shortcut)
    if ($sc.TargetPath -ne $env:LINKGATE_INSTALL_TRAY) { throw '托盘启动项属于其他安装，拒绝覆盖。' }
    Remove-Item -LiteralPath $shortcut
}
@{ Config = $env:LINKGATE_INSTALL_CONFIG; Autostart = ($env:LINKGATE_INSTALL_AUTOSTART -eq 'true'); Tray = ($env:LINKGATE_INSTALL_WITH_TRAY -eq 'true'); LogonType = $env:LINKGATE_INSTALL_LOGON_TYPE; RegistrationMethod = $env:LINKGATE_INSTALL_REGISTRATION_METHOD; RegistrationDiagnostic = $env:LINKGATE_INSTALL_REGISTRATION_DIAGNOSTIC } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $env:LINKGATE_INSTALL_DIR 'install-state.json') -Encoding UTF8
`, values)
	if err != nil {
		return fmt.Errorf("内核任务已注册，但托盘启动项未完成；可重新安装或运行 -uninstall: %w", err)
	}
	if windowsInstall.start {
		if !running {
			out, err := windowsCommand("schtasks.exe", "/Run", "/TN", "Linkgate").CombinedOutput()
			if err != nil {
				return fmt.Errorf("启动内核任务失败: %w\n%s", err, decodeWindowsOutput(out))
			}
			if err := waitWindowsKernel(cfg, executable); err != nil {
				return err
			}
		}
		if windowsInstall.tray {
			_, err = windowsPowerShell(`
if (@(Get-Process -Name 'linkgate-tray' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $env:LINKGATE_INSTALL_TRAY }).Count -eq 0) {
    Start-Process -FilePath $env:LINKGATE_INSTALL_TRAY -ArgumentList ('-config "' + $env:LINKGATE_INSTALL_CONFIG + '"') -WorkingDirectory $env:LINKGATE_INSTALL_DIR
}
Start-Sleep -Milliseconds 1000
if (@(Get-Process -Name 'linkgate-tray' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $env:LINKGATE_INSTALL_TRAY }).Count -eq 0) { throw '托盘启动后退出，请检查安装文件。' }
`, values)
			if err != nil {
				return err
			}
		}
		if windowsInstall.openPanel && !cfg.Admin.Disabled {
			values["PANEL"] = windowsAdminURL(cfg) + "/__linkgate/ui"
			if _, err := windowsPowerShell("Start-Process $env:LINKGATE_INSTALL_PANEL", values); err != nil {
				return fmt.Errorf("内核已运行，但自动打开面板失败: %w", err)
			}
		}
	}
	if windowsInteractiveTask(registration.LogonType) {
		fmt.Println("Linkgate 安装完成，使用可见窗口兼容模式；配置与日志保留在用户的 .linkgate 目录。")
	} else {
		fmt.Println("Linkgate 安装完成。配置与日志保留在用户的 .linkgate 目录；内核以 S4U 后台任务运行，无黑色窗口。")
	}
	return nil
}

func uninstallWindows(state windowsState, values map[string]string) error {
	if state.Task != nil {
		// 停任务失败不能直接注销，避免留下不可管理的后台进程。
		for _, args := range [][]string{{"/End", "/TN", "Linkgate"}, {"/Delete", "/TN", "Linkgate", "/F"}} {
			out, err := windowsCommand("schtasks.exe", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("停用或注销 Linkgate 任务失败: %w\n%s", err, decodeWindowsOutput(out))
			}
		}
	}
	_, err := windowsPowerShell(`
$shortcut = Join-Path ([Environment]::GetFolderPath('Startup')) 'Linkgate Tray.lnk'
if (Test-Path -LiteralPath $shortcut) {
    $ws = New-Object -ComObject WScript.Shell
    if ($ws.CreateShortcut($shortcut).TargetPath -eq $env:LINKGATE_INSTALL_TRAY) { Remove-Item -LiteralPath $shortcut }
}
foreach ($name in @('linkgate-tray', 'linkgate')) {
    $expected = Join-Path $env:LINKGATE_INSTALL_DIR ($name + '.exe')
    foreach ($p in @(Get-Process -Name $name -ErrorAction SilentlyContinue | Where-Object { $_.Id -ne [int]$env:LINKGATE_INSTALL_SELF -and $_.Path -eq $expected })) {
        Stop-Process -Id $p.Id -Force
        if (-not $p.WaitForExit(10000)) { throw '同目录进程未退出，停止卸载。' }
    }
}
`, values)
	if err == nil {
		fmt.Println("Linkgate 启动项已卸载。配置、令牌和日志未删除；其他目录的便携实例未停止。")
	}
	return err
}

func checkWindowsPorts(cfg *app.Config) error {
	var addresses []string
	if !cfg.Admin.Disabled {
		addresses = append(addresses, cfg.Admin.Listen)
	}
	for _, g := range cfg.Groups {
		addresses = append(addresses, g.Listen)
	}
	for _, address := range addresses {
		l, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("Linkgate 监听端口不可用（可能被旧实例或其他程序占用），请先退出旧实例并检查配置；未注册任务: %w", err)
		}
		l.Close()
	}
	return nil
}

func windowsAdminURL(cfg *app.Config) string {
	address := cfg.Admin.Listen
	host, port, err := net.SplitHostPort(address)
	if err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		address = net.JoinHostPort("127.0.0.1", port)
		if host == "::" {
			address = net.JoinHostPort("::1", port)
		}
	}
	return "http://" + address
}

func healthyWindowsKernel(cfg *app.Config, configPath string) bool {
	if cfg.Admin.Disabled {
		return false
	}
	token, err := os.ReadFile(app.ExpandPath(cfg.Admin.TokenFile))
	if err != nil {
		return false
	}
	request, err := http.NewRequest(http.MethodGet, windowsAdminURL(cfg)+"/__linkgate/v1/status", nil)
	if err != nil {
		return false
	}
	request.Header.Set("X-Linkgate-Token", strings.TrimSpace(string(token)))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Timeout: 2 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var status struct {
		ConfigPath string `json:"config_path"`
	}
	if json.NewDecoder(response.Body).Decode(&status) != nil {
		return false
	}
	return sameWindowsPath(app.ExpandPath(status.ConfigPath), configPath)
}

func waitWindowsKernel(cfg *app.Config, executable string) error {
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state, err := inspectWindowsInstall()
		if err != nil {
			return err
		}
		running := false
		for _, p := range state.Processes {
			if sameWindowsPath(p.Path, executable) {
				running = true
			}
		}
		if running {
			if cfg.Admin.Disabled {
				return nil
			}
			resp, err := client.Get(windowsAdminURL(cfg) + "/__linkgate/v1/health")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	// 不让失败的首启每分钟重试；配置与任务保留，修复后可重新安装。
	_, _ = windowsCommand("schtasks.exe", "/End", "/TN", "Linkgate").CombinedOutput()
	_, _ = windowsCommand("schtasks.exe", "/Change", "/TN", "Linkgate", "/DISABLE").CombinedOutput()
	return fmt.Errorf("内核启动后未通过健康检查，已停用任务以避免反复失败；请检查 ~/.linkgate/kernel.log 与配置端口，再重新安装")
}
