package deploy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/deploy"
	"github.com/ashborntechnologies-web/OpsPilot/internal/envvars"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	"github.com/ashborntechnologies-web/OpsPilot/internal/testutil"
	"github.com/ashborntechnologies-web/OpsPilot/internal/webhooks"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockEnqueuer is a no-op Enqueuer for handler tests.
type mockEnqueuer struct{}

func (m *mockEnqueuer) EnqueueDeploy(_, _, _, _ string) error                    { return nil }
func (m *mockEnqueuer) EnqueueProvision(_, _ string) error                       { return nil }
func (m *mockEnqueuer) EnqueueRollback(_, _, _, _ string) error                  { return nil }
func (m *mockEnqueuer) EnqueueDeleteProject(p deploy.DeleteProjectPayload) error { return nil }
func (m *mockEnqueuer) SetPendingMutation(_ context.Context, _ string, _ models.MutationProposal, _ time.Duration) error {
	return nil
}
func (m *mockEnqueuer) GetPendingMutation(_ context.Context, _ string) (*models.MutationProposal, error) {
	return nil, nil
}
func (m *mockEnqueuer) DeletePendingMutation(_ context.Context, _ string) error { return nil }

// newTestRouter builds a minimal gin router for a specific handler, injecting a real
// user ID into the context to simulate an authenticated request.
func newTestRouter(t *testing.T, db *models.DB, awsMock *testutil.MockAWSProvider, userID uuid.UUID) (*gin.Engine, *deploy.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	eventSvc := events.NewService(db)
	envVarSvc := envvars.NewService(db, "test-encryption-key", "")
	webhookSvc := webhooks.NewService(db)
	svc := deploy.NewService(db, awsMock, &testutil.MockGitHubProvider{Token: "ghtoken"},
		nil, &mockEnqueuer{}, eventSvc, envVarSvc, webhookSvc)

	// Inject the authenticated user ID into every request context.
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDContextKey, userID)
		c.Next()
	})

	// Project-ownership guard (real middleware — exercises the actual DB check).
	proj := r.Group("/projects/:id")
	proj.Use(middleware.LoadProjectMembership(db))
	{
		proj.GET("", svc.HandleGetProject)
		proj.GET("/deployments", svc.HandleListDeployments)
		proj.POST("/environments/:envId/deploy", svc.HandleDeploy)
		proj.GET("/environments/:envId/health", svc.HandleCheckHealth)
		proj.POST("/environments/:envId/scale", svc.HandleScaleService)
		proj.GET("/environments/:envId/logs", svc.HandleGetLogs)
		proj.GET("/costs", svc.HandleGetCosts)
	}

	r.GET("/projects", svc.HandleListProjects)

	return r, svc
}

func doRequest(r *gin.Engine, method, path string, body interface{}) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req, _ = http.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, _ = http.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// HandleListProjects
// ---------------------------------------------------------------------------

func TestHandleListProjects_Empty(t *testing.T) {
	db := testutil.NewTestDB(t)
	userID := testutil.CreateUser(t, db)
	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, userID)

	w := doRequest(r, http.MethodGet, "/projects", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp []interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

func TestHandleListProjects_ReturnsMine(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	otherID := testutil.CreateUser(t, db)
	testutil.CreateProject(t, db, ownerID)
	testutil.CreateProject(t, db, ownerID)
	testutil.CreateProject(t, db, otherID) // other user — should NOT appear

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodGet, "/projects", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp []interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp, 2, "only the owner's projects should be returned")
}

// ---------------------------------------------------------------------------
// HandleGetProject
// ---------------------------------------------------------------------------

func TestHandleGetProject_NotOwned(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	otherID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, otherID)

	w := doRequest(r, http.MethodGet, "/projects/"+projectID.String(), nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleGetProject_OK(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodGet, "/projects/"+projectID.String(), nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, projectID.String(), resp["id"])
}

