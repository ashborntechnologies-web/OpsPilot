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

	"github.com/ashborntechnologies-web/OpsPilot/internal/analytics"
	"github.com/ashborntechnologies-web/OpsPilot/internal/auth"
	"github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/billing"
	"github.com/ashborntechnologies-web/OpsPilot/internal/conversation"
	"github.com/ashborntechnologies-web/OpsPilot/internal/deploy"
	"github.com/ashborntechnologies-web/OpsPilot/internal/diagnosis"
	"github.com/ashborntechnologies-web/OpsPilot/internal/discovery"
	"github.com/ashborntechnologies-web/OpsPilot/internal/envvars"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	"github.com/ashborntechnologies-web/OpsPilot/internal/export"
	githubsvc "github.com/ashborntechnologies-web/OpsPilot/internal/github"
	"github.com/ashborntechnologies-web/OpsPilot/internal/incidents"
	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/internal/memory"
	"github.com/ashborntechnologies-web/OpsPilot/internal/monitor"
	"github.com/ashborntechnologies-web/OpsPilot/internal/notify"
	"github.com/ashborntechnologies-web/OpsPilot/internal/orgs"
	"github.com/ashborntechnologies-web/OpsPilot/internal/postmortem"
	"github.com/ashborntechnologies-web/OpsPilot/internal/prompts"
	"github.com/ashborntechnologies-web/OpsPilot/internal/queue"
	"github.com/ashborntechnologies-web/OpsPilot/internal/slack"
	"github.com/ashborntechnologies-web/OpsPilot/internal/summary"
	"github.com/ashborntechnologies-web/OpsPilot/internal/terminal"
	"github.com/ashborntechnologies-web/OpsPilot/internal/trust"
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
	if os.Getenv("SLACK_CLIENT_ID") == "" || os.Getenv("SLACK_CLIENT_SECRET") == "" || os.Getenv("SLACK_SIGNING_SECRET") == "" {
		log.Println("WARNING: SLACK_CLIENT_ID/SLACK_CLIENT_SECRET/SLACK_SIGNING_SECRET not all set — Slack integration (notifications, slash commands) is disabled")
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
		// Any member of the project's org may open the socket (viewers receive live
		// deploy/alert broadcasts). Action enforcement happens per-message in the
		// conversation engine, which blocks viewers from triggering actions.
		if _, _, err := db.ProjectOrgRole(ctx, userID, pid); err != nil {
			return uuid.UUID{}, errors.New("forbidden: not a member of this project's workspace")
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
	envVarSvc := envvars.NewService(db, os.Getenv("ENCRYPTION_KEY"), os.Getenv("ENCRYPTION_KEY_PREV"))
	// Encrypt any pre-existing plaintext secret env vars at rest (one-time, idempotent).
	if err := envVarSvc.EncryptExistingSecrets(context.Background()); err != nil {
		slog.Warn(fmt.Sprintf("WARNING: failed to backfill-encrypt secret env vars: %v", err))
	}
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

	// Organizations (team workspaces) + RBAC.
	orgsSvc := orgs.NewService(db, emailSvc, os.Getenv("FRONTEND_URL"))

	// Infrastructure discovery — scans connected AWS accounts for existing resources.
	discoverySvc := discovery.NewService(db, awsSvc)
	discoverySvc.SetEnqueuer(queueClient)

	// When an AWS account is connected, kick off an initial discovery scan.
	awsSvc.SetOnAccountConnected(func(orgID, accountID uuid.UUID) {
		if _, err := queueClient.EnqueueScan(accountID.String()); err != nil {
			slog.Error(fmt.Sprintf("failed to enqueue initial scan for account %s: %v", accountID, err))
		}
	})

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
	// Incident war room — incidents service is injected into diagnosis so completed
	// diagnoses open a shared, real-time incident with an AI+human timeline.
	incidentsSvc := incidents.NewService(db, llm.New(os.Getenv("ANTHROPIC_API_KEY")), hub)

	// Postmortems — generated asynchronously when an incident is resolved (ADR-014).
	postmortemSvc := postmortem.NewService(db, llm.New(os.Getenv("ANTHROPIC_API_KEY")), memorySvc)
	incidentsSvc.SetPostmortemEnqueuer(queueClient)

	diagnosisSvc := diagnosis.NewService(db, awsSvc, eventSvc, os.Getenv("ANTHROPIC_API_KEY"))
	diagnosisSvc.SetMemoryService(memorySvc)
	diagnosisSvc.SetIncidentService(incidentsSvc)
	conversationSvc := conversation.NewService(db, deploySvc, diagnosisSvc, os.Getenv("ANTHROPIC_API_KEY"), hub)
	conversationSvc.SetBillingService(billingSvc)

	// Slack integration — alert/deploy notifications, daily summary, slash commands.
	slackSvc := slack.NewService(
		db,
		os.Getenv("ENCRYPTION_KEY"), os.Getenv("ENCRYPTION_KEY_PREV"),
		os.Getenv("SLACK_CLIENT_ID"), os.Getenv("SLACK_CLIENT_SECRET"), os.Getenv("SLACK_SIGNING_SECRET"),
		os.Getenv("PUBLIC_API_URL"), os.Getenv("FRONTEND_URL"),
	)
	slackSvc.SetDeployer(deploySvc)
	deploySvc.SetSlackNotifier(slackSvc)
	alertEngine.SetSlackNotifier(slackSvc)

	// Trust levels + AI-action approval workflow. AI-initiated actions (chat, diagnosis)
	// route through ProposeAction, which auto-executes or registers a pending approval
	// based on the environment's trust level.
	trustSvc := trust.NewService(db, deploySvc, hub, os.Getenv("FRONTEND_URL"))
	trustSvc.SetSlack(slackSvc)
	trustSvc.SetIncidents(incidentsSvc)
	conversationSvc.SetTrustService(trustSvc)
	diagnosisSvc.SetTrustService(trustSvc)

	// Daily operational summary — AI morning briefing posted to Slack + emailed.
	summarySvc := summary.NewService(db, llm.New(os.Getenv("ANTHROPIC_API_KEY")), emailSvc, awsSvc, os.Getenv("FRONTEND_URL"))
	summarySvc.SetSlack(slackSvc)
	summarySvc.SetEnqueuer(queueClient)

	// Engineering leadership dashboard — SLA/uptime, MTTD/MTTR, reliability trends, and the
	// monthly operational health report (ADR-015).
	analyticsSvc := analytics.NewService(db, llm.New(os.Getenv("ANTHROPIC_API_KEY")), emailSvc, awsSvc, os.Getenv("FRONTEND_URL"))
	analyticsSvc.SetSlack(slackSvc)

	// Init job queue server
	queueServer := queue.NewServer(
		os.Getenv("REDIS_URL"),
		deploySvc,
		diagnosisSvc,
		discoverySvc,
		summarySvc,
		postmortemSvc,
		analyticsSvc,
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

		// Projects (collection — org-scoped via the X-Org-Id header inside the handlers)
		protected.POST("/projects", deploySvc.HandleCreateProject)
		protected.GET("/projects", deploySvc.HandleListProjects)

		// Account (plan, usage, notification preferences)
		protected.GET("/users/me", usersSvc.HandleGetMe)
		protected.PATCH("/users/me/notifications", usersSvc.HandleUpdateNotifications)

		// AWS Accounts (active-workspace-scoped; connect/delete are admin-only,
		// enforced inside the handlers via the active org role).
		protected.GET("/aws-accounts", awsSvc.HandleListAWSAccounts)
		protected.POST("/aws-accounts", awsSvc.HandleConnectAWSAccount)
		protected.DELETE("/aws-accounts/:id", awsSvc.HandleDeleteAWSAccount)

		// Infrastructure discovery (org/role enforced inside the handlers).
		protected.POST("/aws-accounts/:id/scan", discoverySvc.HandleScanAccount)
		protected.PATCH("/resources/:resourceId/assign", discoverySvc.HandleAssignResource)

		// AI action approvals (engineer+ enforced against the action's org in the handler).
		protected.POST("/actions/:actionId/approve", trustSvc.HandleApprove)
		protected.POST("/actions/:actionId/reject", trustSvc.HandleReject)

		// Postmortems — edit/publish/export. Org membership + role checked inside each
		// handler (the postmortem resolves its own org).
		protected.GET("/postmortems/:postmortemId", postmortemSvc.HandleGet)
		protected.PATCH("/postmortems/:postmortemId", postmortemSvc.HandleUpdate)
		protected.POST("/postmortems/:postmortemId/publish", postmortemSvc.HandlePublish)
		protected.GET("/postmortems/:postmortemId/export", postmortemSvc.HandleExport)

		// Organizations (team workspaces)
		protected.POST("/orgs", orgsSvc.HandleCreateOrg)
		protected.GET("/orgs/me", orgsSvc.HandleListMyOrgs)
		// Invite acceptance — any authenticated user with the token can redeem it.
		protected.GET("/invites/:token", orgsSvc.HandleAcceptInvite)
	}

	// Organization management — scoped to a single org the caller belongs to.
	// RequireOrgMembership loads the caller's role and stores org context; member
	// management is admin-only, listing is open to any member.
	org := v1.Group("/orgs/:orgId")
	org.Use(middleware.RequireAuth(authSvc, db))
	org.Use(middleware.RequireOrgMembership(db)) // any member; per-route role below
	{
		org.GET("/members", orgsSvc.HandleListMembers)
		org.GET("/resources", discoverySvc.HandleListOrgResources) // discovered resource inventory
		org.GET("/incidents", incidentsSvc.HandleListOrgIncidents) // war-room incident list
		org.GET("/postmortems", postmortemSvc.HandleListOrg)       // published postmortem library (SOC2)
		org.GET("/actions", trustSvc.HandleListOrgActions)         // pending AI-action approvals

		// Engineering leadership dashboard — org-wide reliability metrics + monthly reports.
		org.GET("/analytics", analyticsSvc.HandleOrgAnalytics)
		org.GET("/reports", analyticsSvc.HandleListReports)
		org.POST("/reports/generate", middleware.RequireRole(models.RoleAdmin), analyticsSvc.HandleGenerateReport)

		// On-call schedule — quiet hours that suppress warn-level alert notifications (ADR-016).
		org.GET("/oncall-schedule", alertEngine.HandleGetOncallSchedule)
		org.PUT("/oncall-schedule", middleware.RequireRole(models.RoleAdmin), alertEngine.HandlePutOncallSchedule)

		// Slack integration — read for any member; install/config/disconnect are admin.
		org.GET("/slack", slackSvc.HandleGetIntegration)
		org.GET("/slack/channels", slackSvc.HandleListChannels)
		org.GET("/slack/install", middleware.RequireRole(models.RoleAdmin), slackSvc.HandleInstallURL)
		org.PATCH("/slack", middleware.RequireRole(models.RoleAdmin), slackSvc.HandleUpdateChannels)
		org.DELETE("/slack", middleware.RequireRole(models.RoleAdmin), slackSvc.HandleDisconnect)

		// Daily operational summaries — reads for any member; generate/config are admin.
		org.GET("/summaries", summarySvc.HandleListSummaries)
		org.GET("/summaries/latest", summarySvc.HandleLatestSummary)
		org.POST("/summaries/generate", middleware.RequireRole(models.RoleAdmin), summarySvc.HandleGenerateNow)
		org.PATCH("/summary-config", middleware.RequireRole(models.RoleAdmin), summarySvc.HandleUpdateConfig)

		org.POST("/invites", middleware.RequireRole(models.RoleAdmin), orgsSvc.HandleCreateInvite)
		org.PATCH("/members/:userId", middleware.RequireRole(models.RoleAdmin), orgsSvc.HandleUpdateMemberRole)
		org.DELETE("/members/:userId", middleware.RequireRole(models.RoleAdmin), orgsSvc.HandleRemoveMember)
	}

	// Project-scoped routes — every handler operates on a single project owned by an
	// org the caller belongs to. LoadProjectMembership resolves project→org→role and
	// is the tenant-isolation guard (404 for non-members). RequireRole then enforces
	// the role hierarchy per action:
	//   viewer   — all reads (no extra guard)
	//   engineer — deploy, rollback, scale, env-var writes, alert ack, chat, webhooks
	//   admin    — create/delete environment, delete project, settings, AWS-linked ops
	requireEngineer := middleware.RequireRole(models.RoleEngineer)
	requireAdmin := middleware.RequireRole(models.RoleAdmin)

	proj := v1.Group("/projects/:id")
	proj.Use(middleware.RequireAuth(authSvc, db))
	proj.Use(middleware.LoadProjectMembership(db))
	{
		// Reads — any member (viewer+).
		proj.GET("", deploySvc.HandleGetProject)
		proj.GET("/environments", awsSvc.HandleListEnvironments)
		proj.GET("/environments/:envId/logs", deploySvc.HandleGetLogs)
		proj.GET("/environments/:envId/env-vars", envVarSvc.HandleList)
		proj.GET("/environments/:envId/health", deploySvc.HandleCheckHealth)
		proj.GET("/webhooks", webhookSvc.HandleList)
		proj.GET("/deployments", deploySvc.HandleListDeployments)
		proj.GET("/deployments/:deployId/events", eventSvc.HandleGetDeploymentEvents)
		proj.GET("/events", eventSvc.HandleGetProjectEvents)
		proj.GET("/alerts", alertEngine.HandleListAlerts)
		proj.GET("/deployments/:deployId/diagnose", diagnosisSvc.HandleDiagnose)
		proj.GET("/diagnose/feedback-summary", diagnosisSvc.HandleFeedbackSummary)
		proj.GET("/costs", deploySvc.HandleGetCosts)
		proj.GET("/health-score", deploySvc.HandleGetHealthScore)
		proj.GET("/resources", discoverySvc.HandleListProjectResources)
		proj.GET("/incidents", incidentsSvc.HandleListProjectIncidents)
		proj.GET("/actions", trustSvc.HandleListProjectActions)
		proj.GET("/environments/:envId/trust", trustSvc.HandleGetTrust)
		proj.PATCH("/environments/:envId/trust", requireAdmin, trustSvc.HandleUpdateTrust)
		proj.GET("/conversation/history", conversationSvc.HandleHistory)

		// Project-level analytics (leadership dashboard) + per-environment SLA config.
		proj.GET("/analytics", analyticsSvc.HandleProjectAnalytics)
		proj.GET("/uptime", analyticsSvc.HandleProjectUptime)
		proj.GET("/environments/:envId/sla", analyticsSvc.HandleGetSLA)
		proj.PUT("/environments/:envId/sla", requireEngineer, analyticsSvc.HandleSetSLA)

		// Engineer actions — deploy, rollback, scale, env vars, alerts, chat, webhooks.
		proj.POST("/environments/:envId/deploy", requireEngineer, deployRL.Middleware(), deploySvc.HandleDeploy)
		proj.POST("/deployments/:deployId/rollback", requireEngineer, deploySvc.HandleRollback)
		proj.POST("/deployments/:deployId/redeploy", requireEngineer, deploySvc.HandleRedeploy)
		proj.POST("/deployments/:deployId/cancel", requireEngineer, deploySvc.HandleCancelDeployment)
		proj.DELETE("/deployments/:deployId", requireEngineer, deploySvc.HandleDeleteDeployment)
		proj.POST("/environments/:envId/scale", requireEngineer, deploySvc.HandleScaleService)
		proj.PUT("/environments/:envId/env-vars", requireEngineer, envVarSvc.HandleUpsert)
		proj.DELETE("/environments/:envId/env-vars/:varId", requireEngineer, envVarSvc.HandleDelete)
		proj.GET("/environments/:envId/env-vars/:varId/reveal", requireEngineer, envVarSvc.HandleReveal)
		proj.POST("/alerts/:alertId/snooze", requireEngineer, alertEngine.HandleSnooze)
		proj.POST("/alerts/:alertId/resolve", requireEngineer, alertEngine.HandleResolve)
		proj.POST("/deployments/:deployId/diagnose/feedback", requireEngineer, diagnosisSvc.HandleSubmitFeedback)
		proj.POST("/conversation", requireEngineer, conversationRL.Middleware(), conversationSvc.HandleMessage)
		proj.POST("/webhooks", requireEngineer, webhookSvc.HandleCreate)
		proj.PATCH("/webhooks/:webhookId", requireEngineer, webhookSvc.HandleUpdate)
		proj.DELETE("/webhooks/:webhookId", requireEngineer, webhookSvc.HandleDelete)
		proj.POST("/previews/enable", requireEngineer, deploySvc.HandleEnablePreviews)
		proj.POST("/previews/disable", requireEngineer, deploySvc.HandleDisablePreviews)

		// Admin actions — provisioning, deletion, settings.
		proj.POST("/environments", requireAdmin, awsSvc.HandleCreateEnvironment)
		proj.POST("/environments/:envId/retry-provision", requireAdmin, awsSvc.HandleRetryProvision)
		proj.DELETE("", requireAdmin, deploySvc.HandleDeleteProject)
		proj.PATCH("", requireAdmin, deploySvc.HandleUpdateProject)
	}

	// Incident war room — single-incident routes. Auth is RequireAuth; org membership +
	// role are checked inside each handler (the incident resolves its own org). Reads
	// need any member; timeline/ack/resolve/actions need engineer+.
	inc := v1.Group("/incidents/:incidentId")
	inc.Use(middleware.RequireAuth(authSvc, db))
	{
		inc.GET("", incidentsSvc.HandleGetIncident)
		inc.POST("/timeline", incidentsSvc.HandlePostTimeline)
		inc.POST("/acknowledge", incidentsSvc.HandleAcknowledge)
		inc.POST("/resolve", incidentsSvc.HandleResolve)
		inc.GET("/postmortem", postmortemSvc.HandleGetByIncident) // 404 {generating} while async generation runs
		inc.POST("/actions/:actionId/approve", incidentsSvc.HandleApproveAction)
		inc.POST("/actions/:actionId/reject", incidentsSvc.HandleRejectAction)
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

	// Slack — public endpoints. OAuth callback (trust = signed state); slash commands and
	// interactive components (trust = X-Slack-Signature HMAC).
	v1.GET("/slack/callback", slackSvc.HandleCallback)
	v1.POST("/slack/commands", slackSvc.HandleCommand)
	v1.POST("/slack/interactivity", slackSvc.HandleInteractivity)

	// WebSocket — outside the auth middleware; auth + project-ownership are verified
	// via the first-message token (see wsAuthFn).
	v1.GET("/ws/:projectId", func(c *gin.Context) {
		hub.HandleUpgrade(c, wsAuthFn, conversationSvc)
	})

	// Terminal WebSocket — auth via first-message token (browsers cannot set custom headers).
	v1.GET("/ws/:projectId/terminal/:envId", terminalSvc.HandleTerminal)

	// Incident war-room WebSocket — broadcast-only stream of timeline entries + status
	// updates. Auth via first-message token; access requires membership of the incident's
	// org (any role — viewers watch live).
	incidentWsAuthFn := pkgws.RoomAuthFunc(func(ctx context.Context, token, incidentID string) (uuid.UUID, error) {
		userID, err := middleware.ResolveToken(ctx, db, authSvc, token)
		if err != nil {
			return uuid.UUID{}, err
		}
		iid, err := uuid.Parse(incidentID)
		if err != nil {
			return uuid.UUID{}, err
		}
		var orgID *uuid.UUID
		if err := db.Pool.QueryRow(ctx, `SELECT org_id FROM incidents WHERE id = $1`, iid).Scan(&orgID); err != nil || orgID == nil {
			return uuid.UUID{}, errors.New("forbidden: incident not found")
		}
		if _, err := db.UserOrgRole(ctx, userID, *orgID); err != nil {
			return uuid.UUID{}, errors.New("forbidden: not a member of this incident's workspace")
		}
		return userID, nil
	})
	v1.GET("/ws/incidents/:incidentId", func(c *gin.Context) {
		hub.HandleIncidentUpgrade(c, incidentWsAuthFn)
	})

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
