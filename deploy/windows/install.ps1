# Windows 10/11, PowerShell 5.1+, no administrator rights required.
# Release ZIP: double-click install.bat after extracting all files.
# Source build: .\deploy\windows\install.ps1 -SourceDir .\bin\dist
param(
    [string]$SourceDir = $PSScriptRoot,
    [string]$Config = "$env:USERPROFILE\.linkgate\config.yaml"
)

. (Join-Path $PSScriptRoot 'common.ps1')

try {
    $kernelSource = Get-LinkgateBinary $SourceDir 'linkgate'
    $traySource = Get-LinkgateBinary $SourceDir 'linkgate-tray'
    $releaseVersion = Get-LinkgateVersion $kernelSource 'linkgate'
    $trayVersion = Get-LinkgateVersion $traySource 'linkgate-tray'
    if ($releaseVersion -ne $trayVersion) { throw 'Kernel and tray versions do not match. Extract one complete release ZIP.' }
    $InstallDir = Get-LinkgateInstallDir
    $kernel = Join-Path $InstallDir 'linkgate.exe'
    $tray = Join-Path $InstallDir 'linkgate-tray.exe'
    # Allow an interrupted install to resume, but never overwrite different binaries.
    foreach ($pair in @(@($kernelSource, $kernel), @($traySource, $tray))) {
        if ((Test-Path -LiteralPath $pair[1]) -and
            (Get-FileHash -LiteralPath $pair[0]).Hash -ne (Get-FileHash -LiteralPath $pair[1]).Hash) {
            throw 'Linkgate is already installed with different binaries. Double-click update.bat in the new release folder.'
        }
    }
    $existingTask = Get-LinkgateTask $InstallDir
    if ($null -ne $existingTask -or (Test-Path -LiteralPath (Join-Path $InstallDir 'install-state.json'))) {
        $Config = Get-LinkgateConfig $InstallDir
    }
    $Config = [IO.Path]::GetFullPath($Config)
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    if (-not (Test-Path -LiteralPath $kernel)) { Copy-Item -LiteralPath $kernelSource -Destination $kernel }
    if (-not (Test-Path -LiteralPath $tray)) { Copy-Item -LiteralPath $traySource -Destination $tray }
    if (-not (Test-Path -LiteralPath $Config)) {
        & $kernel -init -config $Config
        if ($LASTEXITCODE -ne 0) { throw 'Default config initialization failed.' }
    }
    & $kernel -check -config $Config
    if ($LASTEXITCODE -ne 0) { throw 'Config validation failed. Fix the config before starting Linkgate.' }

    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $action = New-ScheduledTaskAction -Execute $kernel -Argument "-config `"$Config`"" -WorkingDirectory $InstallDir
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $sid
    $principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew
    Register-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null

    $startup = [Environment]::GetFolderPath('Startup')
    $ws = New-Object -ComObject WScript.Shell
    $sc = $ws.CreateShortcut((Join-Path $startup 'Linkgate Tray.lnk'))
    $sc.TargetPath = $tray
    $sc.Arguments = "-config `"$Config`""
    $sc.WorkingDirectory = $InstallDir
    $sc.IconLocation = "$tray,0"
    $sc.Save()
    @{ Config = $Config; Version = $releaseVersion } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $InstallDir 'install-state.json') -Encoding UTF8
    foreach ($script in @('common.ps1', 'update.ps1', 'update.bat', 'check-update.bat')) {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot $script) -Destination (Join-Path $InstallDir $script) -Force
    }
    Start-Linkgate $InstallDir $Config
    Write-Host "Installed Linkgate $releaseVersion in $InstallDir."
    Write-Host 'Look for the colored Linkgate logo in the bottom-right tray (or the hidden-icons arrow).'
    Write-Host 'Click the logo, then Open panel. Add your service/routes; the initial config uses examples only.'
    Write-Host 'Next update: extract the new Windows amd64 ZIP and double-click update.bat.'
} catch {
    Write-Host "Installation failed: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host 'Config and tokens were not overwritten. Keep this window open to inspect the error.'
    exit 1
}
