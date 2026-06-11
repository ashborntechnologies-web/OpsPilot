package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/auth"
	"github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/billing"
	"github.com/ashborntechnologies-web/OpsPilot/internal/conversation"
	"github.com/ashborntechnologies-web/OpsPilot/internal/deploy"
	"github.com/ashborntechnologies-web/OpsPilot/internal/diagnosis"
	"github.com/ashborntechnologies-web/OpsPilot/internal/envvars"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	"github.com/ashborntechnologies-web/OpsPilot/internal/export"
	githubsvc "github.com/ashborntechnologies-web/OpsPilot/internal/github"
	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/internal/memory"
	"github.com/ashborntechnologies-web/OpsPilot/internal/monitor"
	"github.com/ashborntechnologies-web/OpsPilot/internal/notify"
	"github.com/ashborntechnologies-web/OpsPilot/internal/prompts"
	"github.com/ashborntechnologies-web/OpsPilot/internal/queue"
	"github.com/ashborntechnologies-web/OpsPilot/internal/terminal"
	"github.com/ashborntechnologies-web/OpsPilot/internal/users"
	"github.com/ashborntechnologies-web/OpsPilot/internal/webhooks"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	pkgws "github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	awssdk "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
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
		slog.Warn(fmt.Sprintf("WARNING: could not load AWS config: %v — bootstrap template unavailable", err))
		return os.Getenv("PLATFORM_AWS_ACCOUNT_ID"), ""
	}

	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		slog.Warn(fmt.Sprintf("WARNING: sts:GetCallerIdentity failed: %v — bootstrap template unavailable", err))
		return os.Getenv("PLATFORM_AWS_ACCOUNT_ID"), ""
	}

	callerARN = *out.Arn
	accountID = *out.Account

	// Allow explicit override of the account ID (e.g. for testing).
	if override := os.Getenv("PLATFORM_AWS_ACCOUNT_ID"); override != "" {
		accountID = override
	}

	slog.Info(fmt.Sprintf("Platform identity: account=%s arn=%s", accountID, callerARN))
	return accountID, callerARN
}

// validateEnv fails fast at startup when required configuration is missing,
// instead of surfacing as confusing runtime errors hours later.
func validateEnv() {
	required := []string{
		"DATABASE_URL",
		"REDIS_URL",
		"CLERK_SECRET_KEY",
		"CLERK_PUBLISHABLE_KEY",
		"ENCRYPTION_KEY",
	}
	var missing []string
	for _, key := range required {
		if os.Getenv(key) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		log.Fatalf("Missing required environment variables: %s — see .env.example", strings.Join(missing, ", "))
	}

	// The AES key is derived by SHA-256 hashing this value, so any length works —
	// just enforce a minimum so a trivially guessable key can't be used.
	if key := os.Getenv("ENCRYPTION_KEY"); len(key) < 16 {
		log.Fatalf("ENCRYPTION_KEY must be at least 16 characters (got %d) — GitHub tokens are encrypted with a key derived from it", len(key))
	}

	// AI prompts (trade secrets) — prompts.MustLoad panics if truly unconfigured;
	// this warning fires first so the operator sees what's about to be required.
	if os.Getenv("INTENT_CLASSIFIER_PROMPT") == "" && os.Getenv("INTENT_CLASSIFIER_PROMPT_FILE") == "" {
		log.Println("WARNING: INTENT_CLASSIFIER_PROMPT(_FILE) not set — startup will fail; prompts are not embedded in the binary")
	}
	if os.Getenv("DIAGNOSIS_PROMPT") == "" && os.Getenv("DIAGNOSIS_PROMPT_FILE") == "" {
		log.Println("WARNING: DIAGNOSIS_PROMPT(_FILE) not set — startup will fail; prompts are not embedded in the binary")
	}

	// Optional integrations — warn so operators know a feature is disabled, not broken.
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Println("WARNING: ANTHROPIC_API_KEY not set — chat intent classification, AI diagnosis, and AI framework detection are disabled")
	}
	if os.Getenv("GITHUB_CLIENT_ID") == "" || os.Getenv("GITHUB_CLIENT_SECRET") == "" {
		log.Println("WARNING: GitHub OAuth credentials not set — repository connection is disabled")
	}
	if os.Getenv("FRONTEND_URL") == "" {
		log.Println("WARNING: FRONTEND_URL not set — CORS and WebSocket origin checks allow all origins (dev mode)")
	}
	if os.Getenv("ADMIN_API_KEY") == "" {
		log.Println("WARNING: ADMIN_API_KEY not set — admin training-data export endpoints are disabled")
	}
}

