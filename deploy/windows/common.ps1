# Shared by the Windows installer and updater. PowerShell 5.1+, current user only.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-LinkgateBinary([string]$SourceDir, [string]$Name) {
    foreach ($file in @("$Name.exe", "$Name-windows-amd64.exe")) {
        $path = Join-Path $SourceDir $file
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            return (Resolve-Path -LiteralPath $path).Path
        }
    }
    throw "Missing $Name.exe in $SourceDir. Extract the complete Windows amd64 ZIP first."
}

function ConvertTo-LinkgateVersion([string]$Value) {
    if ($Value -notmatch '^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$') {
        throw "Unrecognized Linkgate version: $Value"
    }
    $parts = @($Matches[1], $Matches[2], $Matches[3])
    $pre = ''
    if ($Matches.ContainsKey(4)) { $pre = $Matches[4] }
    foreach ($id in ($pre -split '\.')) {
        if ($pre -and (-not $id -or $id -match '^0[0-9]+$')) {
            throw "Invalid prerelease version: $Value"
        }
    }
    return [PSCustomObject]@{ Parts = $parts; Pre = $pre }
}

function Compare-LinkgateNumber([string]$Left, [string]$Right) {
    if ($Left.Length -ne $Right.Length) { return [Math]::Sign($Left.Length - $Right.Length) }
    return [Math]::Sign([string]::CompareOrdinal($Left, $Right))
}

function Compare-LinkgateVersion([string]$Left, [string]$Right) {
    $a = ConvertTo-LinkgateVersion $Left
    $b = ConvertTo-LinkgateVersion $Right
    for ($i = 0; $i -lt 3; $i++) {
        $cmp = Compare-LinkgateNumber $a.Parts[$i] $b.Parts[$i]
        if ($cmp -ne 0) { return $cmp }
    }
    if (-not $a.Pre -and -not $b.Pre) { return 0 }
    if (-not $a.Pre) { return 1 }
    if (-not $b.Pre) { return -1 }
    $ap = $a.Pre -split '\.'
    $bp = $b.Pre -split '\.'
    for ($i = 0; $i -lt [Math]::Min($ap.Count, $bp.Count); $i++) {
        $an = $ap[$i] -match '^[0-9]+$'
        $bn = $bp[$i] -match '^[0-9]+$'
        if ($an -and $bn) { $cmp = Compare-LinkgateNumber $ap[$i] $bp[$i] }
        elseif ($an -ne $bn) { if ($an) { $cmp = -1 } else { $cmp = 1 } }
        else { $cmp = [Math]::Sign([string]::CompareOrdinal($ap[$i], $bp[$i])) }
        if ($cmp -ne 0) { return $cmp }
    }
    return [Math]::Sign($ap.Count - $bp.Count)
}

function Get-LinkgateVersion([string]$Binary, [string]$Name) {
    $output = & $Binary -version
    if ($LASTEXITCODE -ne 0) { throw "Cannot read version from $Binary" }
    $text = $output -join "`n"
    if ($text -notmatch ('^' + [regex]::Escape($Name) + ' ([^\s]+)')) {
        throw "Unexpected version output from $Binary. Install a matching kernel/tray release."
    }
    $value = $Matches[1]
    $null = ConvertTo-LinkgateVersion $value
    return $value
}

function Get-LinkgateInstallDir {
    if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA is not set. Run this on Windows as your normal user.' }
    return Join-Path $env:LOCALAPPDATA 'Linkgate'
}

function Get-LinkgateConfig([string]$InstallDir) {
    $statePath = Join-Path $InstallDir 'install-state.json'
    if (Test-Path -LiteralPath $statePath) {
        $state = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
        return [string]$state.Config
    }
    $task = Get-LinkgateTask $InstallDir
    if ($null -ne $task) {
        $args = $task.Actions[0].Arguments
        if ($args -match '^-config "([^"]+)"$') { return $Matches[1] }
        throw 'Cannot recover the config path from the existing task. Keep the task unchanged and repair the installation manually.'
    }
    return Join-Path $env:USERPROFILE '.linkgate\config.yaml'
}

function Get-LinkgateTask([string]$InstallDir) {
    $task = Get-ScheduledTask -TaskName 'Linkgate' -TaskPath '\' -ErrorAction SilentlyContinue
    if ($null -eq $task) { return $null }
    $expected = Join-Path $InstallDir 'linkgate.exe'
    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $owner = $task.Principal.UserId
    if ($owner -ne $sid) {
        try {
            $account = New-Object System.Security.Principal.NTAccount($owner)
            $owner = $account.Translate([System.Security.Principal.SecurityIdentifier]).Value
        } catch { throw 'Cannot verify the existing Linkgate task owner; no changes made.' }
    }
    if ($owner -ne $sid -or @($task.Actions).Count -ne 1 -or $task.Actions[0].Execute -ne $expected) {
        throw 'The Linkgate task belongs to another installation/user; no changes made.'
    }
    return $task
}

function Get-LinkgateProcesses([string]$InstallDir, [string]$Name) {
    $expected = Join-Path $InstallDir "$Name.exe"
    return @(Get-Process -Name $Name -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $expected })
}

function Stop-LinkgateProcess([System.Diagnostics.Process]$Process) {
    Stop-Process -Id $Process.Id -Force
    if (-not $Process.WaitForExit(10000)) { throw "Process $($Process.Id) did not exit; refusing to replace binaries." }
}

function Start-Linkgate([string]$InstallDir, [string]$Config) {
    Start-ScheduledTask -TaskName 'Linkgate' -TaskPath '\'
    $tray = Join-Path $InstallDir 'linkgate-tray.exe'
    if (@(Get-LinkgateProcesses $InstallDir 'linkgate-tray').Count -eq 0) {
        Start-Process -FilePath $tray -ArgumentList "-config `"$Config`"" -WorkingDirectory $InstallDir
    }
    Start-Sleep -Seconds 2
    if (@(Get-LinkgateProcesses $InstallDir 'linkgate').Count -eq 0 -or
        @(Get-LinkgateProcesses $InstallDir 'linkgate-tray').Count -eq 0) {
        throw 'A process exited during startup. Check the Linkgate scheduled task and your config/ports.'
    }
}
