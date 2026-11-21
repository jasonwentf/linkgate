# Parser and pure-helper tests only. Does not install, stop processes, access
# GitHub, register tasks, or read ~/.linkgate. Run with PowerShell 5.1+ / pwsh.
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
foreach ($file in @(Get-ChildItem -LiteralPath $PSScriptRoot -Filter '*.ps1')) {
    $tokens = $null
    $errors = $null
    $null = [System.Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$tokens, [ref]$errors)
    if ($errors.Count -gt 0) { throw ($errors | Out-String) }
}
. (Join-Path $PSScriptRoot 'common.ps1')
foreach ($case in @(
    @('0.2.2', '0.2.1', 1), @('v0.2.2', '0.2.2', 0),
    @('0.10.0', '0.9.9', 1), @('0.2.2-next', '0.2.2', -1),
    @('1.0.0-alpha.2', '1.0.0-alpha.10', -1),
    @('1.0.0-alpha.10', '1.0.0-beta', -1),
    @('1.0.0-1', '1.0.0-alpha', -1), @('1.0.0-alpha', '1.0.0-alpha.1', -1),
    @('1.0.0+one', '1.0.0+two', 0), @('0.2.1', '0.2.2', -1)
)) {
    $actual = Compare-LinkgateVersion $case[0] $case[1]
    if ($actual -ne $case[2]) { throw "Version comparison failed: $case => $actual" }
}
foreach ($invalid in @('garbage', '01.2.3', '1.2', '1.2.3-01', '1.2.3-alpha..1')) {
    $rejected = $false
    try { $null = ConvertTo-LinkgateVersion $invalid } catch { $rejected = $true }
    if (-not $rejected) { throw "Accepted invalid version: $invalid" }
}
# Mock only OS boundaries to verify config recovery and task-owner checks.
$testSid = 'S-1-5-21-test-only'
$install = [IO.Path]::Combine([IO.Path]::GetTempPath(), 'linkgate-script-test')
function Get-Content { return '{"Config":"custom-config.yaml"}' }
function Get-ScheduledTask {
    return [PSCustomObject]@{ Principal = [PSCustomObject]@{ UserId = $testSid }; Actions = @([PSCustomObject]@{ Execute = (Join-Path $install 'linkgate.exe'); Arguments = '-config "old custom config.yaml"' }) }
}
# Get-LinkgateTask normally obtains the SID from WindowsIdentity; this adapter
# replaces that expression for pure tests, without calling any Windows API.
$taskBody = (Get-Command Get-LinkgateTask).ScriptBlock.ToString().Replace('[System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value', '$testSid')
Set-Item -Path Function:Get-LinkgateTask -Value ([scriptblock]::Create($taskBody))
function Test-Path { return $true }
if ((Get-LinkgateConfig $install) -ne 'custom-config.yaml') { throw 'Install state config path not retained.' }
function Test-Path { return $false }
if ((Get-LinkgateConfig $install) -ne 'old custom config.yaml') { throw 'Legacy task config path not recovered.' }
function Get-ScheduledTask {
    return [PSCustomObject]@{ Principal = [PSCustomObject]@{ UserId = $testSid }; Actions = @([PSCustomObject]@{ Execute = 'another-installation.exe'; Arguments = '' }) }
}
$rejected = $false
try { $null = Get-LinkgateTask $install } catch { $rejected = $true }
if (-not $rejected) { throw 'Accepted a task targeting another installation.' }
Write-Host 'PASS: PowerShell parser, semantic versions, config recovery and task isolation. Windows installation was not executed.'
