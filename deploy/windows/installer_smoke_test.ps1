# 真实安装器 E2E：仅允许临时 GitHub Windows runner，不能在用户机器上顺手执行。
param([Parameter(Mandatory = $true)][string]$Installer)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true' -or $env:RUNNER_OS -ne 'Windows') {
    throw '此测试只能在临时 GitHub Windows runner 上执行。'
}
. (Join-Path $PSScriptRoot 'common.ps1')
$Installer = (Resolve-Path -LiteralPath $Installer).Path
$installDir = Get-LinkgateInstallDir
$configDir = Join-Path $env:USERPROFILE '.linkgate'
$config = Join-Path $configDir 'config.yaml'
$kernel = Join-Path $installDir 'linkgate.exe'
$tray = Join-Path $installDir 'linkgate-tray.exe'
$startup = Join-Path ([Environment]::GetFolderPath('Startup')) 'Linkgate Tray.lnk'
if ((Test-Path -LiteralPath $installDir) -or (Test-Path -LiteralPath $configDir) -or
    (Test-Path -LiteralPath $startup) -or $null -ne (Get-LinkgateTask $installDir) -or
    @(Get-Process -Name 'linkgate', 'linkgate-tray' -ErrorAction SilentlyContinue).Count -gt 0) {
    throw 'Runner 存在旧 Linkgate 状态，拒绝覆盖或清理未知安装。'
}
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 8766)
try { $listener.Start() } finally { $listener.Stop() }
$logDir = Join-Path $env:RUNNER_TEMP ('linkgate-installer-smoke-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $logDir | Out-Null
if ($env:GITHUB_OUTPUT) { "logs=$logDir" >> $env:GITHUB_OUTPUT }
$cleanupNeeded = $false

function Stop-TestInstallation {
    if (Test-Path -LiteralPath $kernel) {
        $output = & $kernel -uninstall 2>&1
        if ($LASTEXITCODE -ne 0) { throw ('测试安装清理失败：' + ($output -join "`n")) }
    }
    if ($null -ne (Get-LinkgateTask $installDir) -or (Test-Path -LiteralPath $startup) -or
        @(Get-LinkgateProcesses $installDir 'linkgate').Count -gt 0 -or
        @(Get-LinkgateProcesses $installDir 'linkgate-tray').Count -gt 0) {
        throw '测试安装仍有启动项或进程，停止后续测试。'
    }
}

function Assert-SilentInstallation([bool]$Autostart, [bool]$WithTray) {
    $state = Get-Content -LiteralPath (Join-Path $installDir 'install-state.json') -Raw | ConvertFrom-Json
    if ($state.Autostart -ne $Autostart -or $state.Tray -ne $WithTray) { throw '静默任务选择与安装元数据不一致。' }
    if ($state.Config -ne $config -or -not (Test-Path -LiteralPath $config)) { throw '默认配置未在当前用户目录生成。' }
    $task = Get-LinkgateTask $installDir
    if ($null -eq $task -or [string]$task.Principal.LogonType -ne 'S4U') { throw '静默安装没有注册当前用户 S4U 内核任务。' }
    $logonTriggers = @($task.Triggers | Where-Object { $null -ne $_ -and $_.CimClass.CimClassName -eq 'MSFT_TaskLogonTrigger' })
    if (($logonTriggers.Count -gt 0) -ne $Autostart) { throw '计划任务登录触发器不符合显式/默认任务选择。' }
    if ($task.Actions[0].Arguments -ne ('-config "' + $config + '"')) { throw '计划任务配置参数错误。' }
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    $ready = $false
    do {
        $kernels = @(Get-LinkgateProcesses $installDir 'linkgate')
        $trays = @(Get-LinkgateProcesses $installDir 'linkgate-tray')
        $healthy = $false
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri 'http://127.0.0.1:8766/__linkgate/v1/health' -TimeoutSec 2
            $healthy = $response.StatusCode -eq 200
        } catch { $healthy = $false }
        if ($kernels.Count -eq 1 -and $healthy -and (($trays.Count -eq 1) -eq $WithTray)) {
            if (-not $WithTray -and $trays.Count -ne 0) { throw '明确不创建托盘时仍启动了托盘进程。' }
            $ready = $true
            break
        }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $deadline)
    if (-not $ready) { throw '静默安装完成后内核/托盘未常驻，或 8766 health 未返回 200。' }
    # 托盘不要求有顶层窗口，MainWindowHandle = 0 是正常状态。
    $expectShortcut = $Autostart -and $WithTray
    if ((Test-Path -LiteralPath $startup) -ne $expectShortcut) { throw '托盘 Startup 快捷方式不符合任务选择。' }
    if ($expectShortcut) {
        $ws = New-Object -ComObject WScript.Shell
        $shortcut = $ws.CreateShortcut($startup)
        if ($shortcut.TargetPath -ne $tray -or $shortcut.Arguments -ne ('-config "' + $config + '"')) { throw '托盘启动项的路径或配置参数错误。' }
    }
}

# 默认第一次安装，以及显式选择/取消任务、再次默认安装（不能沿用上次取消状态）。
$cases = @(
    @{ Name = 'defaults'; Tasks = ''; Autostart = $true; Tray = $true },
    @{ Name = 'autostart-only'; Tasks = '/TASKS=autostart'; Autostart = $true; Tray = $false },
    @{ Name = 'tray-only'; Tasks = '/TASKS=tray'; Autostart = $false; Tray = $true },
    @{ Name = 'no-tasks'; Tasks = '/TASKS=""'; Autostart = $false; Tray = $false },
    @{ Name = 'merge-no-tray'; Tasks = '/MERGETASKS=!tray'; Autostart = $true; Tray = $false },
    @{ Name = 'defaults-after-overrides'; Tasks = ''; Autostart = $true; Tray = $true }
)
try {
    foreach ($case in $cases) {
        $log = Join-Path $logDir ($case.Name + '.log')
        $arguments = '/VERYSILENT /NORESTART /SUPPRESSMSGBOXES /LOG="' + $log + '"'
        if ($case.Tasks) { $arguments += ' ' + $case.Tasks }
        $cleanupNeeded = $true
        $process = Start-Process -FilePath $Installer -ArgumentList $arguments -PassThru
        if (-not $process.WaitForExit(180000)) { throw "静默安装超时。Inno 日志：$log" }
        if ($process.ExitCode -ne 0) { throw "静默安装退出码 $($process.ExitCode)。Inno 日志：$log" }
        Assert-SilentInstallation $case.Autostart $case.Tray
        Write-Host "PASS: $($case.Name): S4U task, selected flags, running kernel/tray, health and Startup shortcut."
        Stop-TestInstallation
        $cleanupNeeded = $false
    }
} finally {
    if ($cleanupNeeded) {
        try { Stop-TestInstallation } catch { Write-Warning $_.Exception.Message }
    }
}
Write-Host 'PASS: real silent installer defaults and explicit TASKS overrides. No browser launch requested; configuration and Inno logs retained in the disposable runner.'
