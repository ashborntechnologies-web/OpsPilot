.PHONY: run build tidy migrate lint

run:
	go run ./cmd/api/main.go

build:
	go build -o bin/api ./cmd/api/main.go

tidy:
	go mod tidy

lint:
	golangci-lint run ./...

migrate:
	@echo "Migrations run automatically on startup via RunMigrations()"

docker-up:
	docker compose up -d

docker-down:
	docker compose down
