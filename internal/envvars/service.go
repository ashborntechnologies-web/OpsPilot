package envvars

import (
	"context"
	"net/http"

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

// HandleList returns all env vars for an environment. Secret values are replaced with "***".
func (s *Service) HandleList(c *gin.Context) {
	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
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

// HandleUpsert creates or updates a single env var (identified by key).
func (s *Service) HandleUpsert(c *gin.Context) {
	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
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

	var v models.EnvVar
	err = s.db.Pool.QueryRow(c.Request.Context(),
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
	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
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
