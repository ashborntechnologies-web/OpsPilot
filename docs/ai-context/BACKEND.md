# Backend Reference — OpsPilot

Go monolith. Module `github.com/ashborntechnologies-web/OpsPilot`. Each `internal/<domain>`
package exposes a `Service` constructed once in `cmd/api/main.go`. Raw `pgx` for DB
(no ORM). Below: every backend module — purpose, key files, important types, and
cross-module dependencies.

> Convention: `Handle*` = Gin HTTP adapter; the matching context-taking method holds
> reusable logic (called by the conversation engine and queue workers too).

---

## `cmd/api/main.go` — composition root
Wires every service, runs migrations, mounts routes, starts the HTTP server +
WebSocket hub + Asynq queue server + watchdog scheduler + monitoring Poller +
LogScanner. `validateEnv()` fails fast on missing config; `prompts.MustLoad()` panics
without trade-secret prompts. `resolvePlatformIdentity()` reads the platform AWS
account/ARN for the bootstrap template + same-account AssumeRole.

---

## `internal/auth` — Clerk identity
- **Purpose:** validate Clerk JWTs and fetch Clerk user profiles.
- **Files:** `service.go`.
- **Key types:** `Service` (caches JWKS), `Claims`, `ClerkUser` (`PrimaryEmail()`).
- **Key methods:** `ValidateToken` (RS256 via cached JWKS, auto-refresh on unknown
  kid), `FetchClerkUser`.
- **Consumed by:** `pkg/middleware/auth.go`, `terminal` (WS auth).

## `internal/llm` — Claude client
- **Purpose:** the only Anthropic API client.
- **Files:** `client.go`.
- **Key type:** `Client`; **method:** `Complete(ctx, system, userMessage, maxTokens)`.
- **Behavior:** retry with backoff on retryable errors; `APIError` for non-2xx.
- **Consumed by:** `conversation`, `diagnosis`, `memory`, `monitor` (alert summaries),
  `deploy` (risk explanation).

## `internal/prompts` — trade-secret prompt loader
- **Purpose:** load intent-classifier + diagnosis prompts from env/file at startup.
- **Files:** `prompts.go`. **API:** `MustLoad()` (panics if unconfigured),
  `IntentClassifier()`, `Diagnosis()`. Never embeds prompt text in source.

## `internal/conversation` — chat engine (intent → action)
- **Purpose:** classify a user message into an intent and route to a Go workflow.
- **Files:** `service.go`.
- **Key types:** `Service`, `IntentResult{Intent, Params}`.
- **Core method:** `ProcessMessage(ctx, projectID, userID, message)` — meters the AI
  action (billing), prepends recent conversation context, classifies via Claude
  (`classifyIntent` → JSON), then `switch`es on intent to
  `deploy.TriggerDeploy/TriggerRollback/FetchLogsForProject/CheckHealth/ScaleService/
  GetCostSummary/ProposeResourceChange/ApplyPendingMutation` or
  `diagnosis.DiagnoseProject`. Degrades gracefully on classifier failure.
- **HTTP:** `HandleMessage` (REST fallback), `HandleHistory`. Primary transport is WS
  (the hub calls `ProcessMessage` as its `MessageHandler`).
- **Depends on:** `llm`, `prompts`, `deploy`, `diagnosis`, `billing`, `ws`, `models`.

## `internal/deploy` — the orchestration core (largest module)
- **Purpose:** deploy/rollback/provision workflows, PR previews, risk & health scores,
  cost intelligence, resource mutations, project/deployment CRUD, GitHub webhook.
- **Files:** `service.go` (~2700 lines), `riskscore.go`, `healthscore.go`,
  `handlers_test.go`, `validate_test.go`, `riskscore_test.go`.
- **Key type:** `Service` (deps: aws, github, ws hub, `Enqueuer`, events, envvars,
  webhooks; optional via `Set*`: email, memory, riskLLM, billing).
- **Workflows (run in Asynq workers):**
  - `RunDeployWorkflow` — AssumeRole → CodeBuild (build/push image) → stream build
    logs → register task def (inject env vars) → ensure target group/listener/ECS
    service → wait stable → health check → live or rollback. Emits operational events.
  - `RunProvisionWorkflow` — ensure shared platform stack + per-env project stack.
  - `RunRollbackWorkflow`, `RunDeleteProjectCleanup`, `runPreviewDeployWorkflow`.
  - `ReconcileStuckResources` — watchdog target (auto-fail stuck deploys).
- **Conversation-facing methods:** `TriggerDeploy`, `TriggerRollback`,
  `FetchLogsForProject`, `CheckHealth`, `ScaleService`, `GetCostSummary`,
  `ProposeResourceChange` + `ApplyPendingMutation` (CPU/memory change behind confirm).
