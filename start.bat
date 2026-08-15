@echo off
setlocal
cd /d "%~dp0"

if exist "gserp.exe" (
    set "GSERP=gserp.exe"
) else (
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
    set "GSERP=gserp.exe"
)

"%GSERP%" doctor
if errorlevel 1 (
    echo.
    echo The preflight check above did not pass. Fix the findings and run this again.
    pause
    exit /b 1
)

pause
endlocal
