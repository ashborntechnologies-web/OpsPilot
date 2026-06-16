# Architectural Decision Records — OpsPilot

Append-only. Newest at the bottom. Each ADR: date, decision, context, alternatives,
rationale, impact. Dates are approximate where reconstructed from code/git history
rather than a recorded decision.

---

## ADR-001 — BYOC: run the user's app in the user's AWS account
**Date:** 2025-05 (initial design) · **Status:** Accepted

**Decision.** OpsPilot provisions and runs each user's app **in the user's own AWS
account**, assuming a scoped IAM role per request; it never stores long-lived tenant
credentials.

**Context.** Target users own an AWS account and won't hand data/billing/compliance to
a third-party cloud. Trust to connect AWS is the activation gate.

**Alternatives.** (a) Run apps in OpsPilot's cloud (Heroku/Render model) — rejected:
loses the ownership/trust advantage and inherits cost/compliance walls. (b) Store
tenant IAM user keys — rejected: long-lived credentials are a liability and not revocable
cleanly.

**Rationale.** `sts:AssumeRole` with a **per-tenant external ID** gives least-privilege,
revocable access (delete the role) and no credential storage. The bootstrap
CloudFormation template makes setup one click.

**Impact.** Every cloud call goes through `aws.AssumeRoleForEnvironment` → a per-request
`ClientBundle`. Drives `aws_accounts.external_id`, the bootstrap-template endpoint, and
the same-account dev setup. Central trust signal of the product.

---

## ADR-002 — Intent-first AI: Claude classifies, Go executes
**Date:** 2025-05 · **Status:** Accepted (core invariant)

**Decision.** The LLM converts a message into a structured `{intent, params}`; a Go
`switch` (`conversation.ProcessMessage`) routes to a real workflow. The LLM never calls
AWS, mutates the DB, or decides destructive parameters.

**Context.** Conversational UX over real, irreversible infrastructure. Letting a model
directly drive cloud actions is unsafe and unauditable.

**Alternatives.** LLM "agent" with tool-calling straight into AWS SDKs — rejected:
unbounded blast radius, hard to audit, unsafe for destructive ops; tool failures become
model-reasoning failures.

**Rationale.** A narrow classification boundary keeps all side effects in testable Go,
makes every action auditable (operational events), and lets the system degrade to
dashboard buttons when the AI is down. Destructive params (replica count, CPU/memory)
are validated by Go and confirmed, never guessed.

**Impact.** Shapes `conversation`, `prompts`, and the `Handle*`/reusable-method split.
Enables the "AI outage → fallback" UX everywhere.

---

## ADR-003 — Operational events as the AI substrate
**Date:** 2025-Q4 ("Phase 2") · **Status:** Accepted

**Decision.** Every meaningful state transition is written to `operational_events` as a
structured row; diagnosis, alerts, risk, and health scores reason over **events**, not
raw log text.

**Context.** Raw CloudWatch logs are noisy, high-token, and inconsistent across
frameworks. AI needs a clean, structured timeline.

**Alternatives.** Feed raw logs directly to the model for every decision — rejected:
expensive, brittle, and non-reusable across features.

**Rationale.** Structured events are cheap to query, drive multiple consumers (alerts,
diagnosis, scores) from one source, and give the model a clean timeline (logs are still
attached for diagnosis, but events frame them).

**Impact.** `internal/events` became the fan-out hub (→ alert engine, → diagnosis
enqueuer). New workflows must emit events rather than rely on log parsing.

---

## ADR-004 — Shared platform stacks (one VPC/ECS/ALB per account × region)
**Date:** 2025-Q4 · **Status:** Accepted

**Decision.** Provision shared infrastructure (VPC, ECS cluster, ALB, security groups)
**once per AWS account × region** (`platform_stacks`); all project environments in that
account+region reuse it. Original "one CF stack per environment" was superseded.

**Context.** Per-environment VPC+ALB is slow to provision and expensive (idle ALBs cost
money per environment).

