# AI Changelog — OpsPilot

Running log of meaningful changes, the files touched, why, and any architectural
assumption that shifted. Newest at the top. **Append an entry after every meaningful
change** and update the affected AI-context docs in the same pass.

Format:
```
## YYYY-MM-DD — <summary>
- What: <change>
- Files: <key paths>
- Why: <reason>
- Assumptions changed: <none | description>
- Docs updated: <list>
```

---

## 2026-06-15 — Environment trust levels & AI-action approval workflow
- **What:** AI-initiated actions are gated by per-environment trust level; they auto-execute
  within boundaries or require human approval. The "act" step of the AI-DevOps vision.
  - **Data model:** `environments.trust_level` + `autonomous_boundaries` (JSONB); new
    `ai_actions` table (org/project/env/incident scope, proposer, action_type, parameters,
    confidence, rationale, approval lifecycle, result). New `AIAction`/`AutonomousBoundaries`
    models + constants.
  - **New `internal/trust`:** `ProposeAction` (pure `requiresApproval` policy → auto-execute
    or pending), `ExecuteAction` (routes to deploy service; records status/result; posts to
    incident timeline), `ApproveAction`/`RejectAction` (engineer+ via org role). Interfaces
    `Deployer`/`SlackActionNotifier`/`IncidentPoster` avoid cycles. HTTP: env trust GET/PATCH,
    org pending actions, project action history, approve/reject.
  - **Wiring:** conversation routes chat deploy/rollback/scale/change_resources through
    `ProposeAction` (replies "executed" or "needs approval"); diagnosis proposes a rollback
    for deploy-failure incidents with the diagnosis confidence; `slack.PostActionProposal`
    (review link); `incidents.PostActionEntry` for war-room timeline. main wires trustSvc
    into conversation/diagnosis + routes. Direct human deploy button unchanged.
  - **Frontend:** per-env trust settings (Settings tab, admin-only), Pending Approvals panel
    in the right column + navbar badge, project Actions history tab, `action_proposed` amber
    banner; shared `components/trust/*` + `components/ai` ConfidenceBadge reuse; types + API.
- **Files:** `pkg/models/{db,types}.go`; `internal/trust/{service,handlers}.go` (new);
  `internal/{conversation,diagnosis,slack,incidents}/…`, `cmd/api/main.go`; frontend
  `types/api.ts`, `lib/api.ts`, `components/trust/{actions,env-trust-settings}.tsx` (new),
  `components/project/alerts-panel.tsx`, `components/layout/navbar.tsx`,
  `app/projects/[id]/page.tsx`.
- **Why:** Graduated, staging-first autonomy — let the AI act on low-risk ops while keeping
  humans in the loop on anything significant (deploys always need approval).
- **Assumptions changed:** AI-initiated actions are no longer executed inline by the
  conversation/diagnosis services — they flow through the trust layer, which may defer them
  for approval. `ai_actions` is the executable action record (vs advisory `incident_actions`).
- **Verification:** `go build`/`go vet`/`gofmt`/`tsc --noEmit` all clean. Auto-execution +
  approvals not exercised against live services.
- **Docs updated:** ARCHITECTURE, DATABASE_SCHEMA, API_CONTRACTS, CURRENT_STATE, BACKEND,
  DECISIONS (ADR-013).

