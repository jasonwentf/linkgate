# 在 Windows 上安装 Linkgate（内核 + 托盘）。
# 用法（PowerShell，普通用户即可，不需要管理员）：
#   .\install.ps1 -SourceDir .\bin\dist -Config "$env:USERPROFILE\.linkgate\config.yaml"
param(
    [string]$SourceDir = "$PSScriptRoot\..\..\bin\dist",
    [string]$Config = "$env:USERPROFILE\.linkgate\config.yaml"
)

$InstallDir = "$env:LOCALAPPDATA\Linkgate"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path (Split-Path $Config) | Out-Null

Copy-Item "$SourceDir\linkgate-windows-amd64.exe" "$InstallDir\linkgate.exe" -Force
if (Test-Path "$SourceDir\linkgate-tray-windows-amd64.exe") {
    Copy-Item "$SourceDir\linkgate-tray-windows-amd64.exe" "$InstallDir\linkgate-tray.exe" -Force
}

if (-not (Test-Path $Config)) {
    Write-Host "缺少配置文件 $Config"
    Write-Host "先运行: $InstallDir\linkgate.exe -init -config `"$Config`""
    Write-Host "然后按你的线路改写，再重新执行本脚本。"
    exit 1
}

# 内核：登录时启动，崩溃自动重启
$action = New-ScheduledTaskAction -Execute "$InstallDir\linkgate.exe" -Argument "-config `"$Config`""
$trigger = New-ScheduledTaskTrigger -AtLogOn
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName "Linkgate" -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName "Linkgate"

# 托盘：放进启动目录
if (Test-Path "$InstallDir\linkgate-tray.exe") {
    $startup = [Environment]::GetFolderPath('Startup')
    $ws = New-Object -ComObject WScript.Shell
    $sc = $ws.CreateShortcut("$startup\Linkgate Tray.lnk")
    $sc.TargetPath = "$InstallDir\linkgate-tray.exe"
    $sc.Save()
    Start-Process "$InstallDir\linkgate-tray.exe"
}

Write-Host "完成。验证: curl http://127.0.0.1:8765/health"
