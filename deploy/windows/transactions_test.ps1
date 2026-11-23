# 纯快照/恢复测试：仅写随机临时目录，所有计划任务/进程/SID/启动目录均为 mock。
param()
. (Join-Path $PSScriptRoot 'common.ps1')
. (Join-Path $PSScriptRoot 'transaction.ps1')
$testSid = 'S-1-5-21-test-only'
foreach ($name in @('Get-LinkgateTask', 'New-LinkgateSnapshot', 'Read-LinkgateSnapshot')) {
    $body = (Get-Command $name).ScriptBlock.ToString().Replace('[System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value', '$testSid')
    Set-Item -Path "Function:$name" -Value ([scriptblock]::Create($body))
}
$root = Join-Path ([IO.Path]::GetTempPath()) ('linkgate-transaction-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $root | Out-Null
$script:events = [Collections.Generic.List[string]]::new()
function Get-ScheduledTask { return $script:task }
function Get-LinkgateStartupShortcut { return $script:shortcut }
function Test-LinkgateOwnedShortcut { param($InstallDir); if ($script:foreignShortcut) { throw 'foreign shortcut' } }
function Get-LinkgateProcesses {
    param($InstallDir, $Name)
    $running = if ($Name -eq 'linkgate') { $script:kernelRunning } else { $script:trayRunning }
    if ($running) { return @([PSCustomObject]@{ Name = $Name; Id = 123 }) }
    return @()
}
function Stop-LinkgateProcess {
    param($Process)
    $script:events.Add('stop:' + $Process.Name)
    if ($Process.Name -eq 'linkgate') { $script:kernelRunning = $false } else { $script:trayRunning = $false }
}
function Disable-ScheduledTask { $script:events.Add('disable'); $script:task.State = 'Disabled' }
function Stop-ScheduledTask { $script:events.Add('stop-task'); $script:kernelRunning = $false }
function Unregister-ScheduledTask { $script:events.Add('unregister'); $script:task = $null }
function Start-ScheduledTask { $script:events.Add('start-task'); $script:task.State = 'Running'; $script:kernelRunning = $true }
function Start-Process {
    param($FilePath, $ArgumentList, $WorkingDirectory, $WindowStyle)
    $script:events.Add('start:' + [IO.Path]::GetFileName($FilePath))
    if ([IO.Path]::GetFileName($FilePath) -eq 'linkgate.exe') { $script:kernelRunning = $true } else { $script:trayRunning = $true }
}
function Export-ScheduledTask {
    $command = [Security.SecurityElement]::Escape((Join-Path $script:install 'linkgate.exe'))
    return "<Task><Principals><Principal><UserId>$testSid</UserId><LogonType>$($script:task.LogonType)</LogonType></Principal></Principals><Actions><Exec><Command>$command</Command></Exec></Actions></Task>"
}
function schtasks.exe {
    $index = [array]::IndexOf($args, '/XML')
    $xml = [xml](Get-Content -LiteralPath $args[$index + 1] -Raw)
    $script:events.Add('register:' + $xml.Task.Principals.Principal.LogonType)
    $script:task = [PSCustomObject]@{ State = 'Ready'; LogonType = [string]$xml.Task.Principals.Principal.LogonType; Principal = [PSCustomObject]@{ UserId = $testSid }; Actions = @([PSCustomObject]@{ Execute = [string]$xml.Task.Actions.Exec.Command }) }
    $global:LASTEXITCODE = 0
}
function Set-Fixture([string]$Name, [string]$TaskState, [bool]$WithShortcut) {
    $script:install = Join-Path $root $Name
    New-Item -ItemType Directory -Path $script:install | Out-Null
    $script:shortcut = Join-Path $root ($Name + '-startup.lnk')
    $script:foreignShortcut = $false
    $script:events.Clear()
    $script:kernelRunning = $TaskState -eq 'Running'
    $script:trayRunning = $TaskState -eq 'Running'
    $script:task = $null
    if ($TaskState) {
        $script:task = [PSCustomObject]@{ State = $TaskState; LogonType = 'Interactive'; Principal = [PSCustomObject]@{ UserId = $testSid }; Actions = @([PSCustomObject]@{ Execute = (Join-Path $script:install 'linkgate.exe') }) }
        foreach ($file in @('linkgate.exe', 'linkgate-tray.exe', 'install-state.json')) { 'original-' + $file | Set-Content -LiteralPath (Join-Path $script:install $file) }
    }
    if ($WithShortcut) { 'original-shortcut' | Set-Content -LiteralPath $script:shortcut }
}
foreach ($taskState in @('Running', 'Disabled', 'Ready')) {
    Set-Fixture $taskState $taskState ($taskState -eq 'Running')
    $snapshot = Join-Path $root ($taskState + '-snapshot')
    $config = Join-Path $root 'custom config.yaml'
    $null = New-LinkgateSnapshot $script:install $snapshot $config
    if ($script:events.Count -ne 0) { throw 'Prepare snapshot changed running state.' }
    $before = [IO.File]::ReadAllBytes((Join-Path $script:install 'install-state.json'))
    foreach ($file in @('linkgate.exe', 'linkgate-tray.exe', 'install-state.json')) { 'new-' + $file | Set-Content -LiteralPath (Join-Path $script:install $file) }
    'new-shortcut' | Set-Content -LiteralPath $script:shortcut
    $script:task.LogonType = 'S4U'
    $script:task.State = 'Running'
    $script:kernelRunning = $true
    $script:trayRunning = $true
    Restore-LinkgateSnapshot $script:install $snapshot
    if ($script:task.LogonType -ne 'Interactive') { throw 'Original Interactive task not restored.' }
    if ($script:task.State -ne $taskState) { throw 'Original task enabled/running state not restored.' }
    if ($script:kernelRunning -ne ($taskState -eq 'Running') -or $script:trayRunning -ne ($taskState -eq 'Running')) { throw 'Original processes not restored.' }
    $after = [IO.File]::ReadAllBytes((Join-Path $script:install 'install-state.json'))
    if ([Convert]::ToBase64String($before) -ne [Convert]::ToBase64String($after)) { throw 'Metadata bytes not restored.' }
    if ((Get-Content -LiteralPath (Join-Path $script:install 'linkgate.exe') -Raw).Trim() -ne 'original-linkgate.exe') { throw 'EXE not restored.' }
    if ($taskState -eq 'Running') {
        if ((Get-Content -LiteralPath $script:shortcut -Raw).Trim() -ne 'original-shortcut') { throw 'Shortcut not restored.' }
    } elseif (Test-Path -LiteralPath $script:shortcut) { throw 'New shortcut not removed from Startup.' }
    $script:events.Clear()
    $state = Get-Content -LiteralPath (Join-Path $snapshot 'snapshot.json') -Raw | ConvertFrom-Json
    $state.SID = 'foreign-sid'
    $state | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $snapshot 'snapshot.json') -Encoding UTF8
    $rejected = $false
    try { Restore-LinkgateSnapshot $script:install $snapshot } catch { $rejected = $true }
    if (-not $rejected -or $script:events.Count -ne 0) { throw 'Foreign snapshot mutated the installation.' }
}
Set-Fixture 'fresh' '' $false
$freshSnapshot = Join-Path $root 'fresh-snapshot'
$null = New-LinkgateSnapshot $script:install $freshSnapshot (Join-Path $root 'fresh.yaml')
'new-exe' | Set-Content -LiteralPath (Join-Path $script:install 'linkgate.exe')
'new-shortcut' | Set-Content -LiteralPath $script:shortcut
Restore-LinkgateSnapshot $script:install $freshSnapshot
if (Test-Path -LiteralPath (Join-Path $script:install 'linkgate.exe')) { throw 'Fresh install EXE was not archived on rollback.' }
if (Test-Path -LiteralPath $script:shortcut) { throw 'Fresh startup item survived rollback.' }
Write-Host 'PASS: snapshot is read-only; Interactive/S4U migration rollback restores XML, exact metadata, shortcut, binaries and running/disabled/idle states; foreign snapshot rejected. No real tasks/processes were changed.'
