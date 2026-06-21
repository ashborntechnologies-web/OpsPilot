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

## ADR-019 — Secret env vars encrypted at the application layer
**Date:** 2026-06-16 · **Status:** Accepted

**Decision.** `is_secret` env-var values are encrypted with AES-256-GCM (`pkg/crypto`, keyed
by `ENCRYPTION_KEY`) before they're written to Postgres, and decrypted only at the two points
that legitimately need plaintext: the explicit per-variable reveal endpoint and deploy-time
task-definition injection. A startup backfill encrypts any pre-existing plaintext secrets.
Non-secret env vars stay plaintext (they're returned in list responses anyway).

**Context.** Secrets were stored plaintext in `env_vars.value`, relying solely on Postgres
at-rest disk encryption. The same `pkg/crypto` primitive already protects GitHub and Slack
tokens, so the building block existed; env-var secrets just hadn't adopted it.

**Why application-layer encryption (vs. relying on DB-at-rest).** Disk encryption only
protects against physical media theft — it does nothing against a leaked DB dump, a
compromised read-replica, a `SELECT` by an over-privileged operator, or a log that captured a
query. Encrypting at the application layer means the value is opaque everywhere except the two
code paths holding the key, shrinking the blast radius of any database-level exposure. It also
makes "secret" mean something concrete in the data model rather than just a redaction flag.

**Why not a dedicated secrets store (SSM/Secrets Manager) now.** A managed secret store is the
stronger end state (per-secret rotation, audit, IAM-scoped access) but it's a larger change:
per-tenant SSM namespacing, deploy-time resolution, lifecycle management, and cost. Encrypting
in place with the existing primitive removes the plaintext-at-rest exposure immediately and is
fully compatible with a later SSM migration. Noted as future work in CURRENT_STATE.

**Why the `v1:` prefix matters here.** `crypto.Decrypt` is prefix-aware, so legacy plaintext
rows (no prefix) keep working during the transition and the startup backfill is idempotent
(it skips already-`v1:`-prefixed values). Decrypt failures at deploy time skip the variable
(and log) rather than injecting ciphertext into the container.

**Impact.** `envvars.Service` now takes `ENCRYPTION_KEY`/`ENCRYPTION_KEY_PREV`; encrypt on
`HandleUpsert`, decrypt on `HandleReveal` + `LoadForEnvironment`; `EncryptExistingSecrets`
backfill runs after migrations in `main.go`. No schema change (same `value` column). No new
dependency (reuses `pkg/crypto`).

---

## ADR-018 — Slack deploy/rollback disabled by default (no Slack→OpsPilot identity)
**Date:** 2026-06-16 · **Status:** Accepted

**Decision.** The destructive Slack slash commands (`/opspilot deploy`, `/opspilot
rollback`) are gated behind a per-workspace `slack_integrations.allow_slack_deploys` flag
that defaults to **false**. An OpsPilot admin must explicitly opt in (Settings →
Integrations). Read-only commands (`status`, `incidents`, `help`) are always available. The
gate is enforced at command time and re-checked in the interactivity (Approve button)
handler.

**Context.** Slack users are not mapped to Clerk/OpsPilot identities — the only mapping is
`team_id → org`. So a slash command's caller cannot be resolved to an OpsPilot user or
role; the RBAC that protects the in-app deploy button (engineer+) can't be applied to a
Slack invocation. As shipped, anyone in a connected Slack workspace could run `/opspilot
deploy` and trigger a production rollout. The workspace *connection* is admin-gated, but
individual callers were not checked.

**Why secure-by-default gating rather than full identity linking (now).** Properly closing
the gap means a Slack-user→OpsPilot-user linking flow (a mapping table, an OAuth/verify
step, role lookup) — a real feature, not a fix. Until that exists, the responsible default
is to not expose a production-deploy trigger to unauthenticated-by-us callers. A default-off
flag removes the dangerous default immediately, keeps the feature available to teams who
accept the tradeoff (small/trusted workspaces), and doesn't block the future linking work.

**Why keep it possible at all (vs. removing Slack deploys).** Slack-triggered deploys are a
genuinely useful workflow for some teams; removing the feature outright punishes trusted
workspaces for a gap that doesn't apply to them. The opt-in surfaces the risk explicitly in
the UI ("anyone in this Slack workspace could trigger deploys") so the admin enabling it is
making an informed choice.

**Why re-check in the interactivity handler.** The confirm/Approve button is only posted when
the gate is open, but Slack interactivity arrives as a separate signed request; defense-in-
depth means re-resolving the project's org and re-checking the flag before executing, so a
replayed or crafted payload can't bypass a disabled gate.

**Impact.** `addSlackDeployToggle` migration; `allow_slack_deploys` on `SlackIntegration`;
`HandleCommand` + `HandleInteractivity` gate checks (`slackDeploysAllowed`); the channel
PATCH accepts the flag; a default-off toggle with a risk explanation in Settings →
Integrations. Full Slack↔Clerk identity linking remains a future pass.

---

## ADR-017 — Billing is per-org (workspace), not per-user
**Date:** 2026-06-16 · **Status:** Accepted

**Decision.** Plan tier and usage limits (project count cap, monthly AI-action allowance)
are enforced per **organization** (workspace), not per user. `plan`,
`ai_actions_this_month`, and `ai_actions_reset_at` move to `organizations`;
`billing.CheckProjectLimit`/`IncrementAIAction`/`GetUsage` key off `org_id`. `/users/me`
reports the caller's **active org** usage (via the `X-Org-Id` header).

**Context.** When tenancy moved to orgs (ADR-009), all tenant data (projects, AWS accounts,
alerts, incidents) became org-owned, but billing still keyed off `user_id`: project counts
came from `projects WHERE user_id = ...` and AI metering incremented a counter on `users`.

**Why this was a real bug, not just a seam.** With per-user limits, a 5-member workspace on
the Free tier (1 project / 10 AI actions each) could create 5 projects and run 50 AI actions
collectively — the workspace as a whole blew past every cap the plan was supposed to enforce.
Worse, the *same* project counted against whichever member happened to create it, so limits
were effectively unbounded at the level that matters commercially (the paying entity is the
workspace). Per-org enforcement makes the limit mean what the pricing page says.

**Why keep the per-user columns.** Dropping `users.plan`/`ai_actions_*` would be a
destructive, irreversible migration for no functional gain; they're left in place (no longer
read for limits) so the change is reversible and any external reference doesn't break. The
org columns are the source of truth.

