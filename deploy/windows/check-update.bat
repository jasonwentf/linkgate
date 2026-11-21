@echo off
setlocal
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0update.ps1" -CheckOnly
set "result=%errorlevel%"
if not "%result%"=="0" echo Version check failed. Visit the GitHub releases page manually.
pause
exit /b %result%
