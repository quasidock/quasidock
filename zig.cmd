@echo off
if "%1"=="build" goto do_build
if "%1"=="version" goto do_version
echo Unknown command %1
exit /b 1

:do_build
shift
set "ARGS="
:loop
if "%1"=="" goto run_build
set "ARGS=%ARGS% %1"
shift
goto loop

:run_build
go run "%~dp0scripts\build.go" %ARGS%
exit /b %errorlevel%

:do_version
go version
exit /b 0
