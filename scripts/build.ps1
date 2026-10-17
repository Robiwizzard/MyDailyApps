# Builds the UI, then the single MyDailyApps executable (Windows PowerShell).
# Usage:  powershell -ExecutionPolicy Bypass -File scripts\build.ps1
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    # Check Go first: go.mod needs Go 1.27.2. Go 1.21 or newer downloads the
    # right version by itself; anything older cannot even read go.mod.
    $goCmd = Get-Command go -ErrorAction SilentlyContinue
    if (-not $goCmd) { throw "Go is not installed or not on the PATH. Install Go 1.27.2 from https://go.dev/dl/" }
    $goVersion = (& go env GOVERSION) 2>$null
    if (-not $goVersion) { $goVersion = (& go version) }
    if ($goVersion -match 'go(\d+)\.(\d+)') {
        if ([int]$Matches[1] -eq 1 -and [int]$Matches[2] -lt 21) {
            throw "Found $goVersion at $($goCmd.Source), but this project needs Go 1.27.2. Install it from https://go.dev/dl/ (and remove or move the old Go from the PATH)."
        }
    }
    Write-Host "Using $goVersion ($($goCmd.Source))"

    Push-Location ui
    npm install
    if ($LASTEXITCODE -ne 0) { throw "npm install failed" }
    npm run build
    if ($LASTEXITCODE -ne 0) { throw "UI build failed" }
    Set-Content -Path dist\.keep -Value ""
    Pop-Location

    $version = if ($env:VERSION) { $env:VERSION } else { "dev" }
    $ldflags = "-s -w -X main.Version=$version"
    if ($env:GOOGLE_CLIENT_ID) {
        $ldflags += " -X github.com/robiwizzard/mydailyapps/internal/config.DefaultGoogleClientID=$($env:GOOGLE_CLIENT_ID)"
        $ldflags += " -X github.com/robiwizzard/mydailyapps/internal/config.DefaultGoogleClientSecret=$($env:GOOGLE_CLIENT_SECRET)"
    }
    $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags $ldflags -o mydailyapps.exe ./cmd/mydailyapps
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
    Write-Host "Built mydailyapps.exe"
} finally {
    Pop-Location
}
