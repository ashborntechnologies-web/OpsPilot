package github

import (
	"context"

	"github.com/google/uuid"
)

// GitHubProvider is the interface consumed by the deploy service.
// *Service satisfies this interface; tests inject a mock implementation.
type GitHubProvider interface {
	GetTokenForDeployment(ctx context.Context, userID uuid.UUID) (string, error)
	GetLatestCommit(ctx context.Context, token, owner, repo, branch string) (sha string, message string, err error)
	RegisterRepoWebhook(ctx context.Context, token, owner, repo, webhookURL, secret string) (int64, error)
	DeleteRepoWebhook(ctx context.Context, token, owner, repo string, webhookID int64) error
	CreatePRComment(ctx context.Context, token, owner, repo string, prNumber int, body string) (int64, error)
	UpdatePRComment(ctx context.Context, token, owner, repo string, commentID int64, body string) error
}
