# 安装器/更新器共用的事务快照。所有恢复入口先核验 SID 和精确目录。
function Get-LinkgateStartupShortcut {
    return Join-Path ([Environment]::GetFolderPath('Startup')) 'Linkgate Tray.lnk'
}

function Test-LinkgateOwnedShortcut([string]$InstallDir) {
    $shortcut = Get-LinkgateStartupShortcut
    if (Test-Path -LiteralPath $shortcut) {
        $ws = New-Object -ComObject WScript.Shell
        if ($ws.CreateShortcut($shortcut).TargetPath -ne (Join-Path $InstallDir 'linkgate-tray.exe')) {
            throw '托盘启动项属于其他安装目录，未做修改。'
        }
    }
}

function New-LinkgateSnapshot([string]$InstallDir, [string]$SnapshotDir, [string]$Config) {
    $task = Get-LinkgateTask $InstallDir
    Test-LinkgateOwnedShortcut $InstallDir
    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $shortcut = Get-LinkgateStartupShortcut
    $files = @('linkgate.exe', 'linkgate-tray.exe', 'install-state.json', 'common.ps1', 'transaction.ps1', 'setup-helper.ps1', 'update.ps1', 'update.bat', 'check-update.bat')
    $present = @()
    New-Item -ItemType Directory -Path $SnapshotDir -ErrorAction Stop | Out-Null
    foreach ($name in $files) {
        $source = Join-Path $InstallDir $name
        if (Test-Path -LiteralPath $source) {
            Copy-Item -LiteralPath $source -Destination (Join-Path $SnapshotDir $name)
            $present += $name
        }
    }
    $hadShortcut = Test-Path -LiteralPath $shortcut
    if ($hadShortcut) { Copy-Item -LiteralPath $shortcut -Destination (Join-Path $SnapshotDir 'tray.lnk') }
    if ($null -ne $task) {
        Export-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' | Set-Content -LiteralPath (Join-Path $SnapshotDir 'task.xml') -Encoding Unicode
    }
    $state = @{
        SID = $sid; InstallDir = [IO.Path]::GetFullPath($InstallDir); Config = $Config
        Files = $files; Present = $present; HadTask = ($null -ne $task); HadShortcut = $hadShortcut
        TaskEnabled = ($null -ne $task -and $task.State -ne 'Disabled')
        TaskRunning = ($null -ne $task -and $task.State -eq 'Running')
        KernelRunning = (@(Get-LinkgateProcesses $InstallDir 'linkgate').Count -gt 0)
        TrayRunning = (@(Get-LinkgateProcesses $InstallDir 'linkgate-tray').Count -gt 0)
    }
    $state | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $SnapshotDir 'snapshot.json') -Encoding UTF8
    return [PSCustomObject]$state
}

function Read-LinkgateSnapshot([string]$InstallDir, [string]$SnapshotDir) {
    $state = Get-Content -LiteralPath (Join-Path $SnapshotDir 'snapshot.json') -Raw | ConvertFrom-Json
    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    if ($state.SID -ne $sid -or $state.InstallDir -ne [IO.Path]::GetFullPath($InstallDir)) {
        throw '快照用户或安装目录不匹配，拒绝恢复。'
    }
    $allowed = @('linkgate.exe', 'linkgate-tray.exe', 'install-state.json', 'common.ps1', 'transaction.ps1', 'setup-helper.ps1', 'update.ps1', 'update.bat', 'check-update.bat')
    foreach ($name in @($state.Files) + @($state.Present)) {
        if ($name -notin $allowed) { throw '快照包含未知文件，拒绝恢复。' }
    }
    if ($state.HadTask) {
        $xml = [xml](Get-Content -LiteralPath (Join-Path $SnapshotDir 'task.xml') -Raw)
        $owner = [string]$xml.Task.Principals.Principal.UserId
        if ($owner -ne $sid) { $owner = (New-Object Security.Principal.NTAccount($owner)).Translate([Security.Principal.SecurityIdentifier]).Value }
        if ($owner -ne $sid -or @($xml.Task.Actions.Exec).Count -ne 1 -or $xml.Task.Actions.Exec.Command -ne (Join-Path $InstallDir 'linkgate.exe')) {
            throw '快照任务的 SID 或执行路径不匹配，拒绝恢复。'
        }
    }
    return $state
}