**Alternatives.** A stack per environment (original design, see README "one stack per
environment") — rejected at scale for cost/latency. A single global stack — rejected:
must respect account + region boundaries.

**Rationale.** Amortizes the expensive shared resources; per-deploy work becomes
lightweight SDK calls (target group, listener rule, service) on the shared ALB.

**Impact.** `GetOrCreatePlatformStack`, `platform_stacks` table, env `platform_stack_id`;
legacy single-stack env fields retained for pre-migration environments. Platform stack
is intentionally **not** deleted on project teardown.

---

## ADR-005 — Asynq (Redis) for async work; WebSocket for progress
**Date:** 2025-05 · **Status:** Accepted

**Decision.** Long operations (deploy, provision, rollback, delete, diagnose) are Asynq
jobs; HTTP handlers enqueue and return immediately; progress streams back over the
WebSocket hub.

**Context.** Deploys take minutes (build + rollout). HTTP request lifetimes can't span
them, and users want live feedback.

**Alternatives.** Synchronous long-poll/SSE — rejected: ties up request goroutines, no
durability/retries. A heavier broker (Kafka/SQS) — rejected: Redis already present, Asynq
is sufficient.

**Rationale.** Durable retries + scheduling (watchdog) with minimal ops; the WS hub
already exists for chat, so progress reuses it.

**Impact.** `internal/queue` (server/client/scheduler), `deploy.Enqueuer` seam,
Redis-backed pending-mutation store; the `Handle*` (enqueue) vs `Run*Workflow` (worker)
split throughout `deploy`.

---

## ADR-006 — Trade-secret prompts loaded externally; proprietary licensing
**Date:** 2026-Q2 ("proprietary platform" commit) · **Status:** Accepted

**Decision.** The intent-classifier and diagnosis prompts are **never** in source —
loaded from env/file at startup (`prompts.MustLoad`, server refuses to boot without
them). The platform is licensed BUSL-1.1, stamps proprietary headers, and exposes
admin-only training-data exports.

**Context.** The AI prompts + the labeled diagnosis/intent datasets are the durable
competitive moat; a deployment tool's infra code is comparatively commoditized.

**Alternatives.** Embed prompts in the binary — rejected: trivially extractable, leaks
the moat. Fully open-source — rejected: gives away the differentiator.

**Rationale.** Keeping prompts external + datasets gated protects IP while keeping the
code shareable for operation.

**Impact.** `internal/prompts`, git-ignored `prompts/*.txt`, `Proprietary()` middleware,
`/api/v1/meta` IP notice, `internal/export` behind `ADMIN_API_KEY`,
`diagnosis_feedback` as the gold dataset.

---

## ADR-007 — External (hosted) LLM initially, not a self-hosted model
**Date:** 2025-05 · **Status:** Accepted (revisit as datasets mature)

**Decision.** Use the hosted Anthropic Claude API for classification/diagnosis/
summaries rather than a self-hosted/fine-tuned model.

**Context.** Early-stage product; classification and diagnosis quality matter more than
inference cost, and there is no training data yet.

**Alternatives.** Self-host an open model — rejected early: ops burden + lower quality
without training data.

**Rationale.** Fastest path to high-quality reasoning; defer model ownership until the
`diagnosis_feedback`/intent datasets justify fine-tuning. Every call is isolated behind
`internal/llm` with graceful degradation, so the provider can change later.

**Impact.** Single `llm.Client` swap point; the export pipeline exists precisely to make
an eventual model-improvement/fine-tune step possible.

---

## ADR-009 — Team workspaces (organizations) + RBAC as the tenant boundary
**Date:** 2026-06-11 · **Status:** Accepted (supersedes ADR-008)

**Decision.** The tenant boundary is the **organization**, not the user. All tenant data
(`projects`, `aws_accounts`, `alerts`, `incidents`) carries `org_id`; users access it via
`organization_members` with a role (**admin > engineer > viewer**). Enforcement lives in
middleware (`LoadProjectMembership` + `RequireRole`, `RequireOrgMembership`). Each user
gets a personal org on first login; existing data was migrated into one. The "active
workspace" for non-project routes is selected by an **`X-Org-Id` header**.

**Context.** OpsPilot was single-user (user_id ownership, ADR-008). Teams need to share a
workspace with least-privilege roles — the top adoption blocker beyond a solo developer.

**Alternatives.**
- *Per-user sharing / ACLs per project* — rejected: doesn't model a team or AWS-account
  sharing; explodes into per-resource grants.
- *Org as a label without RBAC* — rejected: the requirement is graded permissions
  (viewer read-only, engineer operate, admin manage), which needs roles.
- *Role enforcement in handlers* — rejected: easy to forget; enforced at the middleware
  layer instead so it's structural (mirrors ADR-008's rationale).
