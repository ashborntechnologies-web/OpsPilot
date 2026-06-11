package testutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	awsiface "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// MockAWSProvider
// ---------------------------------------------------------------------------

// MockAWSProvider is a no-op implementation of aws.AWSProvider.
// Override individual fields to inject controlled return values.
type MockAWSProvider struct {
	AssumeRoleErr      error
	CostSummary        *models.CostSummary
	CostSummaryErr     error
	HealthResult       *awsiface.ServiceHealth
	HealthErr          error
	LogLines           []string
	LogsErr            error
	TaskARN            string
	TaskARNErr         error
	StreamURL          string
	TokenValue         string
	ExecErr            error
	ScaleErr           error
	BuildID            string
	BuildErr           error
	WaitBuildErr       error
	TaskDefARN         string
	TaskDefErr         error
	TGArn              string
	TGErr              error
	LRArn              string
	LRErr              error
	ECSErr             error
	StableErr          error
	PlatformStack      *models.PlatformStack
	PlatformStackIsNew bool
	PlatformStackErr   error
}

func (m *MockAWSProvider) AssumeRoleForEnvironment(_ context.Context, _ *models.Environment) (*awsiface.ClientBundle, error) {
	return &awsiface.ClientBundle{}, m.AssumeRoleErr
}

func (m *MockAWSProvider) AssumeRoleForAccount(_ context.Context, _, _, _ string) (*awsiface.ClientBundle, error) {
	return &awsiface.ClientBundle{}, m.AssumeRoleErr
}

func (m *MockAWSProvider) StartCodeBuildJob(_ context.Context, _ *awsiface.ClientBundle, _, _, _, _, _, _, _, _, _, _ string) (*awsiface.StartCodeBuildResult, error) {
	return &awsiface.StartCodeBuildResult{BuildID: m.BuildID}, m.BuildErr
}

func (m *MockAWSProvider) WaitForCodeBuild(_ context.Context, _ *awsiface.ClientBundle, _ string, _ func(string)) error {
	return m.WaitBuildErr
}

func (m *MockAWSProvider) DeleteSSMParameter(_ context.Context, _ *awsiface.ClientBundle, _ string) {}

func (m *MockAWSProvider) RegisterECSTaskDefinition(_ context.Context, _ *awsiface.ClientBundle, _ *models.Environment, _ *models.Project, _ string, _ []ecstypes.KeyValuePair) (string, error) {
	return m.TaskDefARN, m.TaskDefErr
}

func (m *MockAWSProvider) EnsureTargetGroup(_ context.Context, _ *awsiface.ClientBundle, _ *models.PlatformStack, _ *models.Environment, _ *models.Project) (string, error) {
	return m.TGArn, m.TGErr
}

func (m *MockAWSProvider) EnsureListenerRule(_ context.Context, _ *awsiface.ClientBundle, _ *models.PlatformStack, _ *models.Environment, _ string) (string, error) {
	return m.LRArn, m.LRErr
}

func (m *MockAWSProvider) EnsureECSService(_ context.Context, _ *awsiface.ClientBundle, _ *models.Environment, _ *models.Project, _, _ string, _ []string, _ string) error {
	return m.ECSErr
}

func (m *MockAWSProvider) WaitForECSServiceStable(_ context.Context, _ *awsiface.ClientBundle, _ string, _ *models.Environment, _ func(string)) error {
	return m.StableErr
}

func (m *MockAWSProvider) DescribeECSService(_ context.Context, _ *awsiface.ClientBundle, _, _ string) (*awsiface.ServiceHealth, error) {
	if m.HealthResult != nil {
		return m.HealthResult, m.HealthErr
	}
	return &awsiface.ServiceHealth{RunningCount: 1, DesiredCount: 1}, m.HealthErr
}

func (m *MockAWSProvider) UpdateServiceDesiredCount(_ context.Context, _ *awsiface.ClientBundle, _, _ string, _ int32) error {
	return m.ScaleErr
}

func (m *MockAWSProvider) DeleteECSService(_ context.Context, _ *awsiface.ClientBundle, _, _ string) error {
	return nil
}

func (m *MockAWSProvider) GetRunningTask(_ context.Context, _ *awsiface.ClientBundle, _, _ string) (string, error) {
	return m.TaskARN, m.TaskARNErr
}

func (m *MockAWSProvider) StartExecSession(_ context.Context, _ *awsiface.ClientBundle, _, _, _ string) (string, string, error) {
	return m.StreamURL, m.TokenValue, m.ExecErr
}

func (m *MockAWSProvider) DeleteListenerRule(_ context.Context, _ *awsiface.ClientBundle, _ string) error {
	return nil
}

func (m *MockAWSProvider) DeleteTargetGroup(_ context.Context, _ *awsiface.ClientBundle, _ string) error {
	return nil
}

func (m *MockAWSProvider) PurgeECRRepository(_ context.Context, _ *awsiface.ClientBundle, _ string) error {
	return nil
}

func (m *MockAWSProvider) FetchRecentECSLogs(_ context.Context, _ *awsiface.ClientBundle, _ string, _ int32) ([]string, error) {
	return m.LogLines, m.LogsErr
}

func (m *MockAWSProvider) CreateLogGroupIfNotExists(_ context.Context, _ *awsiface.ClientBundle, _ string) {
}

