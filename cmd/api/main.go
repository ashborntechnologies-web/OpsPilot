package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/ashborntechnologies-web/OpsPilot/internal/auth"
	"github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/conversation"
	"github.com/ashborntechnologies-web/OpsPilot/internal/deploy"
	"github.com/ashborntechnologies-web/OpsPilot/internal/diagnosis"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	githubsvc "github.com/ashborntechnologies-web/OpsPilot/internal/github"
	"github.com/ashborntechnologies-web/OpsPilot/internal/queue"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	pkgws "github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

// resolvePlatformIdentity returns the AWS account ID and full caller ARN of the
// entity running ConvDeploy. Used to embed the trust-policy principal in the
// bootstrap CloudFormation template and to grant sts:AssumeRole when same-account.
//
// PLATFORM_AWS_ACCOUNT_ID env var overrides the account ID (caller ARN is still
// auto-detected). If AWS credentials are not configured both values are empty and
// the bootstrap template endpoint returns 503.
func resolvePlatformIdentity() (accountID, callerARN string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := awssdk.LoadDefaultConfig(ctx)
	if err != nil {
		log.Printf("WARNING: could not load AWS config: %v — bootstrap template unavailable", err)
		return os.Getenv("PLATFORM_AWS_ACCOUNT_ID"), ""
	}

	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		log.Printf("WARNING: sts:GetCallerIdentity failed: %v — bootstrap template unavailable", err)
		return os.Getenv("PLATFORM_AWS_ACCOUNT_ID"), ""
	}

	callerARN = *out.Arn
	accountID = *out.Account

	// Allow explicit override of the account ID (e.g. for testing).
	if override := os.Getenv("PLATFORM_AWS_ACCOUNT_ID"); override != "" {
		accountID = override
	}

	log.Printf("Platform identity: account=%s arn=%s", accountID, callerARN)
	return accountID, callerARN
}