## 2026-06-15 — Explainability & confidence on every AI decision
- **What:** Every AI output now ships with a "why" — confidence + structured evidence.
  - **Data model:** `incidents.confidence_score` (FLOAT 0–1) + `incidents.evidence` (JSONB
    array of `{type, description, data, weight}`); `alerts.evidence_text`. New
    `models.EvidenceItem`; `RiskScore.TopFactor`.
  - **Diagnosis:** a second structured Claude call (`analyzeConfidenceEvidence`, 500
    tokens) scores confidence (0–100→0–1) and enumerates evidence; threaded through
    `diagnose()`/`DiagnoseProject`/runtime and `incidents.CreateParams`/`CreateIncident`
    (stored on insert + refreshed on re-diagnosis). Fails safe (`confidence=null,
    evidence=[]`). `HandleDiagnose` returns confidence+evidence; chat appends a compact
    "Based on N signals — confidence: X%" line.
  - **Alerts:** `generateSummary` now also returns a deterministic `evidence_text` built
    from the event payload (error rate / task counts / matched log pattern); stored on
    insert and returned by the alerts list.
  - **Risk:** `ScoreFromSignals` sets `TopFactor` (highest-points reason); the full factor
    list already rides in the `deploy_risk` WS payload.
  - **Frontend:** shared `components/ai/explainability.tsx` (`ConfidenceBadge` color-coded
    >75/50–75/<50; `EvidenceSection` collapsible typed cards). Wired into the diagnosis
    dialog, incident war room (left panel), and chat (confidence badge); `evidence_text`
    under alerts in `AlertsPanel`; factor-count tooltip + `top_factor` on the risk banner.
    Diagnose API + Incident/Alert/RiskScore types updated.
- **Files:** `pkg/models/{db,types}.go`; `internal/diagnosis/service.go`,
  `internal/incidents/{service,handlers}.go`, `internal/monitor/{alerts,handlers}.go`,
  `internal/deploy/riskscore.go`; frontend `types/api.ts`, `lib/api.ts`,
  `components/ai/explainability.tsx` (new), `components/project/alerts-panel.tsx`,
  `app/projects/[id]/page.tsx`, `app/projects/[id]/chat/page.tsx`,
  `app/incidents/[id]/page.tsx`.
- **Why:** Engineers must be able to interrogate and trust AI conclusions — the "show your
  work" requirement for an AI DevOps engineer.
- **Assumptions changed:** diagnosis now costs a second (bounded) LLM call; AI outputs are
  contracts with confidence + typed evidence, not opaque prose.
- **Verification:** `go build`/`go vet`/`gofmt`/`tsc --noEmit` all clean. The evidence
  LLM call not exercised against live Claude.
- **Docs updated:** ARCHITECTURE, DATABASE_SCHEMA, CURRENT_STATE, DECISIONS (ADR-012).

## 2026-06-15 — Daily operational summary (AI morning briefing)
- **What:** A dedicated, richer daily summary subsystem (`internal/summary`) — supersedes
  the simple Slack-only digest stubbed during the Slack feature.
  - **Data model:** `daily_summaries` table (one per org/day, `content_markdown` +
    `content_json`, delivery flags) + per-org schedule columns on `organizations`
    (`summary_time`, `summary_timezone`, `summary_enabled`). Removed `models.DailySummary`
    (the rich `DailySummary` now lives in `internal/summary`); added `DailySummaryRecord`.
  - **internal/summary:** `GenerateDailySummary` (24h deploys/incidents+MTTR/alerts, 7d
    top recurring failures from memory, cost-change best-effort/stubbed) → Claude paragraph
    + ≤3 grounded recommendations (strict JSON, template fallback) → markdown → idempotent
    upsert; `GenerateAndDeliver`/`DeliverSummary` (Slack + email to admins/engineers);
    `EnqueueDueSummaries` (timezone-aware hourly fan-out). Handlers: list/latest/generate/
    config.
  - **Reconciliation:** `slack.PostDailySummary` now posts AI markdown (was a counts
    digest); removed `slack.PostDailySummaries`/`buildDailySummary` and the daily
    `TaskSlackSummary`. New queue tasks `TaskSummaryTick` (hourly cron) + `TaskGenerateSummary`
    (+ `EnqueueGenerateSummary`); queue `Server` now holds the summary service.
  - **Extracted earlier but reused:** `notify.SendDailySummary` email; `/orgs/me` now
    returns the summary config.
  - **Frontend:** "Daily summary" card on the projects dashboard (recent briefing +
    recommendations + history link), org-settings Daily Summary section (enable/time/
    timezone + "Send test summary now"), and `/orgs/[orgId]/summaries` history page; a tiny
    markdown renderer reused from the war room; summary types + API.