- *Active org in a user column vs. header* — chose a client `X-Org-Id` header so the
  navbar switcher is stateless and a user can act in different orgs without a write.

**Rationale.** An org with hierarchical roles is the standard, auditable team model.
Centralizing the check in `LoadProjectMembership`/`RequireRole` keeps isolation
structural and the role→route mapping declarative in `main.go`. Personal-org backfill
makes the migration invisible to existing users.

**Impact.** Replaced `UserOwnsProject`/`UserOwnsAccount`/`RequireProjectOwnership` with
`ProjectOrgRole`/`UserOrgRole`/`LoadProjectMembership`+`RequireRole`. New `internal/orgs`
package, invites + email, frontend workspace switcher/settings/invite pages, role-aware
UI. The WS layer authenticates on membership; viewer action-intents are blocked inside
`conversation.ProcessMessage`. **Open seam:** billing limits/metering still key off
`user_id`, not `org_id`.

---

## ADR-010 — Infrastructure discovery: scan-on-connect + daily refresh
**Date:** 2026-06-12 · **Status:** Accepted

**Decision.** Discover existing AWS infrastructure by **scanning a connected account
immediately on connect**, on demand via a button, and on a **24-hour scheduled refresh**.
Results are upserted into `discovered_resources` (idempotent), scanners run in parallel
and independently, and discovered ECS services assigned to a project join the monitor.

**Context.** Users have existing AWS workloads. Requiring migration into OpsPilot-created
environments is a hard adoption blocker; they need to *see and operate* what they already
run. Discovery must be cheap, safe (read-only), and never block the connect flow.

**Alternatives.**
- *Continuous/streaming discovery (CloudTrail/EventBridge)* — rejected for v1: heavy to
  set up in the customer account, more IAM surface; a periodic pull is far simpler and
  good enough for an inventory that changes slowly.
- *Scan only on demand* — rejected: users expect to see resources right after connecting;
  scan-on-connect powers that first-run "aha".
- *Scan all ~30 AWS regions every time* — rejected: slow and wasteful. We scan the regions
  the account is actually used in (envs + platform stacks), defaulting to us-east-1.
- *One giant scheduled job* — rejected: the daily `scan_all` fans out to one async job
  per account so a slow/failing account doesn't block the rest.

**Rationale.** Scan-on-connect maximizes first-run value; the daily refresh keeps the
inventory current without user effort; idempotent upserts (keyed by
`org_id, resource_type, resource_id`) make re-scans safe and preserve the user's
`project_id` assignments. Parallel, isolated scanners mean a single missing IAM
permission degrades to "that resource type is missing", not a failed scan.

**Impact.** New `internal/discovery` package (clients + 7 scanners + handlers); AWS
service gains `AssumeRoleConfigForAccount`/`AssumeRoleForAccountAndRegion`/`AccountRegions`/
`onAccountConnected`; queue gains `TaskScan`/`TaskScanAll` + daily schedule; monitor
poller/log-scanner include assigned discovered ECS services; frontend inventory +
per-project Infrastructure tab + account scan controls. **Dependency:** discovery needs
read-only Describe/List IAM permissions the bootstrap role may not yet grant (tracked in
CURRENT_STATE).

---

## ADR-011 — Slack via raw HTTP (no SDK)
**Date:** 2026-06-13 · **Status:** Accepted

**Decision.** Integrate Slack by calling the Slack Web API directly over HTTP
(`chat.postMessage`, `oauth.v2.access`, `conversations.list`) with Bearer-token auth,
rather than adding the official Slack Go SDK.

**Context.** OpsPilot needs a small, well-defined slice of Slack: post messages, run
OAuth v2, list channels, and verify slash-command/interactivity signatures. The codebase
already favors thin, dependency-light HTTP clients (see `internal/llm`, the AWS calls).

**Alternatives.**
- *`slack-go/slack` SDK* — rejected: a large dependency (events API, sockets, RTM, dozens
  of methods) for the ~4 endpoints we use; more surface to vet and keep updated, and it
  pulls its own transitive deps. The proprietary-platform posture favors minimal deps.
- *Incoming webhooks only* — rejected: webhooks can't list channels, run slash commands,
  or post to multiple configurable channels with one bot token.

**Rationale.** Raw HTTP keeps the dependency footprint flat, matches the existing `llm`
client pattern, and the surface we need is tiny and stable. Signature verification
(HMAC-SHA256) and OAuth are a few lines each. The base `SendMessage` + Block Kit helpers
are reused by every notification type.

