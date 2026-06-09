.PHONY: run build tidy migrate lint test test-unit test-integration test-all

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

# ── Tests ─────────────────────────────────────────────────────────────────────

# Unit tests — no external dependencies, runs in ~1s
test-unit:
	go test -count=1 -race \
		./internal/aws/... \
		./internal/github/... \
		./internal/diagnosis/... \
		./internal/terminal/...

# Integration tests — require a running postgres
# Usage: make test-integration TEST_DATABASE_URL="postgres://convdeploy:convdeploy@localhost:5432/convdeploy_test?sslmode=disable"
test-integration:
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -count=1 -race -timeout 120s \
		./pkg/models/... \
		./internal/deploy/... \
		./internal/webhooks/...

# Full suite: unit + integration (set TEST_DATABASE_URL or DB tests auto-skip)
test: test-unit test-integration

# Run everything including the db-backed tests — starts docker compose first
test-all:
	docker compose up -d
	@echo "Waiting for postgres to be ready..."
	@until docker compose exec -T postgres pg_isready -U convdeploy > /dev/null 2>&1; do sleep 1; done
	TEST_DATABASE_URL="postgres://convdeploy:convdeploy@localhost:5432/convdeploy?sslmode=disable" \
		go test -count=1 -race -timeout 120s ./...

# TypeScript type check (no emit)
test-ts:
	cd frontend && npx tsc --noEmit

docker-up:
	docker compose up -d

docker-down:
	docker compose down