- **Files:** `pkg/models/{db,types}.go`; `internal/summary/{service,handlers}.go` (new);
  `internal/slack/service.go`, `internal/notify/email.go`, `internal/orgs/service.go`,
  `internal/queue/server.go`, `cmd/api/main.go`; frontend `types/api.ts`, `lib/api.ts`,
  `components/summary/yesterday-card.tsx` (new), `app/orgs/[orgId]/summaries/page.tsx`
  (new), `app/projects/page.tsx`, `app/settings/organization/page.tsx`.
- **Why:** A morning briefing turns OpsPilot's accumulated signals (deploys, incidents,
  alerts, memory) into a proactive, data-grounded digest — the "advise" step of the
  AI-DevOps-engineer vision.
- **Assumptions changed:** daily summaries are now a first-class, scheduled, per-org
  artifact (stored + history), not a transient Slack post; delivery is multi-channel.
- **Verification:** `go build`/`go vet`/`gofmt`/`tsc --noEmit` all clean. Claude generation
  + scheduled delivery not exercised against live services. Cost-change is stubbed.
- **Docs updated:** ARCHITECTURE, DATABASE_SCHEMA, API_CONTRACTS, CURRENT_STATE, BACKEND.

## 2026-06-13 — Slack integration (notifications, daily summary, slash commands)
- **What:** Per-org Slack integration for alert/deploy notifications, a daily digest, and
  `/opspilot` slash commands.
  - **Data model:** new `slack_integrations` table (one per org; `team_id`, encrypted
    `bot_token`, per-purpose channel routing) + `SlackIntegration`/`DailySummary` models.
  - **New `pkg/crypto`:** shared AES-256-GCM `Encrypt`/`Decrypt` (the scheme previously
    inlined in `internal/github`); used for the Slack bot token.
  - **New `internal/slack`:** raw-HTTP Slack Web API client — `SendMessage` base +
    `PostAlert`/`PostDeployResult`/`PostDailySummary(+ies)`; OAuth v2 (signed-state
    install URL + callback storing the encrypted token, channel list, get/update/
    disconnect); `/opspilot` slash commands (`status`/`incidents`/`deploy`/`rollback`/
    `help`) with `X-Slack-Signature` verification + `/slack/interactivity` for the
    deploy/rollback Approve buttons.
  - **Wiring:** alerts (`monitor.notifyOwner` → `SlackAlertNotifier`) and deploy results
    (`deploy` → `SlackNotifier`) post to Slack best-effort; daily summary via a new
    scheduler task (`TaskSlackSummary`, 14:00 UTC); `slack.Deployer` injected for slash
    deploy/rollback. `validateEnv` warns when Slack env is unset; `.env.example` updated.
  - **Frontend:** `/settings/integrations` (connect via OAuth, channel dropdowns from
    `conversations.list`, disconnect) + settings sub-nav linking Members ↔ Integrations;
    Slack types + API client.
- **Files:** `pkg/crypto/crypto.go` (new); `pkg/models/{db,types}.go`;
  `internal/slack/{service,oauth,commands}.go` (new); `internal/deploy/service.go`,
  `internal/monitor/alerts.go`, `internal/queue/server.go`, `cmd/api/main.go`,
  `.env.example`; frontend `types/api.ts`, `lib/api.ts`,
  `app/settings/integrations/page.tsx` (new), `app/settings/organization/page.tsx`.
- **Why:** Teams live in Slack — surfacing alerts/deploys and allowing deploy/rollback
  from chat meets them where they are and shortens incident response.
- **Assumptions changed:** notifications are now multi-channel (email + Slack);
  secret-at-rest crypto is shared (`pkg/crypto`); deploys can be triggered from Slack
  (via a confirmation), expanding the action surface beyond the web app.
- **Verification:** `go build ./...`, `go vet ./...`, `gofmt -l`, `tsc --noEmit` all
  clean. Slack API calls + OAuth not exercised against a live workspace here.
- **Docs updated:** ARCHITECTURE (Slack flow), DATABASE_SCHEMA, API_CONTRACTS,
  CURRENT_STATE, BACKEND, DECISIONS (ADR-011).