**Impact.** New `internal/slack` package (service/oauth/commands) with no third-party
Slack dependency; `pkg/crypto` extracted for the encrypted bot token; interface seams
(`deploy.SlackNotifier`, `monitor.SlackAlertNotifier`, `slack.Deployer`) avoid import
cycles. **Limitations** (CURRENT_STATE): slash commands aren't mapped to a platform
user/role; the alert "Acknowledge" button is a war-room link (no incident exists at alert
time). Added `/slack/interactivity` (beyond the spec) for the Approve-button deploy flow.

---

## ADR-012 — Explainability: confidence + a structured evidence array
**Date:** 2026-06-15 · **Status:** Accepted

**Decision.** Every AI decision is explainable. Diagnoses store a `confidence_score`
(0.0–1.0) and an `evidence` JSONB **array** of items
`{type, description, data, weight}` (type ∈ log_pattern | metric_spike |
deploy_correlation | memory_match | similar_incident), produced by a *second*, structured
Claude call after the prose diagnosis. Alerts store a deterministic `evidence_text`. Risk
scores already carry per-factor reasons (+ a `top_factor`).

**Context.** Engineers won't trust — or act on — AI output they can't interrogate. The
diagnosis prose alone doesn't expose *which signals* drove the conclusion or how sure the
model is.

**Alternatives.**
- *Parse evidence out of the single diagnosis response* — rejected: brittle, and mixes
  free-form prose with structure. A dedicated structured call yields clean JSON.
- *Free-form "reasoning" string* — rejected: not renderable as discrete, weighted signals;
  an array of typed items powers per-signal UI cards and future scoring/aggregation.
- *LLM-generated alert evidence* — rejected for alerts: the triggering data (error rate,
  task counts, matched pattern) is already in the event payload, so a deterministic
  sentence is cheaper, instant, and always accurate (no extra token cost on the hot path).

**Rationale.** A typed, weighted evidence array is a stable contract the UI renders
uniformly (`components/ai/explainability.tsx`) and that can later feed confidence
calibration or "similar incident" retrieval. The second diagnosis call is bounded
(500 tokens) and fully degradable — failure saves `confidence=null, evidence=[]`.

**Impact.** `incidents.confidence_score`/`evidence`, `alerts.evidence_text`,
`models.EvidenceItem`, `RiskScore.TopFactor`; a second diagnosis LLM call; the
diagnose() return + `incidents.CreateParams` thread confidence/evidence through; shared
frontend `ConfidenceBadge`/`EvidenceSection`. Adds one LLM call per diagnosis (the extra
latency/cost is acceptable for a low-frequency, high-stakes action).

---

## ADR-013 — Environment trust levels + AI-action approval
**Date:** 2026-06-15 · **Status:** Accepted

**Decision.** Every AI-initiated action is gated by the target **environment's** trust
level — `suggest` / `supervised` / `autonomous` — with `autonomous` bounded by a
per-environment `autonomous_boundaries` object. Actions are recorded in `ai_actions`;
within-boundary autonomous actions auto-execute, everything else awaits human approval
(engineer/admin). Direct human actions (the dashboard deploy button) are unaffected.

**Context.** The product vision is an autonomous AI DevOps engineer earned via a
staging-first trust model. Teams need graduated control: watch the AI, then let it act on
low-risk operations, without ever surprising them on production.

**Why three levels.** Two (manual/auto) is too coarse — there's no "I want to see it
prominently and act fast, but still decide" middle ground, which is where teams actually
build trust. Four+ adds config burden without a clear behavioral difference. Three maps to
the trust journey: observe (suggest) → supervise (supervised) → delegate (autonomous).

**Why boundaries are per-environment, not per-action-type globally.** Trust is contextual
to *where* an action runs: a team may grant autonomous rollback/scale on staging while
keeping production in suggest. A global per-action policy can't express "auto-scale staging
but not prod." Per-environment boundaries make the blast radius explicit and let staging
genuinely be the proving ground. There is deliberately **no `can_deploy`** — deploys are
high-impact and always require approval even in autonomous mode.