func main() {
	// Load env
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system env")
	}

	// Connect DB
	db, err := models.NewDB(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Run migrations
	if err := models.RunMigrations(db); err != nil {
		log.Fatalf("Failed to run migrations: %v", err)
	}

	// Init WebSocket hub
	hub := pkgws.NewHub(os.Getenv("FRONTEND_URL"))
	go hub.Run()

	// Init services
	authSvc := auth.NewService(os.Getenv("CLERK_PUBLISHABLE_KEY"), os.Getenv("CLERK_SECRET_KEY"))

	// wsAuthFn is injected into the WebSocket hub so it can validate the first-message token
	// without taking a URL query parameter (which leaks into server logs and browser history).
	// It also enforces project ownership so a client cannot subscribe to another tenant's stream.
	wsAuthFn := pkgws.AuthFunc(func(ctx context.Context, token, projectID string) (uuid.UUID, error) {
		userID, err := middleware.ResolveToken(ctx, db, authSvc, token)
		if err != nil {
			return uuid.UUID{}, err
		}
		pid, err := uuid.Parse(projectID)
		if err != nil {
			return uuid.UUID{}, err
		}
		owned, err := db.UserOwnsProject(ctx, userID, pid)
		if err != nil {
			return uuid.UUID{}, err
		}
		if !owned {
			return uuid.UUID{}, errors.New("forbidden: user does not own project")
		}
		return userID, nil
	})
	githubSvc := githubsvc.NewService(
		os.Getenv("GITHUB_CLIENT_ID"),
		os.Getenv("GITHUB_CLIENT_SECRET"),
		os.Getenv("GITHUB_REDIRECT_URL"),
		os.Getenv("ENCRYPTION_KEY"),
		db,
	)
	platformAccountID, platformCallerARN := resolvePlatformIdentity()
	awsSvc := aws.NewService(db, platformAccountID, platformCallerARN)

	// Queue client is used by the deploy service to enqueue jobs.
	// Queue server is separate to avoid a circular import (server imports deploy service).
	queueClient := queue.NewClient(os.Getenv("REDIS_URL"))
	defer queueClient.Close()

	eventSvc := events.NewService(db)
	awsSvc.SetEvents(eventSvc) // enable account-level audit events (e.g. external_id.generated)
	deploySvc := deploy.NewService(db, awsSvc, githubSvc, hub, queueClient, eventSvc)

	// After an environment is created with a linked AWS account, auto-trigger provisioning.
	awsSvc.SetOnEnvCreated(func(projectID, environmentID uuid.UUID) {
		if err := queueClient.EnqueueProvision(projectID.String(), environmentID.String()); err != nil {
			log.Printf("failed to enqueue provision job for env %s: %v", environmentID, err)
		}
	})
	diagnosisSvc := diagnosis.NewService(db, awsSvc, eventSvc, os.Getenv("ANTHROPIC_API_KEY"))
	conversationSvc := conversation.NewService(db, deploySvc, diagnosisSvc, os.Getenv("ANTHROPIC_API_KEY"), hub)

	// Init job queue server
	queueServer := queue.NewServer(
		os.Getenv("REDIS_URL"),
		deploySvc,
		diagnosisSvc,
	)
	go queueServer.Start()
	defer queueServer.Stop()

	// Periodic watchdog — enqueues a stuck-resource reconcile every 5 minutes.
	scheduler := queue.NewScheduler(os.Getenv("REDIS_URL"))
	if err := scheduler.Start(); err != nil {
		log.Printf("WARNING: failed to start watchdog scheduler: %v", err)
	}
	defer scheduler.Stop()

	// Setup router
	r := gin.Default()
	r.Use(middleware.CORS(os.Getenv("FRONTEND_URL")))

	// Health check — verifies database connectivity so load balancers don't route to
	// an instance that can't serve requests.
	r.GET("/health", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := db.Pool.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "degraded", "database": "unreachable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// API routes
	v1 := r.Group("/api/v1")

	// GitHub OAuth callback — public (GitHub redirects here after authorization)
	v1.GET("/github/callback", githubSvc.HandleOAuthCallback)

	// Bootstrap template — public so the frontend can show it before auth
	v1.GET("/cloudformation/bootstrap-template", awsSvc.HandleGetBootstrapTemplate)

	// Rate limiters — applied per-user after auth on expensive endpoints.
	// conversation: 10 req/min (burst 5) — each message hits the Claude API
	// deploy:       5  req/min (burst 2) — each deploy triggers a CodeBuild job
	conversationRL := middleware.NewRateLimiter(10.0/60, 5)
	deployRL := middleware.NewRateLimiter(5.0/60, 2)

	// Protected routes (require a valid Clerk JWT)
	protected := v1.Group("/")
	protected.Use(middleware.RequireAuth(authSvc, db))
	{
		// GitHub
		protected.GET("/github/auth", githubSvc.HandleGetOAuthURL)
		protected.GET("/github/repos", githubSvc.HandleListRepos)
		protected.GET("/github/repos/:owner/:repo/branches", githubSvc.HandleListBranches)
		protected.GET("/github/repos/:owner/:repo/detect", githubSvc.HandleDetectFramework)

		// Projects (collection — ownership enforced inside the handlers)
		protected.POST("/projects", deploySvc.HandleCreateProject)
		protected.GET("/projects", deploySvc.HandleListProjects)

		// AWS Accounts (user-level — ownership enforced inside the handlers)
		protected.GET("/aws-accounts", awsSvc.HandleListAWSAccounts)
		protected.POST("/aws-accounts", awsSvc.HandleConnectAWSAccount)
		protected.DELETE("/aws-accounts/:id", awsSvc.HandleDeleteAWSAccount)
	}

	// Project-scoped routes — every handler here operates on a single project the
	// caller must own. RequireProjectOwnership is the single tenant-isolation guard
	// so individual handlers no longer need to re-check ownership of ":id".
	proj := v1.Group("/projects/:id")
	proj.Use(middleware.RequireAuth(authSvc, db))
	proj.Use(middleware.RequireProjectOwnership(db))
	{
		proj.GET("", deploySvc.HandleGetProject)

		// Environments
		proj.POST("/environments", awsSvc.HandleCreateEnvironment)
		proj.GET("/environments", awsSvc.HandleListEnvironments)
		proj.POST("/environments/:envId/retry-provision", awsSvc.HandleRetryProvision)
		proj.GET("/environments/:envId/logs", deploySvc.HandleGetLogs)

		// Deployments
		proj.POST("/environments/:envId/deploy", deployRL.Middleware(), deploySvc.HandleDeploy)
		proj.GET("/deployments", deploySvc.HandleListDeployments)
		proj.POST("/deployments/:deployId/rollback", deploySvc.HandleRollback)
		proj.POST("/deployments/:deployId/redeploy", deploySvc.HandleRedeploy)
		proj.DELETE("/deployments/:deployId", deploySvc.HandleDeleteDeployment)

		// Deployment events (operational timeline)
		proj.GET("/deployments/:deployId/events", eventSvc.HandleGetDeploymentEvents)

		// Diagnosis
		proj.GET("/deployments/:deployId/diagnose", diagnosisSvc.HandleDiagnose)

		// Conversation (REST fallback — primary is WebSocket)
		proj.POST("/conversation", conversationRL.Middleware(), conversationSvc.HandleMessage)
		proj.GET("/conversation/history", conversationSvc.HandleHistory)
	}

	// WebSocket — outside the auth middleware; auth + project-ownership are verified
	// via the first-message token (see wsAuthFn).
	v1.GET("/ws/:projectId", func(c *gin.Context) {
		hub.HandleUpgrade(c, wsAuthFn, conversationSvc)
	})

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	go func() {
		log.Printf("Server running on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited")
}