- **Scoring:** `ComputeRiskScore` (pre-deploy, advisory, broadcast as `deploy_risk`;
  factors include recent failures, open alerts, Friday-afternoon), `ComputeHealthScore`.
- **HTTP:** project/deployment/env CRUD, deploy/rollback/redeploy/cancel, logs, health,
  scale, costs, previews enable/disable, settings, `HandleGithubWebhook` (HMAC-verified).
- **Depends on:** `aws`, `github`, `events`, `envvars`, `webhooks`, `notify`, `memory`,
  `billing`, `llm`, `queue` (via `Enqueuer`), `ws`, `models`.

## `internal/aws` — cloud provider (BYOC)
- **Purpose:** all AWS calls in the user's account via assumed roles.
- **Files:** `service.go` (~2200 lines), `cloudformation.go` (platform-stack CF
  template incl. optional HTTPS listener), `monitoring.go` (ALB metrics, CodeBuild log
  streaming/stop), `provider.go` (`AWSProvider` interface for mocking), `service_test.go`.
- **Key types:** `Service`, `ClientBundle` (per-request set of AWS SDK clients from an
  assumed role), `ServiceHealth`, `ALBMetrics`, `StartCodeBuildResult`.
- **Highlights:** `AssumeRoleForEnvironment`/`AssumeRoleForAccount` (per-tenant
  external ID; `explainAssumeRoleError` for friendly messages), `GetOrCreatePlatformStack`
  (shared VPC/ECS/ALB per account×region), `StartCodeBuildJob` + `WaitForCodeBuild`,
  `RegisterECSTaskDefinition`, `EnsureTargetGroup/EnsureListenerRule/EnsureECSService`,
  `WaitForECSServiceStable`, SSM secure-param helpers (GitHub token for build),
  `StartExecSession` (terminal), `FetchRecentECSLogs`, `GetAccountCostSummary`,
  `CreatePreviewService`/`TeardownPreviewService`, bootstrap template + CloudShell
  script rendering, Dockerfile/buildspec generation per framework.
- **Depends on:** `events`, `awstags`, `models`, AWS SDK v2.

## `internal/awstags` — resource tagging
- `BuildResourceTags(projectID, envName, platformAccountID)` → converters
  `ToCloudFormation/ToECS/ToELB`. Consistent ownership tags on every created resource.

## `internal/diagnosis` — root-cause AI
- **Purpose:** explain deploy failures and runtime anomalies; capture feedback.
- **Files:** `service.go`, `feedback.go`, `service_test.go`.
- **Key methods:** `DiagnoseProject` (last failed deploy), `diagnose` (logs + event
  timeline + history + past incidents + **memory section** → Claude), `DiagnoseRuntime`
  (continuous-monitoring failures), `saveIncident`. `SetMemoryService` injects memory.
- **HTTP:** `HandleDiagnose`, `HandleSubmitFeedback`, `HandleFeedbackSummary`.
- **Depends on:** `aws` (logs), `events`, `memory`, `llm`, `prompts`, `models`.

## `internal/memory` — long-term project memory
- **Purpose:** learn facts about each project and feed them into diagnosis prompts.
- **Files:** `service.go`. **Methods:** `upsert` (dedup via normalized `contentKey`,
  `reference_count++`), `RecordDiagnosisFeedback`, `RecordRecurringFailure`,
  `RecordDeployPattern`, `GetRelevantMemory`, `FormatForPrompt`.
- **Depends on:** `llm`, `models`.

## `internal/events` — operational event hub
- **Purpose:** record structured state transitions; fan out to alerts + auto-diagnosis.
- **Files:** `service.go`. **Interfaces:** `AlertEvaluator`, `DiagnosisEnqueuer`
  (wired post-construction). `Emit` writes the row then notifies the alert engine and
  (for runtime failures) enqueues a diagnosis. `EmitAccount` for pre-project audit
  events. **HTTP:** `HandleGetDeploymentEvents`, `HandleGetProjectEvents`.

## `internal/monitor` — continuous monitoring + alerts
- **Files:** `poller.go`, `logscanner.go`, `alerts.go`, `handlers.go`, `monitor_test.go`.
- **`Poller`** (every 60s, one worker per ready env): ECS `ServiceHealth` + `ALBMetrics`
  → emits `runtime.*` events on degradation/recovery.
- **`LogScanner`** (every 5m): pattern-matches recent CloudWatch logs (`ScanLines`,
  pure Go — no AI) → `runtime.log_anomaly` / crash-loop events.
