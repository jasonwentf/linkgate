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
    # 同目录实例由 Go 核验配置与健康，允许修复缺失步骤；外部便携实例仍拒绝。
    if (@(Get-CimInstance Win32_Process -Filter "Name='linkgate.exe'" | Where-Object { -not $_.ExecutablePath -or $_.ExecutablePath -ne $kernel }).Count -gt 0) {
        throw '检测到其他目录的 Linkgate 内核，请先退出旧便携实例。未修改安装目录或启动项。'
    }
    $Config = [IO.Path]::GetFullPath($Config)
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    if (-not (Test-Path -LiteralPath $kernel)) { Copy-Item -LiteralPath $kernelSource -Destination $kernel }
    if (-not (Test-Path -LiteralPath $tray)) { Copy-Item -LiteralPath $traySource -Destination $tray }
    # 和向导安装器复用同一个 Go 实现：S4U 非交互任务、配置校验、启动项与首启检查。
    & $kernel -install -config $Config -install-open-panel
    if ($LASTEXITCODE -ne 0) { throw 'Linkgate 自安装失败，请查看上方错误与 ~/.linkgate/kernel.log。' }
    @{ Config = $Config; Version = $releaseVersion; Autostart = $true; Tray = $true } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $InstallDir 'install-state.json') -Encoding UTF8
    foreach ($script in @('common.ps1', 'transaction.ps1', 'update.ps1', 'update.bat', 'check-update.bat')) {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot $script) -Destination (Join-Path $InstallDir $script) -Force
    }
    Write-Host "Installed Linkgate $releaseVersion in $InstallDir."
    Write-Host 'Look for the colored Linkgate logo in the bottom-right tray (or the hidden-icons arrow).'
    Write-Host 'Click the logo, then Open panel. Add your service/routes; the initial config uses examples only.'
    Write-Host 'Next update: extract the new Windows amd64 ZIP and double-click update.bat.'
} catch {
    Write-Host "Installation failed: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host 'Config and tokens were not overwritten. Keep this window open to inspect the error.'
    exit 1
}
