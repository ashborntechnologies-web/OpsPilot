package envvars

import (
	"context"
	"net/http"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Service struct {
	db *models.DB
}

func NewService(db *models.DB) *Service {
	return &Service{db: db}
}

// resolveEnv parses :id and :envId and verifies the environment belongs to the project.
// RequireProjectOwnership guards the project; this guards the cross-reference so a caller
// cannot operate on another tenant's environment by passing a foreign envId.
func (s *Service) resolveEnv(c *gin.Context) (envID uuid.UUID, ok bool) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return uuid.UUID{}, false
	}
	envID, err = uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
		return uuid.UUID{}, false
	}
	var exists bool
	err = s.db.Pool.QueryRow(c.Request.Context(),
		`SELECT EXISTS (SELECT 1 FROM environments WHERE id = $1 AND project_id = $2)`,
		envID, projectID,
	).Scan(&exists)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify environment"})
		return uuid.UUID{}, false
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "environment not found"})
		return uuid.UUID{}, false
	}
	return envID, true
}

// HandleList returns all env vars for an environment. Secret values are replaced with "***".
func (s *Service) HandleList(c *gin.Context) {
	envID, ok := s.resolveEnv(c)
	if !ok {
		return
	}

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT id, environment_id, key, value, is_secret, created_at, updated_at
		 FROM env_vars WHERE environment_id = $1 ORDER BY key ASC`, envID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch env vars"})
		return
	}
	defer rows.Close()

	var vars []models.EnvVar
	for rows.Next() {
		var v models.EnvVar
		if err := rows.Scan(&v.ID, &v.EnvironmentID, &v.Key, &v.Value, &v.IsSecret, &v.CreatedAt, &v.UpdatedAt); err != nil {
			continue
		}
		if v.IsSecret {
			v.Value = "***"
		}
		vars = append(vars, v)
	}
	if vars == nil {
		vars = []models.EnvVar{}
	}
	c.JSON(http.StatusOK, vars)
}

// envVarKeyRe validates POSIX-style env var names; invalid names would be
// rejected by ECS at deploy time, long after the user typed them.
var envVarKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)

// HandleUpsert creates or updates a single env var (identified by key).
func (s *Service) HandleUpsert(c *gin.Context) {
	envID, ok := s.resolveEnv(c)
	if !ok {
		return
	}

	var req struct {
		Key      string `json:"key" binding:"required"`
		Value    string `json:"value" binding:"required"`
		IsSecret bool   `json:"is_secret"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !envVarKeyRe.MatchString(req.Key) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid key: must start with a letter or underscore and contain only letters, numbers, underscores"})
		return
	}
	if len(req.Value) > 32768 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "value too large (max 32KB)"})
		return
	}

	var v models.EnvVar
	err := s.db.Pool.QueryRow(c.Request.Context(),
		`INSERT INTO env_vars (environment_id, key, value, is_secret)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (environment_id, key)
		 DO UPDATE SET value = EXCLUDED.value, is_secret = EXCLUDED.is_secret, updated_at = NOW()
		 RETURNING id, environment_id, key, value, is_secret, created_at, updated_at`,
		envID, req.Key, req.Value, req.IsSecret,
	).Scan(&v.ID, &v.EnvironmentID, &v.Key, &v.Value, &v.IsSecret, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save env var"})
		return
	}

	if v.IsSecret {
		v.Value = "***"
	}
	c.JSON(http.StatusOK, v)
}

// HandleDelete removes an env var by ID.
func (s *Service) HandleDelete(c *gin.Context) {
	varID, err := uuid.Parse(c.Param("varId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid var id"})
		return
	}
	envID, ok := s.resolveEnv(c)
	if !ok {
		return
	}

	result, err := s.db.Pool.Exec(c.Request.Context(),
		`DELETE FROM env_vars WHERE id = $1 AND environment_id = $2`, varID, envID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete env var"})
		return
	}
	if result.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "env var not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// LoadForEnvironment fetches the full (unredacted) env vars for an environment so they can
// be injected into the ECS task definition at deploy time.
func (s *Service) LoadForEnvironment(ctx context.Context, environmentID uuid.UUID) ([]ecstypes.KeyValuePair, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT key, value FROM env_vars WHERE environment_id = $1 ORDER BY key ASC`, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pairs []ecstypes.KeyValuePair
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		pairs = append(pairs, ecstypes.KeyValuePair{
			Name:  aws.String(k),
			Value: aws.String(v),
		})
	}
	return pairs, nil
}
