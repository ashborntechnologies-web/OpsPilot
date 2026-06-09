package models_test

import (
	"testing"

	"github.com/ashborntechnologies-web/OpsPilot/internal/testutil"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunMigrations_Idempotent verifies that running all migrations twice
// on a fresh schema does not error (every migration uses IF NOT EXISTS / IF EXISTS).
func TestRunMigrations_Idempotent(t *testing.T) {
	db := testutil.NewTestDB(t) // skips if TEST_DATABASE_URL not set
	err := models.RunMigrations(db)
	assert.NoError(t, err, "second RunMigrations call must be a no-op")
}

// TestUserOwnsProject verifies the tenant-isolation ownership check.
func TestUserOwnsProject(t *testing.T) {
	db := testutil.NewTestDB(t)

	ownerID := testutil.CreateUser(t, db)
	otherID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)

	owned, err := db.UserOwnsProject(t.Context(), ownerID, projectID)
	require.NoError(t, err)
	assert.True(t, owned, "owner should own their own project")

	notOwned, err := db.UserOwnsProject(t.Context(), otherID, projectID)
	require.NoError(t, err)
	assert.False(t, notOwned, "other user should not own the project")
}

// TestUserOwnsAccount verifies the account-level ownership check.
func TestUserOwnsAccount(t *testing.T) {
	db := testutil.NewTestDB(t)

	ownerID := testutil.CreateUser(t, db)
	otherID := testutil.CreateUser(t, db)
	accountID := testutil.CreateAWSAccount(t, db, ownerID)

	owned, err := db.UserOwnsAccount(t.Context(), ownerID, accountID)
	require.NoError(t, err)
	assert.True(t, owned)

	notOwned, err := db.UserOwnsAccount(t.Context(), otherID, accountID)
	require.NoError(t, err)
	assert.False(t, notOwned)
}