func (m *MockAWSProvider) GetOrCreatePlatformStack(_ context.Context, _ uuid.UUID, _ string) (*models.PlatformStack, bool, error) {
	if m.PlatformStack != nil {
		return m.PlatformStack, m.PlatformStackIsNew, m.PlatformStackErr
	}
	ps := &models.PlatformStack{StackStatus: "ready"}
	return ps, false, m.PlatformStackErr
}

func (m *MockAWSProvider) GetPlatformStack(_ context.Context, _ uuid.UUID) (*models.PlatformStack, error) {
	if m.PlatformStack != nil {
		return m.PlatformStack, m.PlatformStackErr
	}
	return &models.PlatformStack{StackStatus: "ready"}, m.PlatformStackErr
}

func (m *MockAWSProvider) DeployPlatformStack(_ context.Context, _ *awsiface.ClientBundle, _, _ string) (string, error) {
	return "stack-id", nil
}

func (m *MockAWSProvider) WaitForPlatformStackAndPopulate(_ context.Context, _ *awsiface.ClientBundle, _ *models.PlatformStack, _ string, _ func(string)) error {
	return nil
}

func (m *MockAWSProvider) DeployProjectStack(_ context.Context, _ *awsiface.ClientBundle, _ *models.Environment, _ *models.Project) (string, error) {
	return "proj-stack-id", nil
}

func (m *MockAWSProvider) WaitForProjectStackAndPopulate(_ context.Context, _ *awsiface.ClientBundle, _ *models.Environment, _ *models.Project, _ string, _ *models.PlatformStack, _ func(string)) error {
	return nil
}

func (m *MockAWSProvider) DeleteProjectStack(_ context.Context, _ *awsiface.ClientBundle, _ string) error {
	return nil
}

func (m *MockAWSProvider) GetAccountCostSummary(_ context.Context, _ string) (*models.CostSummary, error) {
	if m.CostSummary != nil {
		return m.CostSummary, m.CostSummaryErr
	}
	return &models.CostSummary{
		TotalMonthlyCost: 42.50,
		ByService:        map[string]float64{"Amazon ECS": 42.50},
		Currency:         "USD",
		PeriodStart:      "2026-05-01",
		PeriodEnd:        "2026-05-31",
	}, m.CostSummaryErr
}

func (m *MockAWSProvider) GetCurrentTaskResources(_ context.Context, _ *awsiface.ClientBundle, _, _ string) (string, string, error) {
	return "512", "1024", nil
}

func (m *MockAWSProvider) UpdateServiceResources(_ context.Context, _ *awsiface.ClientBundle, _ *models.Environment, _ *models.Project, _, _, _ string) error {
	return nil
}

func (m *MockAWSProvider) CreatePreviewService(_ context.Context, _ *awsiface.ClientBundle, _ *models.Environment, _ *models.PlatformStack, _ *models.Environment, _ string, _ *models.Project) (string, string, error) {
	return "rule-arn", "tg-arn", nil
}

func (m *MockAWSProvider) TeardownPreviewService(_ context.Context, _ *awsiface.ClientBundle, _ *models.Environment) error {
	return nil
}

// ---------------------------------------------------------------------------
// MockGitHubProvider
// ---------------------------------------------------------------------------

// MockGitHubProvider is a no-op implementation of github.GitHubProvider.
type MockGitHubProvider struct {
	Token        string
	TokenErr     error
	CommitSHA    string
	CommitMsg    string
	CommitErr    error
	WebhookID    int64
	WebhookErr   error
	PRCommentID  int64
	PRCommentErr error
}

func (m *MockGitHubProvider) GetTokenForDeployment(_ context.Context, _ uuid.UUID) (string, error) {
	return m.Token, m.TokenErr
}

func (m *MockGitHubProvider) GetLatestCommit(_ context.Context, _, _, _, _ string) (string, string, error) {
	sha := m.CommitSHA
	if sha == "" {
		sha = "deadbeef12345678"
	}
	return sha, m.CommitMsg, m.CommitErr
}

func (m *MockGitHubProvider) RegisterRepoWebhook(_ context.Context, _, _, _, _, _ string) (int64, error) {
	return m.WebhookID, m.WebhookErr
}

func (m *MockGitHubProvider) DeleteRepoWebhook(_ context.Context, _, _, _ string, _ int64) error {
	return m.WebhookErr
}

func (m *MockGitHubProvider) CreatePRComment(_ context.Context, _, _, _ string, _ int, _ string) (int64, error) {
	return m.PRCommentID, m.PRCommentErr
}

func (m *MockGitHubProvider) UpdatePRComment(_ context.Context, _, _, _ string, _ int64, _ string) error {
	return m.PRCommentErr
}

// ---------------------------------------------------------------------------
// WebhookSink — captures outbound webhook deliveries in tests
// ---------------------------------------------------------------------------

// WebhookSink starts an httptest server that records POSTed payloads.
func WebhookSink(t *testing.T) (url string, payloads func() [][]byte) {
	t.Helper()
	var received [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		received = append(received, buf)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() [][]byte { return received }
}

// StreamCodeBuildLogs satisfies aws.AWSProvider for tests — returns the cursor unchanged.
func (m *MockAWSProvider) StreamCodeBuildLogs(_ context.Context, _ *awsiface.ClientBundle, _ string, since time.Time, _ func(string)) (time.Time, error) {
	return since, nil
}

// StopCodeBuildJob satisfies aws.AWSProvider for tests.
func (m *MockAWSProvider) StopCodeBuildJob(_ context.Context, _ *awsiface.ClientBundle, _ string) error {
	return nil
}
