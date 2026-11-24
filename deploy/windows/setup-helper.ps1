# Inno 隐藏执行桥：Prepare 只快照；Configure 是可回滚提交；Launch 不回滚。
param(
    [ValidateSet('Prepare', 'Configure', 'Rollback', 'Launch', 'Uninstall')][string]$Phase,
    [Parameter(Mandatory = $true)][string]$InstallDir,
    [string]$SnapshotDir,
    [string]$SourceDir = $PSScriptRoot,
    [ValidateSet('true', 'false')][string]$Autostart = 'true',
    [ValidateSet('true', 'false')][string]$WithTray = 'true',
    [ValidateSet('true', 'false')][string]$OpenPanel = 'false',
    [Parameter(Mandatory = $true)][string]$ErrorFile
)
function ConvertTo-InstallerArgument {
    param([string]$Value)
    if ($Value -and $Value -notmatch '[\s"]') { return $Value }
    $escaped = [regex]::Replace($Value, '(\\*)"', '$1$1\"')
    $escaped = [regex]::Replace($escaped, '(\\+)$', '$1$1')
    return '"' + $escaped + '"'
}

function Invoke-LinkgateInstallerCommand {
    param([string]$Binary, [string[]]$Arguments)
    # 不用 & exe 2>&1：PS5.1 + ErrorActionPreference=Stop 会把 stderr 当终止错误截断。
    $info = New-Object System.Diagnostics.ProcessStartInfo
    $info.FileName = $Binary
    $info.Arguments = ($Arguments | ForEach-Object { ConvertTo-InstallerArgument $_ }) -join ' '
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $info.StandardOutputEncoding = [Text.Encoding]::UTF8
    $info.StandardErrorEncoding = [Text.Encoding]::UTF8
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $info
    try {
        if (-not $process.Start()) { throw '无法启动 Linkgate 安装命令。' }
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        $process.WaitForExit()
        $output = $stdout.GetAwaiter().GetResult() + $stderr.GetAwaiter().GetResult()
        if ($process.ExitCode -ne 0) { throw "Linkgate 退出码 $($process.ExitCode)：`n$output" }
        return $output
    } finally { $process.Dispose() }
}

function Save-InstallerError {
    param([string]$Message, [string]$TemporaryFile, [string]$InstallDir)
    $name = [IO.Path]::GetFileName($TemporaryFile)
    $session = [IO.Path]::GetFileName([IO.Path]::GetDirectoryName($TemporaryFile))
    if ($session) { $name = $session + '-' + $name }
    $persistent = Join-Path (Join-Path $env:USERPROFILE '.linkgate\install-errors') $name
    $written = $false
    foreach ($destination in @($persistent, (Join-Path (Join-Path $InstallDir 'logs') $name))) {
        try {
            New-Item -ItemType Directory -Path ([IO.Path]::GetDirectoryName($destination)) -Force | Out-Null
            $Message | Set-Content -LiteralPath $destination -Encoding UTF8
            $written = $true
        } catch { }
    }
    try { $Message | Set-Content -LiteralPath $TemporaryFile -Encoding UTF8 } catch { }
    if (-not $written) { [Console]::Error.WriteLine("持久诊断日志无法写入：`n$Message") }
}

function Save-InstallerCompatibilityWarning {
    $stateFile = Join-Path $InstallDir 'install-state.json'
    if (-not (Test-Path -LiteralPath $stateFile)) { return }
    $state = Get-Content -LiteralPath $stateFile -Raw | ConvertFrom-Json
    if ($state.PSObject.Properties['LogonType'] -and $state.LogonType -in @('Interactive', 'InteractiveToken')) {
        Save-InstallerError '兼容模式：S4U 不可用，内核会以可见命令行窗口运行；关闭该窗口会停止内核。托盘仍独立运行。' ($ErrorFile + '.warning.log') $InstallDir
    }
}

function Test-InstallerInstances {
    $kernel = Join-Path $InstallDir 'linkgate.exe'
    $foreign = @(Get-CimInstance Win32_Process -Filter "Name='linkgate.exe'" | Where-Object { -not $_.ExecutablePath -or $_.ExecutablePath -ne $kernel })
    if ($foreign.Count -gt 0) { throw '检测到其他目录的 Linkgate/便携实例。请先退出旧实例，再重试；未停止其他路径进程。' }
    $null = Get-LinkgateTask $InstallDir
    Test-LinkgateOwnedShortcut $InstallDir
}

