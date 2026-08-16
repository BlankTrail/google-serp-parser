@echo off
setlocal
cd /d "%~dp0"

rem Where the interface listens, and so where the browser is sent. It is one
rem value rather than two, so the address the program prints and the address
rem opened for the reader cannot drift apart.
if not defined GSERP_ADDR set "GSERP_ADDR=127.0.0.1:8080"

rem The program is named by its full path, never by its bare name. A machine
rem configured not to run programs out of the working directory finds nothing
rem at all otherwise, and says only that the command is not recognised.
if not exist "%~dp0gserp.exe" (
    where go >nul 2>nul
    if errorlevel 1 (
        echo Neither gserp.exe nor Go was found in this folder.
        echo Download a release build, or install Go and run: go build -o gserp.exe ./cmd/gserp
        pause
        exit /b 1
    )
    echo gserp.exe not found - building it from source, this takes a moment...
    go build -o gserp.exe ./cmd/gserp || (
        echo Build failed.
        pause
        exit /b 1
    )
)

rem The interface gets a window of its own. What it prints there is the address
rem below and whether it can run a job at all, and closing that window is how it
rem is stopped.
start "gserp" "%~dp0gserp.exe" serve -addr %GSERP_ADDR%

rem The browser follows the interface rather than leading it. The socket is
rem bound before the program opens anything else, so a browser that waits a
rem moment lands on a page rather than on a refusal.
timeout /t 2 /nobreak >nul 2>&1
start "" "http://%GSERP_ADDR%/"
endlocal