function Stop-LinkgateInstallation([string]$InstallDir, [switch]$RemoveTask) {
    $task = Get-LinkgateTask $InstallDir
    if ($null -ne $task) {
        Disable-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' | Out-Null
        Stop-ScheduledTask -TaskName 'Linkgate' -TaskPath '\'
    }
    foreach ($name in @('linkgate-tray', 'linkgate')) {
        foreach ($p in @(Get-LinkgateProcesses $InstallDir $name)) { Stop-LinkgateProcess $p }
    }
    if ($RemoveTask -and $null -ne $task) {
        Unregister-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' -Confirm:$false
    }
}

function Restore-LinkgateSnapshot([string]$InstallDir, [string]$SnapshotDir) {
    $state = Read-LinkgateSnapshot $InstallDir $SnapshotDir
    Test-LinkgateOwnedShortcut $InstallDir
    Stop-LinkgateInstallation $InstallDir -RemoveTask
    # 本轮新增文件归档到快照中，不删除用户配置、日志或令牌。
    $replaced = Join-Path $SnapshotDir ('replaced-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $replaced | Out-Null
    foreach ($name in $state.Files) {
        if ($name -notin @('linkgate.exe', 'linkgate-tray.exe', 'install-state.json', 'common.ps1', 'transaction.ps1', 'setup-helper.ps1', 'update.ps1', 'update.bat', 'check-update.bat')) {
            throw '快照包含未知文件，拒绝恢复。'
        }
        $destination = Join-Path $InstallDir $name
        if ($name -in $state.Present) {
            Copy-Item -LiteralPath (Join-Path $SnapshotDir $name) -Destination $destination -Force
        } elseif (Test-Path -LiteralPath $destination) {
            Move-Item -LiteralPath $destination -Destination (Join-Path $replaced $name)
        }
    }
    $shortcut = Get-LinkgateStartupShortcut
    if ($state.HadShortcut) {
        Copy-Item -LiteralPath (Join-Path $SnapshotDir 'tray.lnk') -Destination $shortcut -Force
    } elseif (Test-Path -LiteralPath $shortcut) {
        Move-Item -LiteralPath $shortcut -Destination (Join-Path $replaced 'tray.lnk')
    }
    if ($state.HadTask) {
        & schtasks.exe /Create /TN Linkgate /XML (Join-Path $SnapshotDir 'task.xml') /F | Out-Null
        if ($LASTEXITCODE -ne 0) { throw '旧任务 XML 恢复失败，快照已保留。' }
        if (-not $state.TaskEnabled) { Disable-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' | Out-Null }
        if ($state.TaskRunning) { Start-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' }
    }
    if ($state.KernelRunning -and -not $state.TaskRunning) {
        # 原来是同路径便携启动，不把它强行改成计划任务实例。
        Start-Process -FilePath (Join-Path $InstallDir 'linkgate.exe') -ArgumentList ('-config "' + $state.Config + '"') -WorkingDirectory $InstallDir -WindowStyle Hidden
    }
    if ($state.TrayRunning) {
        Start-Process -FilePath (Join-Path $InstallDir 'linkgate-tray.exe') -ArgumentList ('-config "' + $state.Config + '"') -WorkingDirectory $InstallDir
    }
    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    do {
        $kernelRunning = (@(Get-LinkgateProcesses $InstallDir 'linkgate').Count -gt 0)
        $trayRunning = (@(Get-LinkgateProcesses $InstallDir 'linkgate-tray').Count -gt 0)
        if ($kernelRunning -eq [bool]$state.KernelRunning -and $trayRunning -eq [bool]$state.TrayRunning) { return }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $deadline)
    throw '文件与启动项已恢复，但原进程运行状态未恢复；快照已保留。'
}
