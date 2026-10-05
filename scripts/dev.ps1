param([ValidateSet("up","down","test","token","logs")] [string]$Action = "up")
$ErrorActionPreference = "Stop"
if ($Action -eq "up") {
    if (-not (Test-Path .env)) { go run ./cmd/devtoken -init; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
    docker compose up --build -d
} elseif ($Action -eq "down") { docker compose down }
elseif ($Action -eq "test") { go test -race -count=1 ./... }
elseif ($Action -eq "token") { go run ./cmd/devtoken }
elseif ($Action -eq "logs") { docker compose logs -f app }
exit $LASTEXITCODE
