@echo off
chcp 65001 >nul
title udpfs_server_go - restart
set "ROOT=%~dp0"

call "%ROOT%stop.bat"
timeout /t 2 /nobreak >nul
call "%ROOT%start.bat"
