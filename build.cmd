@echo off
rem ============================================================
rem  3z9wOpt - build
rem
rem  Two modes (2026-10-19). See the release doc for the full picture.
rem  NOTE: this file must stay pure ASCII - cmd.exe reads .bat/.cmd
rem        in the OEM codepage (936 on Chinese Windows), so any
rem        non-ASCII literal here would turn into garbage paths.
rem
rem    build.cmd            -> DEV build  (bin\3z9wOpt_v<ver>-dev.exe)
rem                            version ends with -dev: no update check,
rem                            no fingerprint check. Build as often as
rem                            you like, never publish it.
rem
rem    build.cmd release    -> RELEASE build (bin\3z9wOpt_v<ver>.exe)
rem                            allocates a build number (YYYYMMDD.N),
rem                            repacks download\*.zip, appends the build
rem                            ledger, rewrites both version.json files,
rem                            syncs the site page, copies the exe.
rem
rem  Why two modes: one version number used to carry three jobs
rem  (release label / update comparison key / official fingerprint key).
rem  Rebuilding the same version then forced a choice between
rem  "old users get no update" and "old users fail the self check".
rem  Now version = feature set, build number = which release of it.
rem
rem  Works from any folder: all paths are derived from %~dp0
rem
rem  WARNING 1: GOCACHE / GOTMPDIR / GOPATH must point to the
rem             _gocache / _gotmp / _gopath folders NEXT TO THIS FILE.
rem  WARNING 2: _toolchain (Go compiler) and _gopath (modules) are
rem             REQUIRED. Delete them and this source can never be
rem             built offline again (GOPROXY=off, no network fetch).
rem  WARNING 3: If you changed design\ui-prototype.html, run
rem             "node build_frontend.js" FIRST, or the exe will
rem             still embed the previous frontend build.
rem ============================================================
setlocal
cd /d "%~dp0"
set "ROOT=%~dp0"
set "GOCACHE=%ROOT%_gocache"
set "GOTMPDIR=%ROOT%_gotmp"
set "GOPATH=%ROOT%_gopath"
set "GOFLAGS=-mod=mod"
set "GOPROXY=off"

rem 2026-10-19: amd64 is pinned on purpose. The startup enum reads
rem HKLM\Software\...\Run and relies on the 64-bit registry view not
rem being redirected by WOW64; a 32-bit build would duplicate every
rem entry (it falls into Wow6432Node, which the code also reads).
set "GOOS=windows"
set "GOARCH=amd64"

rem Version is defined ONCE here. It means "feature set" - bump it only
rem when you decide to announce a new release. Bug fixes and UI tweaks
rem just get a new build number from tools/relgen.
set "APPVER=1.1.0"

rem ---- args: release = publishable build; quiet = automation, no pause ----
set "MODE=dev"
set "QUIET="
if not "%~1"=="" for %%a in (%*) do (
  if /i "%%a"=="release" set "MODE=release"
  if /i "%%a"=="quiet" set "QUIET=1"
)

if not exist "%GOTMPDIR%" mkdir "%GOTMPDIR%"
if not exist "%GOCACHE%" mkdir "%GOCACHE%"
if not exist "%ROOT%bin" mkdir "%ROOT%bin"

if /i "%MODE%"=="release" goto :plan_release

rem ---------- everyday build (dev) ----------
set "VERSTR=%APPVER%-dev"
set "EXE=%ROOT%bin\3z9wOpt_%APPVER%-dev.exe"
set "LDFLAGS=-s -w -H windowsgui -X main.appVersion=%VERSTR%"
echo.
echo   Building 3z9wOpt [DEV] ...
echo   version  = %VERSTR%   (update check / fingerprint verify disabled)
goto :build

rem ---------- release build ----------
:plan_release
set "PATH=%ROOT%_toolchain\go\bin;%PATH%"
for /f "delims=" %%i in ('go run .\tools\relgen next --ver %APPVER%') do set "BUILDNUM=%%i"
if "%BUILDNUM%"=="" goto :nobuild
set "BUILDDATE=%BUILDNUM:~0,4%-%BUILDNUM:~4,2%-%BUILDNUM:~6,2%"
set "VERSTR=%APPVER%"
set "EXE=%ROOT%bin\3z9wOpt_v%APPVER%.exe"
set "LDFLAGS=-s -w -H windowsgui -X main.appVersion=%APPVER% -X main.buildDate=%BUILDDATE% -X main.buildNum=%BUILDNUM%"
echo.
echo   Building 3z9wOpt [RELEASE] ...
echo   version  = %APPVER%
echo   build    = %BUILDNUM%   (%BUILDDATE%)
goto :build

rem ---------- compile ----------
:build
echo   GOCACHE  = %GOCACHE%
echo   GOPATH   = %GOPATH%
echo.
"%ROOT%_toolchain\go\bin\go.exe" build -tags desktop,production -ldflags "%LDFLAGS%" -o "%EXE%" .
if errorlevel 1 goto :fail

echo.
echo   OK: %EXE%
dir "%EXE%" | findstr /R "3z9wOpt"
echo   If size is below 14 MB the build is wrong - do not use it.

if /i "%MODE%"=="release" goto :publish
echo.
echo   [DEV] This build will NOT check for updates and will NOT pass
echo   the "official release" check. Use "build.cmd release" to ship.
goto :done

rem ---------- publish: copy exe / ledger / zip / version.json / site ----------
:publish
echo.
echo   Copying exe to download\ ...
copy /y "%EXE%" "%ROOT%opt.3z9w.com\download\3z9wOpt_v%APPVER%.exe" >nul

echo.
echo   Publishing (zip / ledger / version.json / site page) ...
go run .\tools\relgen publish --ver %APPVER% --build %BUILDNUM% --exe "%EXE%"
if errorlevel 1 goto :fail
goto :done

:nobuild
echo.
echo   BUILD FAILED - could not allocate a build number.
echo   Check tools\relgen and the build ledger.
goto :fail

:fail
echo.
echo   BUILD FAILED - see errors above.
set "RC=1"

:done
rem "quiet" = called from automation; do not wait for a keypress
if "%QUIET%"=="1" exit /b %RC%
echo.
pause
exit /b %RC%
