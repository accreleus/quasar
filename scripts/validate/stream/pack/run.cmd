@echo off
rem Windows launcher. All the work is in the Node entry; see README.txt.
cd /d "%~dp0"
where node >nul 2>&1
if errorlevel 1 (
  echo Node 22 is needed: https://nodejs.org 1>&2
  echo RESULT status=error target=e2e-pack zip= reason=no-node
  exit /b 2
)
if "%~1"=="" goto run0
if "%~2"=="" goto run1
if "%~3"=="" goto run2
if "%~4"=="" goto run3
if "%~5"=="" goto run4
if "%~6"=="" goto run5
node scripts\validate\stream\pack-run.mjs "%~1" "%~2" "%~3" "%~4" "%~5" "%~6"
exit /b %ERRORLEVEL%
:run0
node scripts\validate\stream\pack-run.mjs
exit /b %ERRORLEVEL%
:run1
node scripts\validate\stream\pack-run.mjs "%~1"
exit /b %ERRORLEVEL%
:run2
node scripts\validate\stream\pack-run.mjs "%~1" "%~2"
exit /b %ERRORLEVEL%
:run3
node scripts\validate\stream\pack-run.mjs "%~1" "%~2" "%~3"
exit /b %ERRORLEVEL%
:run4
node scripts\validate\stream\pack-run.mjs "%~1" "%~2" "%~3" "%~4"
exit /b %ERRORLEVEL%
:run5
node scripts\validate\stream\pack-run.mjs "%~1" "%~2" "%~3" "%~4" "%~5"
exit /b %ERRORLEVEL%
