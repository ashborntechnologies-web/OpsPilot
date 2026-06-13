# Architecture — OpsPilot

## System overview

A Go monolith API + Asynq worker (same binary), a Next.js frontend, Postgres, and
Redis. All cloud mutations happen inside the **user's** AWS account via assumed IAM
roles. Claude (Anthropic API) is called only for intent classification, diagnosis,
and short natural-language summaries.

```mermaid
graph TB
  subgraph Client
    FE["Next.js frontend<br/>(Clerk auth, SWR, WebSocket, xterm)"]
  end
  subgraph Platform["OpsPilot platform (single Go binary)"]
    API["HTTP API (Gin)"]
    WS["WebSocket Hub<br/>pkg/ws"]
    QW["Asynq worker<br/>internal/queue.Server"]
    SCH["Scheduler<br/>(watchdog every 5m)"]
    MON["Monitor: Poller (60s) +<br/>LogScanner (5m) + AlertEngine"]
  end
  subgraph Data
    PG[("Postgres")]
    RD[("Redis / Asynq")]
  end
  subgraph External
    CLERK["Clerk (JWT/JWKS)"]
    ANTH["Anthropic API (Claude)"]
    GH["GitHub (OAuth, repos, webhooks)"]
  end
  subgraph UserAWS["User's AWS account (BYOC)"]
    ROLE["Assumed IAM role<br/>(per-tenant external ID)"]
    CF["CloudFormation"] --> PLAT["Platform stack:<br/>VPC, ECS cluster, ALB"]
    CB["CodeBuild"] --> ECR["ECR"]
    ECS["ECS service (Fargate)"]
    CW["CloudWatch Logs"]
  end

  FE -->|"/api/v1 (proxied)"| API
  FE <-->|WebSocket| WS
  API --> PG
  API -->|enqueue| RD
  QW -->|consume| RD
  SCH --> RD
  API & QW & MON -->|AssumeRole| ROLE
  ROLE --- CF & CB & ECS & CW
  API --> CLERK
  API & QW & MON --> ANTH
  API & QW --> GH
  QW --> WS
  MON --> WS
  MON --> PG
```

### Process model
Everything runs in one process started by `cmd/api/main.go`: the Gin HTTP server, the
WebSocket hub, the Asynq queue server (`go queueServer.Start()`), the watchdog
scheduler, the monitoring Poller, and the LogScanner. Horizontal scaling would split
the Asynq worker and monitors out, but today it's a single binary.

---

## Backend architecture

Domain-driven packages under `internal/`, each exposing a `Service`. `cmd/api/main.go`
is the composition root — it constructs every service once and injects dependencies
(including post-construction `Set*` calls to break cycles). See `BACKEND.md` for the
per-module reference.

Layering:
- **Transport:** Gin routes (`main.go`) + the WebSocket hub (`pkg/ws`).
- **Middleware** (`pkg/middleware`): RequestID → Proprietary → CORS globally; then
  `RequireAuth` and `RequireProjectOwnership` on protected groups; `RateLimiter` on
  expensive endpoints; `ApiKeyAuth` on admin exports.
- **Domain services** (`internal/*`): business logic, each with reusable
  context-taking methods + thin `Handle*` HTTP adapters.
- **Async:** `internal/queue` (Asynq) for deploy/provision/rollback/delete/diagnose +
  a Redis-backed pending-mutation store.
- **Persistence:** `pkg/models` (raw pgx, migrations-in-code).
- **Cloud:** `internal/aws` wraps STS/CloudFormation/ECS/ECR/CodeBuild/ELBv2/SSM/
  CloudWatch/Cost Explorer behind a per-request assumed-role `ClientBundle`.

---

## Frontend architecture

Next.js App Router (`frontend/app`). Clerk for auth, SWR for data fetching, raw
`WebSocket` for live streams, shadcn-style UI primitives, `xterm.js` for the terminal.
HTTP calls use relative `/api/v1/*` paths rewritten to the backend (no CORS);
WebSockets connect directly to the backend (`NEXT_PUBLIC_API_URL`). See `FRONTEND.md`.

---

## Data flow: a deploy

```mermaid
sequenceDiagram
  participant U as User
  participant FE as Frontend
  participant API as API (deploy.HandleDeploy)
  participant Q as Asynq/Redis
  participant W as Worker (RunDeployWorkflow)
  participant AWS as User AWS
  participant WS as WS Hub
  participant EV as events + monitor

  U->>FE: Deploy (button or "deploy to prod")
  FE->>API: POST /environments/:id/deploy
  API->>API: risk score (broadcast deploy_risk)
  API->>Q: EnqueueDeploy(...)
  API-->>FE: 200 {message}
  Q->>W: deploy task
  W->>AWS: AssumeRole → StartCodeBuild (build+push ECR)
  W->>WS: build_log / deploy_progress (streamed)
  W->>AWS: RegisterTaskDef (inject env vars) → EnsureTargetGroup/Listener/Service
  W->>AWS: WaitForECSServiceStable
  W->>EV: emit deploy.started/build.*/ecs.*/healthcheck.*
  alt success
    W->>WS: deploy_done
  else failure
    W->>EV: build.failed / healthcheck.failed (→ alert + auto-diagnose)
    W->>WS: deploy_failed
  end
```