**How existing orgs are seeded.** The migration backfills each org's `plan` from its founding
user (`organizations.created_by`) when that user was on a paid plan — so a Pro user's personal
org stays Pro rather than silently downgrading to Free on first run.

**Why active-org for `/users/me`.** A user can belong to several workspaces with different
plans; usage is only meaningful relative to one. The frontend already sends `X-Org-Id` on
every request and reloads on workspace switch, so the navbar usage pill now reflects the
workspace in view with no extra plumbing.

**Impact.** `addBillingToOrganizations` migration; `billing.Service` methods re-keyed to
`orgID`; callers updated — `deploy.HandleCreateProject` (uses the already-resolved active
org), `conversation.ProcessMessage` (resolves the project's org before metering),
`users.HandleGetMe` (resolves active org via `middleware.ActiveOrg`). No payment integration
yet — this is limit enforcement, not invoicing.

---

## ADR-016 — On-call quiet hours: warn alerts suppressed, error alerts always break through
**Date:** 2026-06-16 · **Status:** Accepted

**Decision.** Each org can configure an on-call schedule (`oncall_schedules`: timezone,
quiet-hours window, quiet days). During quiet hours, **warn**-severity alert notifications
(email + Slack) are suppressed, while the `alerts` row and the real-time `alert` WebSocket
broadcast still happen. **Error**-severity alerts always notify regardless of the schedule,
with a note appended to the Slack message. No schedule configured ⇒ never quiet (fail open).

**Context.** Continuous monitoring fires alerts at all hours. Many are warn-level (a brief
latency blip, tasks momentarily degraded) that don't justify waking someone at 3am — but
some are error-level (service down) that absolutely do. Paging on everything trains people to
mute notifications, so the real outage gets ignored too. The goal is to cut nighttime noise
without ever silencing a real outage.

**Why severity is the breakthrough criterion.** Severity is already assigned at event
emission (`runtime.service_down` = error; `tasks_degraded`/`high_latency` = warn) and flows
onto the alert, so it's a signal we already trust and that drives the rest of the system
(diagnosis, health scores). Using it for paging keeps one consistent notion of "how bad is
this" rather than inventing a second priority axis. A service being **down** is the canonical
"wake me up" event; the warn tier is exactly the "can wait until morning" set. So error
breaking through quiet hours is the whole point — quiet hours suppress *noise*, never
*outages*.

**Why still write + broadcast warn alerts during quiet hours.** Quiet hours target *push*
notifications (email/Slack), not the record. An engineer who happens to be at their desk
should still see the alert appear live in the dashboard, and the history must be complete for
later review and the analytics dashboard. So suppression is strictly about not *paging* —
the alert still exists and streams over WebSocket.

**Why fail open.** If the schedule can't be loaded (no row, DB error, bad timezone), the
safe default is to notify — a missed page is worse than an extra one. `CheckQuietHours`
returns false on any error.

**Why org-level, not per-environment or per-user.** Quiet hours model a *team's* working
rhythm, which is an org-wide property; per-environment/per-user schedules add configuration
burden without a clear use case at this stage (`escalation_after_minutes` is stored for a
future escalation-policy pass but not yet acted on). Evaluated in the org's timezone so a
distributed team sets it once.

**Impact.** New `oncall_schedules` table; `monitor.CheckQuietHours` + quiet-hours branch in
`notifyOwner`; `GET`/`PUT /orgs/:orgId/oncall-schedule`; On-Call Schedule section in
`/settings/organization`. Slack `PostAlert` is unchanged — the error-severity note is
appended to a copy of the alert's summary for the Slack post only.

---

## ADR-015 — Uptime computed from operational events, not external probes
**Date:** 2026-06-16 · **Status:** Accepted

**Decision.** The leadership dashboard's uptime/SLA tracking is computed from the
`runtime.service_down` / `runtime.service_recovered` operational events the monitor already
emits, snapshotted daily into `uptime_snapshots` (one row per environment per day) by an
Asynq job. There is no separate external uptime-probing system (no synthetic HTTP checks
from outside AWS).

**Context.** A reliability dashboard needs an uptime number. The two obvious sources are
(a) external blackbox probes (a prober hits the app URL every N seconds from outside and
records pass/fail) or (b) the platform's own continuous monitoring, which already polls ECS
service health + ALB metrics every 60s and emits structured `runtime.*` events into
`operational_events` — the same substrate diagnosis, alerts, and health scores reason over.

**Why events, not probes.**
- **The signal already exists.** The Poller already detects service-down/recovered and
  writes timestamped events. Computing uptime from them is a SQL walk, not a new subsystem —
  no prober fleet, scheduling, egress, or per-environment URL/health-path config to manage.
- **One source of truth.** Uptime, alerts, incidents, and MTTD all derive from the same
  events, so the dashboard can't disagree with the alerts that fired. An external prober
  would introduce a second, independently-wrong notion of "down" to reconcile.
- **BYOC fit.** Probing customer apps from our infrastructure means egress to their
  environments, auth to private endpoints, and false positives from network paths we don't
  control. The in-account Poller already has the assumed-role access and sees real ECS/ALB
  state.
- **Honest partial days.** Snapshots clip downtime to the day and cap "today" at now, and
  outages spanning midnight are split across days — so a day's `uptime_pct` is accurate even
  mid-day.

**Trade-offs (accepted).** Uptime reflects *ECS/ALB health as the Poller sees it*, not true
end-user reachability — a CDN/DNS/region outage upstream of the ALB won't register, and a
service with no monitoring events reads as 100% (no observed downtime, not "proven up"). The
60s poll interval bounds resolution. These are documented in CURRENT_STATE; an external
synthetic-probe option can be added later as an additional event source without changing the
storage or aggregation (it would just emit the same `service_down`/`recovered` events).

**Why store daily snapshots** (vs. computing from raw events on every dashboard load):
bounded, indexable rows keep dashboard queries cheap and stable as event volume grows, give
trends a natural grain, and let the monthly report read a small fixed set of rows. The job is
idempotent per `(environment, date)` and recomputes yesterday+today each run to absorb
late-arriving recovery events.

**Impact.** New `internal/analytics` package; `uptime_snapshots` + `service_slas` tables;
`analytics:snapshot` (daily) and `analytics:monthly` (1st) scheduler jobs; org/project
analytics + SLA + report endpoints; monthly report stored in `daily_summaries`
(`is_monthly`). Dashboard charts are dependency-free SVG (recharts not installed).

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

---

## ADR-020 — Dual onboarding paths (Flow A / Flow B) converging on one trust ladder
**Date:** 2026-06-21 · **Status:** Accepted (vision-level); promotion mechanics and
Flow B's IAM role separation are explicitly **not yet designed** — see Open Decisions
in `ROADMAP.md`.

**Decision.** OpsPilot serves two populations through two onboarding flows that
converge into a single five-tier trust model, rather than as two separate products:

- **Flow A (new infrastructure).** OpsPilot provisions and deploys from day one.
  Starts at Tier 2 (execute with approval) by default, because there is no existing
  production to put at risk.
- **Flow B (existing infrastructure).** OpsPilot connects read-only — to AWS, repos,
  and eventually existing monitoring — and starts at Tier 0 (observe only: discover,
  monitor, diagnose, recommend; no write access at all). It earns Tier 1
  (recommend — generate reviewable IaC/alarm/runbook artifacts), then Tier 2, then
  potentially Tier 3, the same way Flow A's environments earn Tier 3: by an explicit,
  admin-confirmed promotion based on track record, never automatically.

Both flows are governed by the same `trust_level` concept already implemented for
environments (`suggest`/`supervised`/`autonomous`, ADR-013) — Flow B is a new entry
point into that model, not a parallel one.

**Context.** The existing product (and `PRODUCT_VISION.md` prior to this revision)
only described Population A: small teams with no existing AWS footprint. That's a
real but narrow market. Most companies above the earliest stage already run
production infrastructure on AWS with their own CI/CD, IaC, and monitoring — they
will not migrate to adopt a deployment platform, and they will not grant an unproven
AI write access to production on day one. Without a path for this population, every
company past ~5 engineers with existing infrastructure was simply unaddressable, and
infrastructure discovery (ADR-010, already built) had no onboarding story to sit
underneath.

**Alternatives considered.**
- *Single onboarding flow, ask everyone to connect read-write from the start* —
  rejected: this is the status quo and it's exactly what makes Population B
  unaddressable. No serious engineering org grants write access to an unproven
  system.
- *Two separate products (a "deploy platform" and an "observability platform")* —
  rejected: doubles the surface area to build and maintain, and throws away the
  actual insight, which is that both populations want the same end state (an AI that
  diagnoses and eventually acts) — they just need to start from different trust
  positions to get there safely. Segmenting by product instead of by trust tier would
  also strand Flow A customers who grow past the "small team" stage with no natural
  path to the capabilities Flow B customers eventually unlock.
- *Let Flow B customers request write access immediately, gated only by a checkbox* —
  rejected: a checkbox is not a trust signal. It would either be ignored (everyone
  checks it, no actual risk reduction) or distrusted (security teams won't accept
  "the customer agreed" as the control). The whole point of Flow B is that trust has
  to be demonstrated with evidence (`ai_actions`, `diagnosis_feedback` already
  capture the raw data for this, per ADR-012/ADR-013), not asserted.

**Rationale.** The two flows are different starting *positions* on the same ladder,
not different products, because the thing being sold is identical in both cases — an
AI that gets more capable and more trusted the longer it operates correctly. Starting
position should track real risk (does this account currently run anything that
matters), not company size or segment. This also means every investment already made
in the trust ladder (explainability — ADR-012, approval flows — ADR-013) pays off for
both populations simultaneously, rather than needing a separate trust mechanism built
for Flow B specifically.

**Explicitly deferred, not decided here.**
- The exact thresholds for tier promotion (incident count, time window, false-positive
  rate) — flagged in `PRODUCT_VISION.md` as the single most important open mechanism
  in the product. "Trust is earned over time" is not itself a mechanism.
- Whether Flow B's read-only IAM role is enforced by policy alone or also
  belt-and-suspenders-checked in the application layer before any write call.
- Tier 1 (IaC generation) implementation — real, scoped, multi-month future work; not
  to be estimated or started opportunistically.
- Whether a Tier 3 grant is durable once given or requires periodic re-confirmation.

**Impact.** `PRODUCT_VISION.md` rewritten around the two-population framing and the
trust ladder table. `ROADMAP.md` near-term priority list reordered around Flow B's
read-only onboarding path. No code changes yet — this ADR records the vision-level
decision; the promotion-mechanics and IAM-role-separation designs are tracked as open
decisions and must be resolved before either is handed to Claude Code as an
implementation prompt.