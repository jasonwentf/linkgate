@echo off
setlocal
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" -SourceDir "%~dp0." %*
set "result=%errorlevel%"
if not "%result%"=="0" echo Installation failed. Read the error above.
pause
exit /b %result%