- **`AlertEngine`:** `EvaluateEvent` maps events → deduplicated `alerts` (AI summary via
  `generateSummary`), respects snoozes, auto-resolves on recovery, broadcasts WS +
  emails owner. **HTTP:** `HandleListAlerts`, `HandleSnooze`, `HandleResolve`.
- **Depends on:** `aws`, `events`, `llm`, `notify`, `ws`, `models`.

## `internal/notify` — email
- `EmailService` over SMTP+STARTTLS. `SendAlert`, `SendDeployResult`. A logging no-op
  when SMTP env is unset (rest of platform never nil-checks).

## `internal/billing` — plans & metering
- `PlanLimits` per tier (`free`/`pro`/`team`), `LimitsFor`. `Service`:
  `CheckProjectLimit`, `IncrementAIAction` (monthly reset; returns `*ErrLimitReached`),
  `GetUsage`. Free = 1 project / 10 AI actions per month.
- **Consumed by:** `conversation` (meter), `deploy` (project limit), `users`.

## `internal/users` — account endpoint
- `HandleGetMe` (plan, usage, project count, notification prefs),
  `HandleUpdateNotifications`. Depends on `billing`, `models`.

## `internal/envvars` — per-environment variables
- CRUD over `env_vars`; secrets redacted in list, plaintext only via
  `HandleReveal`. `LoadForEnvironment` → `[]ecstypes.KeyValuePair` injected into the
  task definition at deploy time.

## `internal/webhooks` — outbound webhooks
- Project-scoped webhook CRUD; `FireEvent` delivers HMAC-SHA256-signed payloads for
  `deploy.started/succeeded/failed`. SSRF guard (`isDisallowedIP`,
  `ALLOW_PRIVATE_WEBHOOKS`). `BuildPayload` standard shape.

## `internal/github` — source integration
- **Files:** `service.go`, `provider.go` (`GitHubProvider` interface), `service_test.go`.
- OAuth (`HandleOAuthCallback`, `HandleGetOAuthURL`), `HandleListRepos`,
  `HandleListBranches`, `HandleDetectFramework` (AI-assisted), PR webhook parsing
  (`PREvent`) for preview environments. Tokens AES-encrypted at rest.

## `internal/terminal` — browser shell into ECS
- **Files:** `service.go`, `datachannel.go`. `HandleTerminal` upgrades a WebSocket,
  authenticates via first-message token + project ownership, opens an SSM exec session
  (`aws.StartExecSession`) and proxies the SSM datachannel ↔ xterm.

## `internal/queue` — async jobs (Asynq/Redis)
- **`Server`** handlers: `handleDeploy`, `handleProvision`, `handleRollback`,
  `handleDeleteProject`, `handleDiagnose`, `handleWatchdog`. **`Client`**: `Enqueue*`
  methods (implements `deploy.Enqueuer`) + Redis pending-mutation store
  (`SetPendingMutation`/`GetPendingMutation`). **`Scheduler`**: periodic watchdog
  (`ReconcileStuckResources`) every 5m. Task constructors `New*Task`.

## `internal/export` — training-data exports (admin)
- `HandleExportIntents`, `HandleExportDiagnoses` → JSONL training rows from
  `conversations` / `diagnosis_feedback`. Behind `ApiKeyAuth(ADMIN_API_KEY)`. Trade-secret datasets.

---

## `pkg/` shared

- **`pkg/models`** — `db.go` (pgxpool, `RunMigrations` = ordered idempotent migration
  strings, `UserOwnsProject`/`UserOwnsAccount` tenancy helpers), `types.go` (all entity
  structs + status/intent/event/alert/memory constants). See `DATABASE_SCHEMA.md`.
- **`pkg/ws`** — `Hub`: per-project client registry, first-message `AuthFunc`,
  `Broadcast(projectID, Message)`, `MessageHandler` seam (conversation engine).
  `Message{Type, Payload}`.
- **`pkg/middleware`** — `auth.go` (`RequireAuth`, `RequireProjectOwnership`,
  `ResolveToken`, user upsert), `ratelimit.go` (per-user token bucket), `cors.go`,
  `apikey.go` (admin), `proprietary.go` (IP/version headers), `requestid.go`
  (X-Request-ID + context).

## Testing
`internal/testutil` provides `db.go` (test DB harness), `fixtures.go`, `mocks.go`
(AWS/GitHub/enqueuer mocks). Unit suites: aws, github, diagnosis, terminal, monitor,
memory, llm, prompts, middleware. Integration (needs Postgres): models, deploy,
webhooks. `e2e/` has real-AWS deploy/failure/rollback/diagnosis suites via
`e2e/cmd/runner`.
