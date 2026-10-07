@echo off
setlocal
cd /d "%~dp0"
if exist "bin\atelier.exe" (
  "bin\atelier.exe"
) else (
  go run .
)
if errorlevel 1 pause
