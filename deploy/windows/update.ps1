# No automatic binary download. Run check-update.bat for a read-only version check,
# or update.bat from an extracted release ZIP to replace this user's installation.
# Windows task/process termination interrupts in-flight requests; finish them first.
param(
    [string]$SourceDir = $PSScriptRoot,
    [switch]$CheckOnly
)

. (Join-Path $PSScriptRoot 'common.ps1')

try {
    $InstallDir = Get-LinkgateInstallDir
    $kernel = Join-Path $InstallDir 'linkgate.exe'
    $tray = Join-Path $InstallDir 'linkgate-tray.exe'
    if (-not (Test-Path -LiteralPath $kernel)) { throw 'Linkgate is not installed. Double-click install.bat first.' }
    $current = Get-LinkgateVersion $kernel 'linkgate'
    Write-Host "Installed kernel: $current"
    # Pre-0.2.2 trays had no -version flag; they can still be replaced together.
    $installedTray = $null
    try {
        $installedTray = Get-LinkgateVersion $tray 'linkgate-tray'
        Write-Host "Installed tray: $installedTray"
    } catch { Write-Host 'Installed tray cannot report its version (older release or missing tray).' }
    if ($CheckOnly) {
        # This request is anonymous and only performed on an explicit check.
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        $release = Invoke-RestMethod -Uri 'https://api.github.com/repos/jasonwentf/linkgate/releases/latest' -Headers @{ 'User-Agent' = 'Linkgate-update-check'; Accept = 'application/vnd.github+json' } -TimeoutSec 15
        $latest = [string]$release.tag_name
        $cmp = Compare-LinkgateVersion $latest $current
        if ($cmp -gt 0) {
            Write-Host "New version available: $latest"
            Write-Host 'Open https://github.com/jasonwentf/linkgate/releases/latest'
            Write-Host 'Download linkgate_x.x.x_windows_amd64.zip, extract it, then double-click update.bat.'
        } elseif ($cmp -eq 0) {
            Write-Host "Up to date: $latest"
        } else {
            Write-Host "Installed version is newer than latest stable ($latest). No downgrade needed."
        }
        if ($installedTray -and $installedTray -ne $current) {
            Write-Host 'Kernel/tray versions differ. Use update.bat from one complete release ZIP to align them.'
        } elseif (-not $installedTray) {
            Write-Host 'Tray version is unknown. A complete release ZIP updates both binaries together.'
        }
        return
    }

    $kernelSource = Get-LinkgateBinary $SourceDir 'linkgate'
    $traySource = Get-LinkgateBinary $SourceDir 'linkgate-tray'
    if ($kernelSource -eq $kernel -or $traySource -eq $tray) {
        throw 'Run update.bat from a newly extracted release folder, not from the installation folder.'
    }
    $incoming = Get-LinkgateVersion $kernelSource 'linkgate'
    if ($incoming -ne (Get-LinkgateVersion $traySource 'linkgate-tray')) { throw 'Kernel and tray versions do not match.' }
    $cmp = Compare-LinkgateVersion $incoming $current
    if ($cmp -lt 0) { throw "Refusing downgrade from $current to $incoming. Use the backup only for an intentional rollback." }
    if ($cmp -eq 0) { Write-Host 'Versions match; this will repair/reinstall both binaries.' }
    $Config = Get-LinkgateConfig $InstallDir
    & $kernelSource -check -config $Config
    if ($LASTEXITCODE -ne 0) { throw 'The new kernel rejected your config. The running installation was not changed.' }
    $task = Get-LinkgateTask $InstallDir
    if ($null -eq $task -or $task.State -eq 'Disabled') { throw 'The Linkgate logon task is missing or disabled. Repair the installation first.' }
    Write-Host "Update $current -> $incoming. Config and tokens will be kept."
    Write-Host 'This stops tray, then kernel, and interrupts any active requests.'
    $answer = Read-Host 'Finish active requests first. Type YES to update'
    if ($answer -cne 'YES') { Write-Host 'Cancelled; no changes made.'; return }

    $backup = Join-Path (Join-Path $InstallDir 'backups') ((Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $backup -Force | Out-Null
    Copy-Item -LiteralPath $kernel -Destination (Join-Path $backup 'linkgate.exe')
    $hadTray = Test-Path -LiteralPath $tray
    if ($hadTray) { Copy-Item -LiteralPath $tray -Destination (Join-Path $backup 'linkgate-tray.exe') }
    try {
        # Disable respawn before stopping processes. Only exact installation paths
        # are selected, never every process named linkgate on the machine.
        Disable-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' | Out-Null
        foreach ($p in @(Get-LinkgateProcesses $InstallDir 'linkgate-tray')) {
            Stop-LinkgateProcess $p
        }
        Stop-ScheduledTask -TaskName 'Linkgate' -TaskPath '\'
        foreach ($p in @(Get-LinkgateProcesses $InstallDir 'linkgate')) {
            Stop-LinkgateProcess $p
        }
        Copy-Item -LiteralPath $kernelSource -Destination $kernel -Force
        Copy-Item -LiteralPath $traySource -Destination $tray -Force
        if ((Get-LinkgateVersion $kernel 'linkgate') -ne $incoming -or
            (Get-LinkgateVersion $tray 'linkgate-tray') -ne $incoming) { throw 'Installed binary version check failed.' }
        Enable-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' | Out-Null
        Start-Linkgate $InstallDir $Config
    } catch {
        $failure = $_.Exception.Message
        # Roll back both binaries as a pair; keep all backups, never delete data.
        try {
            Disable-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' | Out-Null
            foreach ($p in @(Get-LinkgateProcesses $InstallDir 'linkgate-tray')) { Stop-LinkgateProcess $p }
            Stop-ScheduledTask -TaskName 'Linkgate' -TaskPath '\'
            foreach ($p in @(Get-LinkgateProcesses $InstallDir 'linkgate')) { Stop-LinkgateProcess $p }
            Copy-Item -LiteralPath (Join-Path $backup 'linkgate.exe') -Destination $kernel -Force
            if ($hadTray) { Copy-Item -LiteralPath (Join-Path $backup 'linkgate-tray.exe') -Destination $tray -Force }
            Enable-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' | Out-Null
            Start-Linkgate $InstallDir $Config
            throw "Update failed: $failure. Previous binaries restored from $backup."
        } catch {
            # Even when copying/restarting fails, restore the logon task's enabled state.
            Enable-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' -ErrorAction SilentlyContinue | Out-Null
            throw "Update/rollback result: $($_.Exception.Message). Backup: $backup"
        }
    }
    foreach ($script in @('common.ps1', 'update.ps1', 'update.bat', 'check-update.bat')) {
        try {
            Copy-Item -LiteralPath (Join-Path $PSScriptRoot $script) -Destination (Join-Path $InstallDir $script) -Force
        } catch { Write-Warning "Binaries updated, but could not refresh $script. Keep this release folder for future checks/updates." }
    }
    try {
        @{ Config = $Config; Version = $incoming } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $InstallDir 'install-state.json') -Encoding UTF8
    } catch { Write-Warning 'Binaries updated, but install metadata could not be refreshed.' }
    Write-Host "Updated to $incoming. Previous binaries: $backup"
    Write-Host 'Click the tray logo and Open panel to verify your services/routes.'
} catch {
    Write-Host "Update failed: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host 'No config or token was overwritten. If online checking failed, visit the releases page manually.'
    exit 1
}