func TestHandleGetProject_InvalidUUID(t *testing.T) {
	db := testutil.NewTestDB(t)
	userID := testutil.CreateUser(t, db)
	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, userID)

	w := doRequest(r, http.MethodGet, "/projects/not-a-uuid", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ---------------------------------------------------------------------------
// HandleListDeployments
// ---------------------------------------------------------------------------

func TestHandleListDeployments_Empty(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)
	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodGet, "/projects/"+projectID.String()+"/deployments", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp []interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

func TestHandleListDeployments_ReturnsList(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	accountID := testutil.CreateAWSAccount(t, db, ownerID)
	projectID := testutil.CreateProject(t, db, ownerID)
	envID := testutil.CreateEnvironment(t, db, projectID, accountID)
	testutil.CreateDeployment(t, db, projectID, envID, "live")
	testutil.CreateDeployment(t, db, projectID, envID, "failed")

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodGet, "/projects/"+projectID.String()+"/deployments", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp []interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
}

// ---------------------------------------------------------------------------
// HandleDeploy
// ---------------------------------------------------------------------------

func TestHandleDeploy_EnvNotReady(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	accountID := testutil.CreateAWSAccount(t, db, ownerID)
	projectID := testutil.CreateProject(t, db, ownerID)
	envID := testutil.CreateEnvironment(t, db, projectID, accountID) // status = "pending"

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodPost,
		fmt.Sprintf("/projects/%s/environments/%s/deploy", projectID, envID), nil)
	assert.Equal(t, http.StatusAccepted, w.Code)

	// The deploy is accepted and queued. The actual "env not ready" error will
	// surface asynchronously via the worker. The handler itself returns 202.
}

// ---------------------------------------------------------------------------
// HandleCheckHealth
// ---------------------------------------------------------------------------

func TestHandleCheckHealth_NotDeployed(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	accountID := testutil.CreateAWSAccount(t, db, ownerID)
	projectID := testutil.CreateProject(t, db, ownerID)
	envID := testutil.CreateEnvironment(t, db, projectID, accountID)

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodGet,
		fmt.Sprintf("/projects/%s/environments/%s/health", projectID, envID), nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "not_deployed", resp["status"])
}

func TestHandleCheckHealth_DeployedReturnsURL(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	accountID := testutil.CreateAWSAccount(t, db, ownerID)
	projectID := testutil.CreateProject(t, db, ownerID)
	envID := testutil.CreateReadyEnvironment(t, db, projectID, accountID)

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodGet,
		fmt.Sprintf("/projects/%s/environments/%s/health", projectID, envID), nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["url"], "ready env should include the ALB URL")
	assert.Equal(t, "up", resp["status"])
}

// ---------------------------------------------------------------------------
// HandleScaleService
// ---------------------------------------------------------------------------

func TestHandleScaleService_InvalidBody(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	accountID := testutil.CreateAWSAccount(t, db, ownerID)
	projectID := testutil.CreateProject(t, db, ownerID)
	envID := testutil.CreateReadyEnvironment(t, db, projectID, accountID)

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	// Missing "replicas" field
	w := doRequest(r, http.MethodPost,
		fmt.Sprintf("/projects/%s/environments/%s/scale", projectID, envID),
		map[string]interface{}{})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleScaleService_OK(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	accountID := testutil.CreateAWSAccount(t, db, ownerID)
	projectID := testutil.CreateProject(t, db, ownerID)
	envID := testutil.CreateReadyEnvironment(t, db, projectID, accountID)

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodPost,
		fmt.Sprintf("/projects/%s/environments/%s/scale", projectID, envID),
		map[string]interface{}{"replicas": 2})
	assert.Equal(t, http.StatusOK, w.Code)
}

// ---------------------------------------------------------------------------
// HandleGetCosts
// ---------------------------------------------------------------------------

func TestHandleGetCosts_OK(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, ownerID)

	w := doRequest(r, http.MethodGet, "/projects/"+projectID.String()+"/costs", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotZero(t, resp["total_monthly_cost"])
	assert.Equal(t, "USD", resp["currency"])
}

func TestHandleGetCosts_NotOwned(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	otherID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)

	r, _ := newTestRouter(t, db, &testutil.MockAWSProvider{}, otherID)

	w := doRequest(r, http.MethodGet, "/projects/"+projectID.String()+"/costs", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}
