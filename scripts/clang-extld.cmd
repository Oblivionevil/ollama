@echo off
powershell -ExecutionPolicy Bypass -File "%~dp0lld-link-wrapper.ps1" %*
exit /b %ERRORLEVEL%