## 2026-06-12 — Incident war room (shared real-time investigation + AI postmortem)
- **What:** Turned incidents into first-class, lifecycle-tracked objects with a shared
  real-time war room where the team investigates alongside the AI.
  - **Data model:** extended `incidents` (title, status open/investigating/resolved,
    severity, acknowledged_by/at, resolved_by/at, postmortem, org_id) + new
    `incident_timeline` and `incident_actions` tables. New structs/constants in models.
  - **New `internal/incidents`:** `CreateIncident` (deduplicated per deployment / per env;
    posts the AI diagnosis as the first timeline entry; surfaces the suggested fix as a
    pending action), `GeneratePostmortem` (Claude → markdown with fixed sections, template
    fallback), and handlers (list org/project, get full, post timeline, acknowledge,
    resolve, save/publish postmortem, approve/reject actions) with per-incident org+role
    checks.
  - **WS hub:** generalized the client room key from project ID to any room;
    `HandleIncidentUpgrade` + `RoomAuthFunc` add a broadcast-only war-room socket keyed by
    incident ID. Timeline/status/action changes broadcast live.
  - **Wiring:** diagnosis now opens incidents via the `IncidentCreator` interface
    (`SetIncidentService`) for both deploy-failure and runtime paths; alert emails link to
    `/incidents` (war room) instead of the project page.
  - **Frontend:** `/incidents` list (open-first, severity-sorted), `/incidents/[id]` war
    room (left metadata panel, center live timeline with AI/human styling + auto-scroll +
    update composer, right AI-actions approve/reject panel, top-bar acknowledge/resolve +
    live time-open), postmortem modal (editable, publish), navbar open-incident red badge,
    a dependency-free `lib/markdown.tsx` renderer, incident types + API + WS client.
- **Files:** `pkg/models/{db,types}.go`; `pkg/ws/hub.go`;
  `internal/incidents/{service,handlers}.go` (new); `internal/diagnosis/service.go`;
  `internal/monitor/alerts.go`; `cmd/api/main.go`; frontend `types/api.ts`,
  `lib/{api.ts,markdown.tsx,incidents.ts}`, `components/layout/navbar.tsx`,
  `app/incidents/page.tsx` + `app/incidents/[id]/page.tsx` (new).
- **Why:** Incidents need a collaborative, real-time response surface (not just a row +
  a project-page toast) — the war room is where humans and the AI converge on a fix and
  capture the postmortem.
- **Assumptions changed:** Incidents are now lifecycle objects with shared state and a
  per-incident realtime channel; the WS hub is room-generic, not project-only; diagnosis
  output flows into an incident timeline rather than a bare incidents row.
- **Verification:** `go build ./...`, `go vet ./...`, `gofmt -l`, `tsc --noEmit` all clean.
  LLM postmortem + live WS not exercised against running services here.
- **Docs updated:** ARCHITECTURE (war-room flow), DATABASE_SCHEMA, API_CONTRACTS (+WS),
  CURRENT_STATE, BACKEND.

## 2026-06-12 — AWS infrastructure discovery (onboard existing resources)
- **What:** Scan connected AWS accounts for existing infrastructure so users onboard
  without migration.
  - **Data model:** new `discovered_resources` table (org/account scoped, JSONB
    metadata+tags, nullable `project_id`, `is_managed`, unique
    `org_id+resource_type+resource_id`); `aws_accounts.last_scanned_at`.
  - **New `internal/discovery` package:** `ScanClients` (ECS/ELB/RDS/ElastiCache/Lambda/
    S3/SQS), `ScanAccountByID`→`ScanAccount` running 7 parallel, isolated scanners
    (`ScanECSServices` incl. clusters + task-def log group, `ScanRDSInstances`,
    `ScanElastiCache`, `ScanLambda`, `ScanS3`, `ScanALBs`, `ScanSQS`), idempotent upsert,
    and HTTP handlers (scan, list org/project resources, assign).
  - **AWS service:** `AssumeRoleConfigForAccount`, `AssumeRoleForAccountAndRegion`,
    `AccountRegions`, `MarkAccountScanned`, `SetOnAccountConnected`; account-list now
    returns `last_scanned_at` + `resource_count`.
  - **Queue/scheduler:** `TaskScan`/`TaskScanAll` + handlers, `EnqueueScan`, daily
    `@every 24h` scan-all fan-out; `NewServer` takes the discovery service.
  - **Triggers:** scan-on-connect (`onAccountConnected`→`EnqueueScan`), on-demand
    endpoint, daily refresh.
  - **Monitor:** poller + log scanner now include discovered ECS services assigned to a
    project (assume role per account+region; health-only, no ALB metrics).
  - **Frontend:** `/orgs/resources` inventory (type/region filters + assign), per-project
    Infrastructure tab, AWS-accounts "Scan now" + last-scan + resource count; shared
    `lib/resources.tsx`; new discovery types + API functions.
  - **Deps:** added AWS SDK v2 modules rds, elasticache, lambda, s3, sqs.
