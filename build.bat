@echo off
setlocal enabledelayedexpansion

REM ============================================================
REM  mmx build script
REM  usage: build.bat [version]
REM    version  - optional, e.g. v1.19.1 (default: read from VERSION file, fallback v0.0.0)
REM ============================================================

cd /d "%~dp0"

REM --- determine version ---
set "MMX_VERSION=%~1"
if "%MMX_VERSION%"=="" (
    if exist "internal\core\VERSION" (
        set /p MMX_VERSION=<"internal\core\VERSION"
    )
    if "!MMX_VERSION!"=="" set "MMX_VERSION=v0.0.0"
)

echo [1/3] Version: %MMX_VERSION%

REM --- write VERSION embed file (no trailing newline) ---
powershell -NoProfile -Command "[System.IO.File]::WriteAllText('internal\core\VERSION', '%MMX_VERSION%')"
if errorlevel 1 (
    echo ERROR: failed to write version file
    exit /b 1
)

REM --- ensure bin directory ---
if not exist "bin" mkdir "bin"

REM --- build ---
echo [2/3] Building mmx.exe ...
go build -ldflags="-s -w" -o "bin\mmx.exe" .
if errorlevel 1 (
    echo ERROR: build failed
    exit /b 1
)

REM --- done ---
echo [3/3] Done: bin\mmx.exe
for %%F in ("bin\mmx.exe") do echo        size: %%~zF bytes, version: %MMX_VERSION%