`HandleDeploy` enqueues only; `RunDeployWorkflow` (in `internal/deploy/service.go`)
does the work in the worker. Operational events emitted throughout are the substrate
for diagnosis, alerts, health and risk scores.

---

## Authentication flow

```mermaid
sequenceDiagram
  participant FE as Frontend (Clerk)
  participant MW as RequireAuth
  participant CK as Clerk JWKS
  participant DB as Postgres
  FE->>MW: Authorization: Bearer <Clerk JWT> (+ X-Org-Id)
  MW->>CK: fetch/cache JWKS, verify RS256 signature
  MW->>DB: upsert user by clerk_id → user UUID
  MW->>DB: ensure personal org + admin membership (new users)
  MW->>MW: set userID in context
  Note over MW: /projects/:id → LoadProjectMembership<br/>resolves project→org→role (404 if not a member),<br/>then RequireRole(min) enforces admin>engineer>viewer
```

OpsPilot is multi-tenant via **organizations (team workspaces) + RBAC**. All tenant
data (`projects`, `aws_accounts`, `alerts`, `incidents`) belongs to an `org_id`; users
access it through `organization_members` with a role. See `DATABASE_SCHEMA.md` and
ADR-009.

- **Identity:** `RequireAuth` validates the Clerk JWT against cached JWKS and upserts
  the user. On first login it also creates the user's **personal organization** (admin
  membership); existing users were migrated by `backfillPersonalOrgs`.
- **Tenant isolation + roles** (`pkg/middleware/auth.go`):
  - `LoadProjectMembership(db)` on `/projects/:id/...` resolves the project's org and
    the caller's role (`db.ProjectOrgRole`), 404 for non-members, and stores
    `org_id`/`org_role` on the context.
  - `RequireRole(min...)` gates action routes by the hierarchy **admin(3) > engineer(2)
    > viewer(1)** — viewer = reads, engineer = deploy/rollback/scale/env-vars/alerts/
    chat/webhooks, admin = create/delete env, delete project, settings, AWS connect.
  - `RequireOrgMembership(db, roles...)` guards `/orgs/:orgId/...` (member management).
  - **Active org** for non-project routes (`/projects`, `/aws-accounts`) comes from the
    `X-Org-Id` header (`middleware.ActiveOrg`), defaulting to the personal org.
- **WebSocket:** first-message `{type:"auth", token}`; the hub's `AuthFunc` verifies
  **org membership** for the project (any role — viewers receive live broadcasts) then
  replies `auth_ok`. Action intents over chat are blocked for viewers inside
  `conversation.ProcessMessage` (the WS layer only checks membership).
- Admin export endpoints use a static `ADMIN_API_KEY` bearer instead of Clerk.

---

## AI conversation flow

```mermaid
sequenceDiagram
  participant U as User
  participant WS as WS Hub
  participant C as conversation.ProcessMessage
  participant B as billing (meter)
  participant LLM as Claude (intent classifier prompt)
  participant D as deploy / diagnosis (Go)

  U->>WS: { message: "scale to 3" }
  WS->>C: ProcessMessage(projectID,userID,msg)
  C->>B: IncrementAIAction (limit check)
  C->>C: prepend recent conversation context; save user msg
  C->>LLM: Complete(intentPrompt, msg) → JSON {intent, params}
  C->>D: switch(intent) → Go method executes (never the LLM)
  D-->>C: human-readable result
  C->>WS: response (persisted to conversations)
```