- **Files:** `pkg/models/{db,types}.go`; `internal/discovery/{clients,service,handlers}.go`
  (new); `internal/aws/service.go`; `internal/queue/server.go`;
  `internal/monitor/{poller,logscanner}.go`; `cmd/api/main.go`; frontend
  `lib/{api.ts,resources.tsx}`, `types/api.ts`, `app/orgs/resources/page.tsx` (new),
  `app/aws-accounts/page.tsx`, `app/projects/[id]/page.tsx`; `go.mod`/`go.sum`.
- **Why:** Removing the "migrate everything first" barrier is the top onboarding blocker
  for teams with existing AWS workloads (see PRODUCT_VISION onboarding goal).
- **Assumptions changed:** OpsPilot now reasons about resources it did **not** create;
  monitoring is no longer limited to OpsPilot-provisioned environments. Tenant scope for
  resources is the org. Scan is read-only and best-effort (per-scanner isolation).
- **Verification:** `go build ./...`, `go vet ./...`, `gofmt -l`, and `tsc --noEmit` all
  clean. AWS scanners not exercised against a live account here.
- **Docs updated:** ARCHITECTURE (discovery flow), DATABASE_SCHEMA, API_CONTRACTS,
  CURRENT_STATE, DECISIONS (ADR-010), BACKEND.

## 2026-06-11 — Team workspaces (organizations) + role-based access control
- **What:** Introduced multi-tenancy. Tenant ownership moved from per-user to
  **organizations** with roles **admin > engineer > viewer**.
  - **Data model:** new `organizations`, `organization_members`,
    `organization_invites` tables; `org_id` added to `projects`, `aws_accounts`,
    `alerts`, `incidents`; `backfillPersonalOrgs` migrates every existing user into a
    personal org (admin) and assigns their data. New users get a personal org in the
    auth middleware (`ensurePersonalOrg`).
  - **Middleware (security-critical):** replaced `RequireProjectOwnership`/
    `UserOwnsProject` with `LoadProjectMembership` (resolves project→org→role, 404 for
    non-members) + `RequireRole(min...)` hierarchy checker; added `RequireOrgMembership`
    for `/orgs/:orgId` and `ActiveOrg` (X-Org-Id header → active workspace). New DB
    helpers `ProjectOrgRole`/`UserOrgRole` (+ `ErrNoMembership`).
  - **Backend:** new `internal/orgs` service (create org, list mine, members, invite,
    accept, role change, remove — last-admin protected); `notify.SendOrgInvite`;
    project/AWS handlers org-scoped; alerts/incidents set `org_id` on insert;
    `conversation.ProcessMessage` blocks viewer action intents; routes in `main.go`
    gated per role.
  - **Frontend:** `X-Org-Id` header in the API client; `useActiveOrg` hook; navbar
    workspace switcher; `/settings/organization` (members, invites, role mgmt);
    `/invites/[token]` accept page; role-aware dashboard (view-only banner + guarded
    handlers).
- **Files:** `pkg/models/{db,types}.go`, `pkg/middleware/auth.go`,
  `internal/orgs/service.go` (new), `internal/notify/email.go`,
  `internal/{deploy,aws,conversation,monitor,diagnosis,terminal}/…`, `cmd/api/main.go`,
  `internal/testutil/fixtures.go`, `pkg/models/migration_test.go`,
  `internal/{deploy,webhooks}/*_test.go`; frontend `lib/{api.ts,use-org.ts}`,
  `types/api.ts`, `components/layout/navbar.tsx`, `app/settings/organization/page.tsx`
  (new), `app/invites/[token]/page.tsx` (new), `app/projects/[id]/page.tsx`.
