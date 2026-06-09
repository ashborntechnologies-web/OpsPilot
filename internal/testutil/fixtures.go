package testutil

import (
	"context"
	"testing"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

// CreateUser inserts a test user and returns its ID.
func CreateUser(t *testing.T, db *models.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO users (clerk_id, email) VALUES ($1, $2) RETURNING id`,
		"clerk_test_"+uuid.NewString(), "test-"+uuid.NewString()+"@example.com",
	).Scan(&id)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id
}

// CreateProject inserts a minimal test project and returns its ID.
func CreateProject(t *testing.T, db *models.DB, userID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO projects (user_id, name, repo_url, repo_owner, repo_name, framework)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		userID, "test-project-"+uuid.NewString()[:8],
		"https://github.com/test/repo", "test", "repo", "go",
	).Scan(&id)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return id
}

// CreateAWSAccount inserts a test AWS account and returns its ID.
func CreateAWSAccount(t *testing.T, db *models.DB, userID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO aws_accounts (user_id, label, aws_account_id, iam_role_arn)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, "test-account", "123456789012",
		"arn:aws:iam::123456789012:role/ConvDeployPlatformRole",
	).Scan(&id)
	if err != nil {
		t.Fatalf("CreateAWSAccount: %v", err)
	}
	return id
}

// CreateEnvironment inserts a test environment (staging, pending) and returns its ID.
func CreateEnvironment(t *testing.T, db *models.DB, projectID, accountID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO environments (project_id, name, aws_region, account_id, stack_status)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		projectID, "staging", "us-east-1", accountID, "pending",
	).Scan(&id)
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	return id
}

// CreateReadyEnvironment inserts a test environment marked ready with all infra fields set.
func CreateReadyEnvironment(t *testing.T, db *models.DB, projectID, accountID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	ecr := "123456789012.dkr.ecr.us-east-1.amazonaws.com/test"
	cluster := "convdeploy-cluster"
	svcName := "svc-" + uuid.NewString()[:8]
	cbProject := "cb-" + uuid.NewString()[:8]
	roleARN := "arn:aws:iam::123456789012:role/TaskExecRole"
	logGroup := "/ecs/test"
	albDNS := "test-alb-123.us-east-1.elb.amazonaws.com"
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO environments
		 (project_id, name, aws_region, account_id, stack_status,
		  ecr_repo_uri, ecs_cluster_name, ecs_service_name, codebuild_project_name,
		  task_execution_role_arn, log_group_name, alb_dns)
		 VALUES ($1,$2,$3,$4,'ready',$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		projectID, "staging", "us-east-1", accountID,
		ecr, cluster, svcName, cbProject, roleARN, logGroup, albDNS,
	).Scan(&id)
	if err != nil {
		t.Fatalf("CreateReadyEnvironment: %v", err)
	}
	return id
}

// CreateDeployment inserts a test deployment and returns its ID.
func CreateDeployment(t *testing.T, db *models.DB, projectID, envID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO deployments (project_id, environment_id, commit_sha, status)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		projectID, envID, "abc12345", status,
	).Scan(&id)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	return id
}