func main() {
	// Structured JSON logging — every component logs through slog.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Load env
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system env")
	}

	validateEnv()

	// AI prompts are trade secrets loaded from the environment, never embedded in
	// source. Panics with setup instructions when unconfigured.
	prompts.MustLoad()

	// Run Gin in release mode outside development to avoid debug logging overhead.
	if env := os.Getenv("ENV"); env != "" && env != "development" {
		gin.SetMode(gin.ReleaseMode)
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
		os.Getenv("ANTHROPIC_API_KEY"),
	)
	platformAccountID, platformCallerARN := resolvePlatformIdentity()
	awsSvc := aws.NewService(db, platformAccountID, platformCallerARN)

	// Queue client is used by the deploy service to enqueue jobs.
	// Queue server is separate to avoid a circular import (server imports deploy service).
	queueClient := queue.NewClient(os.Getenv("REDIS_URL"))
	defer queueClient.Close()

	eventSvc := events.NewService(db)
	awsSvc.SetEvents(eventSvc) // enable account-level audit events (e.g. external_id.generated)
	envVarSvc := envvars.NewService(db)
	webhookSvc := webhooks.NewService(db)
	terminalSvc := terminal.NewService(db, awsSvc, authSvc)
	deploySvc := deploy.NewService(db, awsSvc, githubSvc, hub, queueClient, eventSvc, envVarSvc, webhookSvc)

	// Notification service — no-op (logging) when SMTP is not configured.
	emailSvc := notify.NewEmailService(
		os.Getenv("SMTP_HOST"), os.Getenv("SMTP_PORT"),
		os.Getenv("SMTP_USER"), os.Getenv("SMTP_PASS"),
		os.Getenv("SMTP_FROM"),
	)

	// Project memory — long-term facts injected into diagnosis prompts.
	memorySvc := memory.NewService(db, llm.New(os.Getenv("ANTHROPIC_API_KEY")))

	// Alert engine — turns operational events into user-facing alerts.
	alertEngine := monitor.NewAlertEngine(db, llm.New(os.Getenv("ANTHROPIC_API_KEY")), hub, emailSvc)
	eventSvc.SetAlertEngine(alertEngine)
	eventSvc.SetDiagnosisEnqueuer(queueClient)

	// Billing limits + users endpoints.
	billingSvc := billing.NewService(db)
	usersSvc := users.NewService(db, billingSvc)

	deploySvc.SetEmailService(emailSvc)
	deploySvc.SetMemoryService(memorySvc)
	deploySvc.SetRiskLLM(llm.New(os.Getenv("ANTHROPIC_API_KEY")))
	deploySvc.SetBillingService(billingSvc)

	// After an environment is created with a linked AWS account, auto-trigger provisioning.
	awsSvc.SetOnEnvCreated(func(projectID, environmentID uuid.UUID) {
		if err := queueClient.EnqueueProvision(projectID.String(), environmentID.String()); err != nil {
			slog.Error(fmt.Sprintf("failed to enqueue provision job for env %s: %v", environmentID, err))
		}
	})
	diagnosisSvc := diagnosis.NewService(db, awsSvc, eventSvc, os.Getenv("ANTHROPIC_API_KEY"))
	diagnosisSvc.SetMemoryService(memorySvc)
	conversationSvc := conversation.NewService(db, deploySvc, diagnosisSvc, os.Getenv("ANTHROPIC_API_KEY"), hub)
	conversationSvc.SetBillingService(billingSvc)

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
		slog.Warn(fmt.Sprintf("WARNING: failed to start watchdog scheduler: %v", err))
	}
	defer scheduler.Stop()

	// Continuous monitoring — health poller (ECS/ALB, every 60s) and log anomaly
	// scanner (every 5m), one worker per ready environment.
	poller := monitor.NewPoller(db, awsSvc, eventSvc, hub)
	go poller.Start()
	defer poller.Stop()

	logScanner := monitor.NewLogScanner(db, awsSvc, eventSvc, hub)
	go logScanner.Start()
	defer logScanner.Stop()

	// Setup router — gin.New (not Default) so the platform controls every header;
	// logging and panic recovery are attached explicitly.
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.Proprietary())
	r.Use(middleware.CORS(os.Getenv("FRONTEND_URL")))

	// robots.txt — this host only serves the API (and the proprietary admin export
	// endpoints). Nothing here should ever be crawled or indexed; disallow everything.
	r.GET("/robots.txt", func(c *gin.Context) {
		c.String(http.StatusOK, "User-agent: *\nDisallow: /\n")
	})

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

	// Product metadata — public, used by clients and license tooling.
	v1.GET("/meta", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"product":   "OpsPilot",
			"version":   middleware.Version(),
			"license":   "BUSL-1.1",
			"terms":     "https://opspilot.dev/terms",
			"ip_notice": "OpsPilot's AI prompts, models, and training data are proprietary trade secrets. Competitive use is prohibited.",
		})
	})

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
		// Account (plan, usage, notification preferences)
		protected.GET("/users/me", usersSvc.HandleGetMe)
		protected.PATCH("/users/me/notifications", usersSvc.HandleUpdateNotifications)

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
		proj.DELETE("", deploySvc.HandleDeleteProject)

		// Environments
		proj.POST("/environments", awsSvc.HandleCreateEnvironment)
		proj.GET("/environments", awsSvc.HandleListEnvironments)
		proj.POST("/environments/:envId/retry-provision", awsSvc.HandleRetryProvision)
		proj.GET("/environments/:envId/logs", deploySvc.HandleGetLogs)

		// Env vars (injected into the ECS task definition at deploy time)
		proj.GET("/environments/:envId/env-vars", envVarSvc.HandleList)
		proj.PUT("/environments/:envId/env-vars", envVarSvc.HandleUpsert)
		proj.DELETE("/environments/:envId/env-vars/:varId", envVarSvc.HandleDelete)
		proj.GET("/environments/:envId/env-vars/:varId/reveal", envVarSvc.HandleReveal)

		// Health check + scaling
		proj.GET("/environments/:envId/health", deploySvc.HandleCheckHealth)
		proj.POST("/environments/:envId/scale", deploySvc.HandleScaleService)

		// Webhooks
		proj.GET("/webhooks", webhookSvc.HandleList)
		proj.POST("/webhooks", webhookSvc.HandleCreate)
		proj.PATCH("/webhooks/:webhookId", webhookSvc.HandleUpdate)
		proj.DELETE("/webhooks/:webhookId", webhookSvc.HandleDelete)

		// Deployments
		proj.POST("/environments/:envId/deploy", deployRL.Middleware(), deploySvc.HandleDeploy)
		proj.GET("/deployments", deploySvc.HandleListDeployments)
		proj.POST("/deployments/:deployId/rollback", deploySvc.HandleRollback)
		proj.POST("/deployments/:deployId/redeploy", deploySvc.HandleRedeploy)
		proj.DELETE("/deployments/:deployId", deploySvc.HandleDeleteDeployment)

		// Deployment events (operational timeline)
		proj.GET("/deployments/:deployId/events", eventSvc.HandleGetDeploymentEvents)

		// Project-wide recent events (sidebar activity feed)
		proj.GET("/events", eventSvc.HandleGetProjectEvents)

		// Alerts
		proj.GET("/alerts", alertEngine.HandleListAlerts)
		proj.POST("/alerts/:alertId/snooze", alertEngine.HandleSnooze)
		proj.POST("/alerts/:alertId/resolve", alertEngine.HandleResolve)

		// Deploy cancellation
		proj.POST("/deployments/:deployId/cancel", deploySvc.HandleCancelDeployment)

		// Project settings
		proj.PATCH("", deploySvc.HandleUpdateProject)

		// Diagnosis
		proj.GET("/deployments/:deployId/diagnose", diagnosisSvc.HandleDiagnose)
		proj.POST("/deployments/:deployId/diagnose/feedback", diagnosisSvc.HandleSubmitFeedback)
		proj.GET("/diagnose/feedback-summary", diagnosisSvc.HandleFeedbackSummary)

		// Cost intelligence
		proj.GET("/costs", deploySvc.HandleGetCosts)

		// Deployment health score (computed from platform data, no AWS calls)
		proj.GET("/health-score", deploySvc.HandleGetHealthScore)

		// PR Preview Environments
		proj.POST("/previews/enable", deploySvc.HandleEnablePreviews)
		proj.POST("/previews/disable", deploySvc.HandleDisablePreviews)

		// Conversation (REST fallback — primary is WebSocket)
		proj.POST("/conversation", conversationRL.Middleware(), conversationSvc.HandleMessage)
		proj.GET("/conversation/history", conversationSvc.HandleHistory)
	}

	// Admin — training data exports (trade secret datasets). Protected by a static
	// bearer key (ADMIN_API_KEY), not Clerk; 404s when no key is configured.
	exportSvc := export.NewService(db)
	admin := v1.Group("/admin", middleware.ApiKeyAuth(os.Getenv("ADMIN_API_KEY")))
	{
		admin.GET("/export/intents", exportSvc.HandleExportIntents)
		admin.GET("/export/diagnoses", exportSvc.HandleExportDiagnoses)
	}

	// GitHub webhook — public; authentication via HMAC-SHA256 signature.
	v1.POST("/github/webhook", deploySvc.HandleGithubWebhook)

	// WebSocket — outside the auth middleware; auth + project-ownership are verified
	// via the first-message token (see wsAuthFn).
	v1.GET("/ws/:projectId", func(c *gin.Context) {
		hub.HandleUpgrade(c, wsAuthFn, conversationSvc)
	})

	// Terminal WebSocket — auth via first-message token (browsers cannot set custom headers).
	v1.GET("/ws/:projectId/terminal/:envId", terminalSvc.HandleTerminal)

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
		// ReadHeaderTimeout protects against slowloris. Full read/write timeouts are
		// deliberately absent — they would kill long-lived WebSocket connections.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info(fmt.Sprintf("Server running on port %s", port))
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