try {
    [Console]::OutputEncoding = [Text.Encoding]::UTF8
    . (Join-Path $PSScriptRoot 'common.ps1')
    . (Join-Path $PSScriptRoot 'transaction.ps1')
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)
    $kernel = Join-Path $InstallDir 'linkgate.exe'
    if ($Phase -eq 'Uninstall') {
        # 唯一卸载入口：EXE 存在则检查返回码；缺失时按 SID/目录清启动项。
        if (Test-Path -LiteralPath $kernel) {
            $output = Invoke-LinkgateInstallerCommand $kernel @('-uninstall')
        } else {
            $null = Get-LinkgateTask $InstallDir
            Test-LinkgateOwnedShortcut $InstallDir
            Stop-LinkgateInstallation $InstallDir -RemoveTask
            $shortcut = Get-LinkgateStartupShortcut
            if (Test-Path -LiteralPath $shortcut) { Remove-Item -LiteralPath $shortcut }
        }
        exit 0
    }
    if ($Phase -eq 'Prepare') {
        if ($InstallDir -ne [IO.Path]::GetFullPath((Get-LinkgateInstallDir))) { throw '向导只支持当前用户的默认 Linkgate 安装目录。' }
        Test-InstallerInstances
        $config = Get-LinkgateConfig $InstallDir
        if (Test-Path -LiteralPath $config) {
            $output = Invoke-LinkgateInstallerCommand (Join-Path $SourceDir 'linkgate.exe') @('-check', '-config', $config)
        }
        $null = New-LinkgateSnapshot $InstallDir $SnapshotDir $config
        @('[Install]', "Config=$config") | Set-Content -LiteralPath (Join-Path $SnapshotDir 'config.ini') -Encoding Unicode
    } elseif ($Phase -eq 'Rollback') {
        if (-not (Test-Path -LiteralPath (Join-Path $SnapshotDir 'restored.txt'))) {
            Restore-LinkgateSnapshot $InstallDir $SnapshotDir
            'restored' | Set-Content -LiteralPath (Join-Path $SnapshotDir 'restored.txt')
        }
    } elseif ($Phase -eq 'Configure') {
        $state = Read-LinkgateSnapshot $InstallDir $SnapshotDir
        # ssInstall 在 Inno 自动复制前触发；此处显式复制 EXE 并提交，再交给 Inno 记录文件。
        try {
            Test-InstallerInstances
            Stop-LinkgateInstallation $InstallDir -RemoveTask
            New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
            foreach ($name in @('linkgate.exe', 'linkgate-tray.exe')) {
                Copy-Item -LiteralPath (Join-Path $SourceDir $name) -Destination (Join-Path $InstallDir $name) -Force
            }
            $output = Invoke-LinkgateInstallerCommand $kernel @('-install', '-config', $state.Config, '-install-start=false', "-install-autostart=$Autostart", "-install-tray=$WithTray")
            Save-InstallerCompatibilityWarning
        } catch {
            $failure = $_.Exception.Message
            try {
                Restore-LinkgateSnapshot $InstallDir $SnapshotDir
                'restored' | Set-Content -LiteralPath (Join-Path $SnapshotDir 'restored.txt')
            } catch { throw "安装提交失败：$failure；恢复失败：$($_.Exception.Message)。快照：$SnapshotDir" }
            throw "安装提交失败，原任务、快捷方式、EXE、元数据及运行状态已恢复：$failure。快照：$SnapshotDir"
        }
    } else {
        $config = Get-LinkgateConfig $InstallDir
        $output = Invoke-LinkgateInstallerCommand $kernel @('-install', '-config', $config, '-install-start=true', "-install-open-panel=$OpenPanel", "-install-autostart=$Autostart", "-install-tray=$WithTray")
        Save-InstallerCompatibilityWarning
    }
    exit 0
} catch {
    $message = $_.Exception.Message
    Save-InstallerError $message $ErrorFile $InstallDir
    [Console]::Error.WriteLine($message)
    exit 1
}
