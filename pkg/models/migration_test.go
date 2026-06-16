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

// TestProjectOrgRole verifies the org-based tenant-isolation guard: a member of
// the project's org resolves their role; a non-member gets ErrNoMembership.
func TestProjectOrgRole(t *testing.T) {
	db := testutil.NewTestDB(t)

	ownerID := testutil.CreateUser(t, db)
	otherID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)

	orgID, role, err := db.ProjectOrgRole(t.Context(), ownerID, projectID)
	require.NoError(t, err)
	assert.Equal(t, models.RoleAdmin, role, "creator is admin of their personal org")
	assert.Equal(t, testutil.UserOrg(t, db, ownerID), orgID)

	_, _, err = db.ProjectOrgRole(t.Context(), otherID, projectID)
	assert.ErrorIs(t, err, models.ErrNoMembership, "non-member must not access the project")
}

// TestUserOrgRole verifies role resolution and the membership-hierarchy roles.
func TestUserOrgRole(t *testing.T) {
	db := testutil.NewTestDB(t)

	adminID := testutil.CreateUser(t, db)
	orgID := testutil.UserOrg(t, db, adminID)

	role, err := db.UserOrgRole(t.Context(), adminID, orgID)
	require.NoError(t, err)
	assert.Equal(t, models.RoleAdmin, role)

	// A second user added as a viewer resolves to viewer; a non-member errors.
	viewerID := testutil.CreateUser(t, db)
	testutil.AddOrgMember(t, db, orgID, viewerID, models.RoleViewer)
	role, err = db.UserOrgRole(t.Context(), viewerID, orgID)
	require.NoError(t, err)
	assert.Equal(t, models.RoleViewer, role)
	assert.Less(t, models.RoleRank(role), models.RoleRank(models.RoleEngineer))

	strangerID := testutil.CreateUser(t, db)
	_, err = db.UserOrgRole(t.Context(), strangerID, orgID)
	assert.ErrorIs(t, err, models.ErrNoMembership)
}
