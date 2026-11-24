# 只测 UTF-8 输出捕获及临时日志清理后的持久副本，不注册任务、不读取用户配置。
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$tokens = $null
$errors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'setup-helper.ps1'), [ref]$tokens, [ref]$errors)
if ($errors.Count -gt 0) { throw ($errors | Out-String) }
$names = @('ConvertTo-InstallerArgument', 'Invoke-LinkgateInstallerCommand', 'Save-InstallerError')
$functions = $ast.FindAll({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $names }, $false)
foreach ($definition in $functions) {
    $body = $definition.Body.Extent.Text
    Set-Item -Path ('Function:' + $definition.Name) -Value ([scriptblock]::Create($body.Substring(1, $body.Length - 2)))
}
if ($functions.Count -ne $names.Count) { throw 'Diagnostic helper functions missing.' }
$root = Join-Path ([IO.Path]::GetTempPath()) ('linkgate-diagnostics-test-' + [guid]::NewGuid().ToString('N'))
$session = Join-Path $root 'is-test.tmp'
New-Item -ItemType Directory -Path $session | Out-Null
$oldProfile = $env:USERPROFILE
try {
    $env:USERPROFILE = Join-Path $root 'fake-profile'
    $install = Join-Path $root 'fake-install'
    $temporary = Join-Path $session 'linkgate-configure-error.log'
    $message = "schtasks XML: 拒绝访问`nPowerShell S4U: 完整原文第二行`n末尾标记"
    Save-InstallerError $message $temporary $install
    $name = 'is-test.tmp-linkgate-configure-error.log'
    $persistent = Join-Path (Join-Path $env:USERPROFILE '.linkgate\install-errors') $name
    $secondary = Join-Path (Join-Path $install 'logs') $name
    Move-Item -LiteralPath $temporary -Destination (Join-Path $root 'archived-temp-error.log')
    foreach ($copy in @($persistent, $secondary)) {
        if (-not (Test-Path -LiteralPath $copy)) { throw 'Error copy did not survive temporary-file cleanup.' }
        if ((Get-Content -LiteralPath $copy -Raw).TrimEnd([char[]]"`r`n") -ne $message) { throw 'Persistent diagnostic was truncated or garbled.' }
    }
    # Native stderr 在 PS5.1 + Stop 下也不能提前终止或死锁；两路输出均超过管道缓冲区。
    $shell = (Get-Command powershell.exe).Source
    $script = '[Console]::OutputEncoding=[Text.Encoding]::UTF8; [Console]::WriteLine("stdout中文" + ("o" * 100000)); [Console]::Error.WriteLine("stderr完整中文" + ("x" * 200000) + "末尾标记"); exit 23'
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($script))
    $rejected = $false
    try { $null = Invoke-LinkgateInstallerCommand $shell @('-NoProfile', '-NonInteractive', '-EncodedCommand', $encoded) }
    catch {
        $text = $_.Exception.Message
        $rejected = $text.Contains('退出码 23') -and $text.Contains('stdout中文') -and $text.Contains('stderr完整中文') -and $text.Contains('末尾标记') -and $text.Contains(('x' * 200000))
    }
    if (-not $rejected) { throw 'Native UTF-8 stdout/stderr or the exit code was not preserved completely.' }
} finally { $env:USERPROFILE = $oldProfile }
Write-Host 'PASS: complete UTF-8 native stdout/stderr, nonzero exit, persistent error copies after temporary cleanup. No tasks or user installation were changed.'
