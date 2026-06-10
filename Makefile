.PHONY: run build tidy migrate lint test test-unit test-integration test-all e2e e2e-deploy e2e-failure e2e-rollback e2e-diagnosis e2e-dry-run

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

# ── E2E Tests ──────────────────────────────────────────────────────────────────
# Requires: E2E_AUTH_TOKEN, E2E_GITHUB_TOKEN, E2E_AWS_ACCOUNT_ID, E2E_AWS_ROLE_ARN
# Optional: E2E_OPSPILOT_URL (default http://localhost:8080), E2E_PARALLEL, E2E_CLEANUP

E2E_FLAGS ?=

# Run a specific suite
e2e-deploy:
	go run ./e2e/cmd/runner -suite=deploy $(E2E_FLAGS)

e2e-failure:
	go run ./e2e/cmd/runner -suite=failure $(E2E_FLAGS)

e2e-rollback:
	go run ./e2e/cmd/runner -suite=rollback $(E2E_FLAGS)

e2e-diagnosis:
	go run ./e2e/cmd/runner -suite=diagnosis $(E2E_FLAGS)

# Full E2E suite (all scenarios)
e2e:
	go run ./e2e/cmd/runner -suite=all $(E2E_FLAGS)

# Print what would run without executing
e2e-dry-run:
	go run ./e2e/cmd/runner -suite=all -dry-run $(E2E_FLAGS)