**Invariant:** Claude returns a structured intent + params; a Go `switch`
(`ProcessMessage`) routes to a real workflow. The LLM never calls AWS or the DB.
Destructive params are validated by Go and never guessed (e.g. missing replica count
→ ask, don't assume). On classifier failure the chat degrades to a helpful fallback
pointing at dashboard buttons.

---

## LLM integration

- One client: `internal/llm.Client.Complete(ctx, system, userMessage, maxTokens)` →
  Anthropic Messages API with retry/backoff on transient errors.
- Three call sites: **intent classification** (`conversation`), **diagnosis**
  (`diagnosis`, deploy-failure + runtime), and **short summaries** (alert summaries in
  `monitor.AlertEngine`, risk explanation in `deploy.explainRisk`).
- **Prompts are trade secrets** loaded at startup from env/file via `internal/prompts`
  (`MustLoad` panics if absent). The intent classifier and diagnosis prompts are never
  in source. Model defaults to the latest Claude (see `internal/llm`).
- Every LLM call has a degradation path; an AI outage never hard-fails a request.

---

## Memory architecture

```mermaid
graph LR
  DIAG["diagnosis (failure/runtime)"] -->|successful_fix, recurring_failure| MEM[(project_memory)]
  DEPLOY["deploy workflow"] -->|deploy_pattern| MEM
  FB["diagnosis_feedback (helpful+fixed)"] --> MEM
  MEM -->|GetRelevantMemory + FormatForPrompt| DIAGPROMPT["diagnosis prompt context"]
  DIAGPROMPT --> LLM["Claude"]
```

`internal/memory` stores long-lived facts about each project in `project_memory`
(types: `recurring_failure`, `successful_fix`, `deploy_pattern`, …). Near-duplicate
facts are merged by a normalized `contentKey` (incrementing `reference_count` instead
of inserting). `diagnosis.memorySection` calls `GetRelevantMemory` and
`FormatForPrompt`, injecting the top facts into the diagnosis prompt so the model gets
project-specific over time. This is the system's learning loop.

---

## Infrastructure discovery flow

Lets users onboard **existing** AWS infrastructure (not created by OpsPilot) without
migration. See `internal/discovery` and ADR-010.

```mermaid
graph TB
  CONNECT["AWS account connected"] -->|onAccountConnected| ENQ1["queue: EnqueueScan"]
  SCHED["Scheduler @every 24h → scan_all"] -->|per account| ENQ1
  BTN["POST /aws-accounts/:id/scan (engineer+)"] --> ENQ1
  ENQ1 --> WORKER["Asynq handleScan"]
  WORKER --> SAI["discovery.ScanAccountByID"]
  SAI -->|AccountRegions + AssumeRoleConfigForAccount| SCAN["ScanAccount (per region)"]
  SCAN -->|parallel, isolated| SC["ScanECSServices / RDS / ElastiCache /\nLambda / S3 / ALBs / SQS"]
  SC -->|upsert org_id,type,resource_id| DR[(discovered_resources)]
  DR -->|GET /orgs/:orgId/resources| INV["inventory UI"]
  DR -->|PATCH /resources/:id/assign| ASSIGN["assign to project"]
  ASSIGN --> MON["monitor poller + logscanner include\nassigned discovered ECS services"]
```

Scanners run in parallel and are independent — one failing (missing IAM permission,
throttling) does not stop the others. Re-scans are idempotent (upsert keyed by
`org_id, resource_type, resource_id`), refreshing `last_seen_at`/metadata/tags without
disturbing the user's `project_id` assignment. Resources tagged `ManagedBy=OpsPilot`
are flagged `is_managed`. Discovered ECS services **assigned to a project** are pulled
into the monitor's worker set (health poll + log anomaly scan), so pre-existing services
get the same alerting as OpsPilot-created ones.

## Incident war room flow

A shared, real-time space where the team investigates an incident alongside the AI.
See `internal/incidents` and the war-room WebSocket below.

```mermaid
graph TB
  DIAG["diagnosis completes\n(deploy failure / runtime anomaly)"] -->|CreateIncident (dedup)| INC[(incidents)]
  INC -->|first entry = AI diagnosis| TL[(incident_timeline)]
  INC -->|suggested fix| ACT[(incident_actions)]
  subgraph WarRoom["/incidents/:id war room"]
    HUMAN["engineer posts update"] -->|POST /timeline| TL
    ACK["Acknowledge → investigating"] --> INC
    APPROVE["Approve/Reject AI action"] --> ACT
    RES["Mark Resolved"] -->|resolve| INC
    RES -->|Claude| PM["postmortem (markdown)\nSummary/Timeline/Root Cause/\nContributing Factors/Action Items"]
    PM --> INC
  end
  TL & INC & ACT -->|hub.Broadcast(incidentID)| WS["war-room WebSocket\n/ws/incidents/:id"]
  WS --> SUB["all subscribers (live)"]
```

- **Incidents are first-class lifecycle objects:** `open → investigating → resolved`,
  with severity, who acknowledged/resolved, and an AI postmortem. They are created by the
  diagnosis pipeline via `incidents.CreateIncident` (deduplicated per deployment, or per
  environment for runtime anomalies), which posts the AI diagnosis as the **first timeline
  entry** and surfaces the suggested fix as a pending action.
- **War-room WebSocket** (`pkg/ws` incident rooms): the hub's room key generalizes from
  project ID to **any room ID**; `HandleIncidentUpgrade` registers a broadcast-only
  socket keyed by incident ID (auth = org membership of the incident). Every timeline
  entry / status / action change is broadcast to all subscribers. Engineers post updates
  over HTTP (`POST /incidents/:id/timeline`), not the socket.
- **Postmortem:** on resolve, Claude is given the full timeline + root cause + actions +
  duration and returns markdown with fixed sections; it is stored on the incident and
  shown editable before the engineer publishes it. Falls back to a template on AI outage.
- Alert emails now link to the war room (`/incidents`) instead of the project page.

## Monitoring & alerting flow

```mermaid
graph TB
  POLL["Poller (60s): ECS health + ALB metrics"] --> EVT["operational_events<br/>(runtime.*)"]
  SCAN["LogScanner (5m): CloudWatch pattern match"] --> EVT
  DEPLOY["deploy/provision workflows"] --> EVT
  EVT --> EVSVC["events.Service.Emit"]
  EVSVC -->|AlertEvaluator| AE["monitor.AlertEngine.EvaluateEvent"]
  EVSVC -->|DiagnosisEnqueuer| Q["Asynq: auto-diagnose on runtime failure"]
  AE -->|dedup + AI summary| ALERTS[(alerts)]
  AE --> WSB["WS: alert / alert_resolved"]
  AE --> EMAIL["notify: email owner"]
  REC["recovery event"] --> AE
  AE -->|resolve matching open alerts| ALERTS
```

`events.Service` is the hub: every `Emit` fans out to the AlertEngine (events →
deduplicated, AI-summarized alerts, with snooze + auto-resolve on recovery) and the
diagnosis enqueuer (runtime failures auto-trigger a diagnosis job). Alerts reach the
user via WebSocket and email (`internal/notify`, a no-op without SMTP).

---

## Slack integration

Per-org Slack workspace connection (`internal/slack`, ADR-011) for notifications and
slash commands. Talks to the Slack Web API over **raw HTTP** (no SDK).

```mermaid
graph TB
  subgraph Connect
    INSTALL["GET /orgs/:id/slack/install (admin)\n→ signed-state OAuth URL"] --> SLACKOAUTH["Slack OAuth consent"]
    SLACKOAUTH --> CB["GET /slack/callback\n(verify state) → store bot_token (encrypted)"]
    CB --> SI[(slack_integrations)]
  end
  ALERTS["monitor.notifyOwner"] -->|PostAlert| SI
  DEPLOY["deploy result"] -->|PostDeployResult| SI
  DAILY["scheduler 14:00 UTC"] -->|PostDailySummaries| SI
  SI -->|chat.postMessage (Bearer)| SLACK["Slack workspace"]
  SLACK -->|/opspilot| CMD["POST /slack/commands\n(verify X-Slack-Signature)"]
  CMD -->|status/incidents/help| SLACK
  CMD -->|deploy/rollback confirm| BTN["Approve button"]
  BTN --> INT["POST /slack/interactivity\n(verify signature) → deploySvc.TriggerDeploy"]
```

- **Notifications** (best-effort, never block the caller): alerts → `PostAlert`
  (color-coded, links to the war room); deploy results → `PostDeployResult`;
  morning digest → `PostDailySummary` (daily scheduler). All resolve the org's
  connected workspace + channel and no-op if absent.
- **Slash commands** (`/opspilot status|incidents|deploy|rollback|help`): the workspace
  maps to an org via `team_id`; responses are ephemeral except deploy/rollback
  **confirmations** (in-channel, Approve button → `/slack/interactivity` → deploy).
- **Trust:** OAuth uses an HMAC-signed `state` (org+user); commands/interactivity verify
  the `X-Slack-Signature` HMAC. Bot tokens are encrypted at rest (`pkg/crypto`).
- **Injection seams** (no import cycles): `deploy.SlackNotifier`,
  `monitor.SlackAlertNotifier`, `slack.Deployer`.

## External integrations

| Integration | Used for | Where |
|---|---|---|
| **Clerk** | User auth (JWT + JWKS), user profile (email) | `internal/auth`, `pkg/middleware/auth.go` |
| **Slack** | Per-org alert/deploy notifications, daily digest, `/opspilot` slash commands | `internal/slack` |
| **Anthropic (Claude)** | Intent classification, diagnosis, summaries | `internal/llm` |
| **GitHub** | OAuth, repo/branch list, framework detection, PR webhooks (previews) | `internal/github`, `deploy.HandleGithubWebhook` |
| **AWS (STS, CloudFormation, ECS, ECR, CodeBuild, ELBv2, SSM, CloudWatch, Cost Explorer)** | All infra in the user's account | `internal/aws` |
| **SMTP** | Alert + deploy-result emails (optional) | `internal/notify` |
| **Redis / Asynq** | Async jobs, watchdog schedule, pending mutations | `internal/queue` |
| **Postgres** | All persistent state | `pkg/models` |
