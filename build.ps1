# Build Chromaframe for the current platform.
# Usage: .\build.ps1
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$exe = if ($IsWindows -or $env:OS -eq 'Windows_NT') { 'Chromaframe.exe' } else { 'chromaframe' }
Write-Host "Building $exe ..." -ForegroundColor Cyan
go build -trimpath -ldflags "-s -w" -o $exe .

Write-Host ""
Write-Host "Build OK: $PSScriptRoot\$exe" -ForegroundColor Green
Write-Host "Run:     .\$exe"
Write-Host "CLI:     .\$exe --render --html examples\demo.html --out out\demo.mp4"