**Alternatives.** Per-action-type global trust (rejected: can't vary by environment);
approval on the deploy service itself (rejected: would also gate direct human actions —
the spec keeps those direct); reusing `incident_actions` (rejected: too narrow — actions
exist outside incidents and need org/env scope, approval lifecycle, and results).

**Impact.** New `internal/trust` package + `ai_actions` table + env trust columns;
conversation routes chat actions and diagnosis proposes a rollback through `ProposeAction`;
new approval UI (pending panel, navbar badge, Actions tab, env settings, `action_proposed`
banner). Slack proposals link to in-app approval (Slack identity isn't mapped to platform
users). `ai_actions` coexists with the advisory `incident_actions`.

---

## ADR-014 — Postmortems generated asynchronously, in their own package
**Date:** 2026-06-15 · **Status:** Accepted

**Decision.** Resolving an incident does **not** generate its postmortem inline. The
resolve handler enqueues a `postmortem:generate` Asynq job and returns immediately with
`{status:"resolved", postmortem_generating:true}`. A new `internal/postmortem` package owns
generation (Claude call), persistence (the `postmortems` table, draft → published), editing,
and export; the war room polls `GET /incidents/:id/postmortem` (404 `{generating:true}`
until ready).

**Context.** Generation is a full Claude completion over the whole incident (timeline +
diagnosis + AI actions + deployment + project memory) — multiple seconds, and it can fail
or rate-limit. Previously `incidents` generated it synchronously inside resolve and returned
the markdown in the response.

**Why async.** Resolving an incident is a high-stakes, time-pressured click; it must feel
instant and must never fail because the LLM is slow or down. Blocking the HTTP response on a
multi-second Claude call couples incident closure to model availability — exactly the
"degrade, don't break" violation the core principles warn against. Enqueuing decouples them:
resolve always succeeds, the postmortem arrives shortly after, and a transient LLM failure
retries (`MaxRetry(1)`) instead of surfacing as a resolve error. This also matches the
established pattern — every other multi-second AI/infra job (deploy, diagnosis, summary)
already goes through Asynq with progress surfaced afterward.

**Why a separate package, not a method on `incidents`.** A postmortem is now a first-class,
editable, publishable, exportable artifact with its own table, lifecycle (draft/published),
org-library queries, and export formats — substantially more than incidents' timeline logic.
Keeping it in `incidents` would also create an import cycle (it needs `memory`, and the queue
worker needs to call it). `incidents` keeps only a `PostmortemEnqueuer` interface (satisfied
by `*queue.Client`) injected via `SetPostmortemEnqueuer`.

**Idempotency.** The worker upserts `ON CONFLICT(incident_id)`, so a retry (or a
double-resolve) overwrites the same draft rather than duplicating. `incidents.postmortem` is
kept as a backward-compat mirror of `content_markdown`.

**Alternatives.** Synchronous generation (rejected: couples resolve to LLM latency/uptime);
a method on `incidents` returning over WebSocket (rejected: still needs the table + editor +
export + library, and the import cycle remains); generating lazily on first view (rejected:
loses the "fix → memory" learning signal and makes the artifact's existence ambiguous).

**Impact.** New `internal/postmortem` package + `postmortems` table + `postmortem:generate`
task/handler + `EnqueueGeneratePostmortem`. `HandleResolve` no longer returns markdown;
`HandleSavePostmortem` removed. New editor (`/postmortems/[id]/edit`) and SOC2-framed library
(`/orgs/postmortems`) pages + navbar link. Generation writes a `successful_fix` to
`project_memory`. PDF export is print-ready HTML (no native PDF dependency).

---

## ADR-008 — Tenant isolation via one ownership middleware
**Date:** 2025-Q4 · **Status:** Superseded by ADR-009 (RBAC/org membership replaces
single-user ownership; the "one middleware guard" principle carries forward)

**Decision.** All `/projects/:id/...` routes sit behind `RequireProjectOwnership`;
individual handlers do **not** re-check ownership and must never be mounted outside the
group. WebSocket subscriptions re-verify ownership in the auth function.

**Context.** Many project-scoped handlers; per-handler ownership checks are easy to
forget → cross-tenant data exposure by UUID guessing.

**Alternatives.** Per-handler checks — rejected: error-prone, repetitive.

**Rationale.** A single guard makes isolation a structural property, not a per-handler
discipline.

**Impact.** `pkg/middleware/auth.go` (`RequireProjectOwnership`, `UserOwnsProject`); new
project-scoped handlers just join the group. (Recorded in project memory:
"new handlers there skip ownership re-checks".)