- **Why:** Enable teams to collaborate on a workspace with least-privilege access —
  the top gap before OpsPilot is usable beyond a solo developer.
- **Assumptions changed:** Tenant isolation is now **org membership + role**, not
  `user_id` ownership (ADR-009 supersedes ADR-008). The "active workspace" is a
  client-selected `X-Org-Id` header. Billing remains per-user (noted as a seam).
- **Verification:** `go build ./...` clean; `go vet ./...` clean (tests compile);
  `tsc --noEmit` clean. DB-backed tests require `TEST_DATABASE_URL` (not run here).
- **Docs updated:** ARCHITECTURE (auth flow), DATABASE_SCHEMA (3 tables + org_id),
  API_CONTRACTS (org endpoints + roles + X-Org-Id), CURRENT_STATE, DECISIONS (ADR-009).

## 2026-06-11 — Establish `docs/ai-context/` living knowledge base
- **What:** Created the AI-context knowledge base (12 documents) describing the system
  as implemented: CLAUDE, PRODUCT_VISION, ARCHITECTURE (with Mermaid diagrams),
  BACKEND, FRONTEND, DATABASE_SCHEMA, API_CONTRACTS, INFRASTRUCTURE, CURRENT_STATE,
  ROADMAP, DECISIONS, CHANGELOG_AI.
- **Files:** `docs/ai-context/*.md` (new).
- **Why:** Let future Claude Code sessions understand the system without re-reading the
  whole repo; prevent documentation drift via explicit maintenance rules in CLAUDE.md.
- **Assumptions changed:** None (documentation only). Content derived by reading the
  codebase: `cmd/api/main.go`, `pkg/models/{db,types}.go`, `internal/*` services,
  `frontend/{lib,types,app}`, infra files.
- **Docs updated:** all (initial creation).

## 2026-06-11 — `robots.txt` on the API host
- **What:** Added `GET /robots.txt` (root) returning `Disallow: /`.
- **Files:** `cmd/api/main.go`.
- **Why:** The API host serves only the API + proprietary admin export endpoints;
  nothing there should be crawled/indexed. (Corrected a stale review that placed it in
  the Next.js `public/` dir — wrong host for the `/api/` concern.)
- **Assumptions changed:** None.
- **Docs updated:** API_CONTRACTS (public routes), CURRENT_STATE.

## 2026-06-11 — Commit: continuous monitoring, billing, memory, risk scoring, email
- **What:** Committed the in-flight production-hardening work (`3e1f58c`): `internal/monitor`
  (Poller + LogScanner + AlertEngine), `internal/memory`, `internal/billing`,
  `internal/notify`, `internal/users`, `internal/deploy/riskscore.go`,
  `internal/aws/monitoring.go`, `pkg/middleware/requestid.go`, plus frontend
  status-sidebar/alerts-panel, usage meter, notification settings, build-log + risk
  streaming, responsive layout, full framework labels, HTTPS plumbing, and memory
  injection into diagnosis.
- **Files:** 48 files (see commit `3e1f58c`).
- **Why:** Ship the continuous-operation intelligence loop + platform features.
- **Assumptions changed:** OpsPilot is now a continuous-operation system (monitors infra
  between deploys), not only a deploy tool — see PRODUCT_VISION / ARCHITECTURE monitoring
  flow. Verified against code that the earlier review of these features was largely stale.
- **Docs updated:** captured in CURRENT_STATE, ARCHITECTURE, BACKEND (initial baseline).

---

### Pre-baseline history (from git, for context — not exhaustive)
- `f64bb41` feat: proprietary platform — trade-secret prompts, tagging, feedback,
  exports, legal. *(See ADR-006.)*
- `3a5d140` / `b80435e` feat: production hardening — security, reliability, UX, health
  scores.
- `c838d23` test: E2E framework — real-repo deploy/failure/rollback/diagnosis suites.
