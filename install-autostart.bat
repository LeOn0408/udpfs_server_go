@echo off
chcp 65001 >nul
title udpfs_server_go - install autostart
set "ROOT=%~dp0"

powershell -NoProfile -Command ^
  "$startup = [Environment]::GetFolderPath('Startup'); ^
   $lnk = Join-Path $startup 'udpfs_server_go.lnk'; ^
   $s = (New-Object -ComObject WScript.Shell).CreateShortcut($lnk); ^
   $s.TargetPath = '%ROOT%start.bat'; ^
   $s.WorkingDirectory = '%ROOT%'; ^
   $s.WindowStyle = 7; ^
   $s.Description = 'UDPFS server for PS2 Neutrino'; ^
   $s.Save(); ^
   Write-Host ('Installed: ' + $lnk)"

echo.
echo The server will start automatically at Windows logon.
echo To remove: delete udpfs_server_go.lnk from the Startup folder.
pause
