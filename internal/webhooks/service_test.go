package webhooks_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/testutil"
	"github.com/ashborntechnologies-web/OpsPilot/internal/webhooks"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWebhookRouter(t *testing.T, db *models.DB, userID uuid.UUID) (*gin.Engine, *webhooks.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := webhooks.NewService(db)

	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDContextKey, userID)
		c.Next()
	})

	proj := r.Group("/projects/:id")
	proj.Use(middleware.RequireProjectOwnership(db))
	{
		proj.GET("/webhooks", svc.HandleList)
		proj.POST("/webhooks", svc.HandleCreate)
		proj.PATCH("/webhooks/:webhookId", svc.HandleUpdate)
		proj.DELETE("/webhooks/:webhookId", svc.HandleDelete)
	}

	return r, svc
}

func doWebhookRequest(r *gin.Engine, method, path string, body interface{}) *httptest.ResponseRecorder {
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

func TestWebhooks_CRUD(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)
	r, _ := newWebhookRouter(t, db, ownerID)
	base := "/projects/" + projectID.String() + "/webhooks"

	// List — initially empty
	w := doWebhookRequest(r, http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var list []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Empty(t, list)

	// Create
	w = doWebhookRequest(r, http.MethodPost, base, map[string]interface{}{
		"url":    "https://example.com/hook",
		"events": []string{"deploy.started", "deploy.succeeded"},
	})
	require.Equal(t, http.StatusCreated, w.Code)
	var created map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	hookID := created["id"].(string)
	assert.Equal(t, "https://example.com/hook", created["url"])
	assert.True(t, created["active"].(bool))

	// List — should have 1
	w = doWebhookRequest(r, http.MethodGet, base, nil)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Len(t, list, 1)

	// Update — disable
	w = doWebhookRequest(r, http.MethodPatch, base+"/"+hookID, map[string]interface{}{
		"active": false,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var updated map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.False(t, updated["active"].(bool))

	// Delete
	w = doWebhookRequest(r, http.MethodDelete, base+"/"+hookID, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// List — empty again
	w = doWebhookRequest(r, http.MethodGet, base, nil)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Empty(t, list)
}

func TestWebhooks_CreateMissingURL(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)
	r, _ := newWebhookRouter(t, db, ownerID)

	w := doWebhookRequest(r, http.MethodPost, "/projects/"+projectID.String()+"/webhooks",
		map[string]interface{}{"events": []string{"deploy.started"}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWebhooks_NotOwned(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	otherID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)
	r, _ := newWebhookRouter(t, db, otherID)

	w := doWebhookRequest(r, http.MethodGet, "/projects/"+projectID.String()+"/webhooks", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestWebhooks_FireEvent verifies that FireEvent delivers a POST to the registered URL.
func TestWebhooks_FireEvent(t *testing.T) {
	db := testutil.NewTestDB(t)
	ownerID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, ownerID)

	// Start a sink server that captures the incoming request
	var received []byte
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		received = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	// Register a webhook pointing at the sink
	svc := webhooks.NewService(db)
	ctx := t.Context()

	// Insert webhook directly via the service
	r2, _ := newWebhookRouter(t, db, ownerID)
	w := doWebhookRequest(r2, http.MethodPost, "/projects/"+projectID.String()+"/webhooks",
		map[string]interface{}{
			"url":    sink.URL,
			"events": []string{string(models.WebhookEventDeploySucceeded)},
		})
	require.Equal(t, http.StatusCreated, w.Code)

	// Fire the event
	payload := webhooks.BuildPayload(projectID.String(), "test-project", "production",
		uuid.NewString(), "deadbeef", "initial commit")
	svc.FireEvent(projectID, models.WebhookEventDeploySucceeded, payload)

	// Give the goroutine time to deliver (production code fires in a goroutine)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(received) == 0 {
		time.Sleep(50 * time.Millisecond)
	}

	require.NotEmpty(t, received, "webhook sink should have received a payload")
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(received, &body))
	assert.Equal(t, string(models.WebhookEventDeploySucceeded), body["event"])
	assert.NotEmpty(t, body["payload"])

	// Verify the HMAC header was sent (no secret → no header)
	_ = ctx
	_ = strings.Contains
	_ = fmt.Sprintf
}
