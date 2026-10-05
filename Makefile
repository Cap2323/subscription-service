.PHONY: init up down test integration bench vet build vuln
init:
	go run ./cmd/devtoken -init
up:
	docker compose up --build -d
down:
	docker compose down
test:
	go test -race -count=1 ./...
integration:
	go test -race -tags=integration -count=1 ./tests/integration
bench:
	go test -run '^$$' -bench . -benchmem ./internal/hashring
vet:
	go vet ./...
build:
	go build -mod=readonly -o bin/api ./cmd/api
	go build -mod=readonly -o bin/admin ./cmd/admin
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
