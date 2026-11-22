@echo off
chcp 65001 >nul
title udpfs_server_go - stop
set "ROOT=%~dp0"

if not exist "%ROOT%udpfs.pid" (
  echo No PID file - not running (or started manually).
  exit /b 0
)

set /p PID=<"%ROOT%udpfs.pid"
taskkill /PID %PID% /F 2>nul
if errorlevel 1 (
  echo Process %PID% not found.
) else (
  echo Stopped PID %PID%.
)
del "%ROOT%udpfs.pid" 2>nul
