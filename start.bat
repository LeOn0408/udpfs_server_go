@echo off
chcp 65001 >nul
title udpfs_server_go - start
set "ROOT=%~dp0"

if exist "%ROOT%udpfs.pid" (
  set /p PID=<"%ROOT%udpfs.pid"
  tasklist /FI "PID eq %PID%" 2>nul | findstr /I /C:"%PID%" >nul
  if not errorlevel 1 (
    echo Already running, PID %PID%
    exit /b 0
  )
)

powershell -NoProfile -Command "$p = Start-Process -FilePath '%ROOT%udpfs_server_go.exe' -ArgumentList '--config','%ROOT%config.json' -WorkingDirectory '%ROOT%' -WindowStyle Hidden -PassThru; $p.Id | Set-Content '%ROOT%udpfs.pid'; Write-Host ('Started PID ' + $p.Id)"
echo Log: stdout/stderr are inherited by the process; use --verbose manually for debugging.
