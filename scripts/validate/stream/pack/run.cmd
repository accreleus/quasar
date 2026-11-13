@echo off
rem Windows launcher. All the work is in the Node entry; see README.txt.
cd /d "%~dp0"
where node >nul 2>&1
if errorlevel 1 (
  echo Node 22 is needed: https://nodejs.org 1>&2
  echo RESULT status=error target=e2e-pack zip= reason=no-node
  exit /b 2
)
node scripts\validate\stream\pack-run.mjs %*
exit /b %ERRORLEVEL%
