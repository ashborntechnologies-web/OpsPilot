// Package testutil provides helpers shared across integration tests.
// DB integration tests require TEST_DATABASE_URL to be set; if it is not,
// tests calling NewTestDB are skipped automatically.
//
// Quick start:
//
//	docker compose up -d
//	TEST_DATABASE_URL="postgres://convdeploy:convdeploy@localhost:5432/convdeploy_test?sslmode=disable" \
//	  go test ./...
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
)

// NewTestDB opens a connection to the test database, runs all migrations, and
// registers a cleanup that truncates all user-data tables between tests so tests
// are isolated without needing a fresh schema per run.
func NewTestDB(t *testing.T) *models.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB integration test")
	}

	db, err := models.NewDB(url)
	if err != nil {
		t.Fatalf("testutil.NewTestDB: connect: %v", err)
	}

	if err := models.RunMigrations(db); err != nil {
		db.Close()
		t.Fatalf("testutil.NewTestDB: migrate: %v", err)
	}

	t.Cleanup(func() {
		truncateAll(t, db)
		db.Close()
	})

	return db
}

// truncateAll removes all rows from the leaf tables in dependency order so FK
// constraints are satisfied. New tables should be added here when created.
func truncateAll(t *testing.T, db *models.DB) {
	t.Helper()
	tables := []string{
		"operational_events",
		"env_vars",
		"webhooks",
		"deployments",
		"environments",
		"platform_stacks",
		"projects",
		"aws_accounts",
		"conversations",
		"incidents",
		"users",
	}
	for _, tbl := range tables {
		if _, err := db.Pool.Exec(context.Background(), "TRUNCATE "+tbl+" CASCADE"); err != nil {
			t.Logf("warning: truncate %s: %v", tbl, err)
		}
	}
}
