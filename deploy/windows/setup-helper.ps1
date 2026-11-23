# Inno 隐藏执行桥：Prepare 只快照；Configure 是可回滚提交；Launch 不回滚。
param(
    [ValidateSet('Prepare', 'Configure', 'Rollback', 'Launch', 'Uninstall')][string]$Phase,
    [Parameter(Mandatory = $true)][string]$InstallDir,
    [string]$SnapshotDir,
    [string]$SourceDir = $PSScriptRoot,
    [ValidateSet('true', 'false')][string]$Autostart = 'true',
    [ValidateSet('true', 'false')][string]$WithTray = 'true',
    [Parameter(Mandatory = $true)][string]$ErrorFile
)
. (Join-Path $PSScriptRoot 'common.ps1')
. (Join-Path $PSScriptRoot 'transaction.ps1')

function Test-InstallerInstances {
    $kernel = Join-Path $InstallDir 'linkgate.exe'
    $foreign = @(Get-CimInstance Win32_Process -Filter "Name='linkgate.exe'" | Where-Object { -not $_.ExecutablePath -or $_.ExecutablePath -ne $kernel })
    if ($foreign.Count -gt 0) { throw '检测到其他目录的 Linkgate/便携实例。请先退出旧实例，再重试；未停止其他路径进程。' }
    $null = Get-LinkgateTask $InstallDir
    Test-LinkgateOwnedShortcut $InstallDir
}

try {
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)
    $kernel = Join-Path $InstallDir 'linkgate.exe'
    if ($Phase -eq 'Uninstall') {
        # 唯一卸载入口：EXE 存在则检查返回码；缺失时按 SID/目录清启动项。
        if (Test-Path -LiteralPath $kernel) {
            $output = & $kernel -uninstall 2>&1
            if ($LASTEXITCODE -ne 0) { throw ($output -join "`n") }
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
            $output = & (Join-Path $SourceDir 'linkgate.exe') -check -config $config 2>&1
            if ($LASTEXITCODE -ne 0) { throw ($output -join "`n") }
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
            $output = & $kernel -install -config $state.Config -install-start=false "-install-autostart=$Autostart" "-install-tray=$WithTray" 2>&1
            if ($LASTEXITCODE -ne 0) { throw ($output -join "`n") }
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
        $output = & $kernel -install -config $config -install-start=true -install-open-panel=true "-install-autostart=$Autostart" "-install-tray=$WithTray" 2>&1
        if ($LASTEXITCODE -ne 0) { throw ($output -join "`n") }
    }
    exit 0
} catch {
    $_.Exception.Message | Set-Content -LiteralPath $ErrorFile -Encoding UTF8
    exit 1
}
