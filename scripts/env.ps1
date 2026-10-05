# Dot-source from the project root: . ./scripts/env.ps1
$ErrorActionPreference = "Stop"
if (-not (Test-Path .env)) { throw "Run go run ./cmd/devtoken -init first" }
Get-Content .env | ForEach-Object {
    if ($_ -match '^([A-Z][A-Z0-9_]*)=(.*)$') {
        [Environment]::SetEnvironmentVariable($matches[1], $matches[2], "Process")
    }
}
