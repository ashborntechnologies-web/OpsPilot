# Current State — OpsPilot

Snapshot as of **2026-06-11**. Update whenever a feature's status changes.
Status is derived from the code on `main`, not from plans.

## ✅ Working (implemented & wired)

**Core deploy loop**
- Project creation from a GitHub repo (18 frameworks; AI framework detection).
- BYOC AWS connection via bootstrap CloudFormation + assumed IAM role (per-tenant
  external ID); AssumeRole validated at connect time.
- Shared platform stack (VPC/ECS/ALB) per account×region; per-env project provisioning.
- Deploy: CodeBuild image build (generated Dockerfile/buildspec) → ECR → ECS rollout →
  health check → live, with **live build-log + stage-progress streaming over WebSocket**.
- Rollback, redeploy, cancel (stops CodeBuild), delete (async AWS cleanup).
- Per-environment env vars (secrets redacted/reveal), injected into the task def.

**Conversational interface**
- Chat over WebSocket: intent classification (Claude) → Go execution for deploy,
  rollback, logs, health, scale, diagnose, cost, resource-change. Graceful fallback on
  AI outage. Persisted history. Context-aware suggested prompts.
- Browser terminal into a running ECS task (SSM exec proxied to xterm).

**AI / intelligence**
- Failure diagnosis (root cause + fix) from logs + structured event timeline + history,
  with **project memory injected into the prompt**; diagnosis feedback capture (👍/👎).
- Long-term project memory (recurring failures, successful fixes, deploy patterns) with
  near-duplicate merging.
- Pre-deploy risk score (advisory, broadcast as `deploy_risk`) + deployment health score.

**Trust levels & AI-action approval**
- Per-environment trust level (`suggest`/`supervised`/`autonomous`) with autonomous
  boundaries (can_rollback/can_scale/min-max replicas/can_change_resources). Configurable
  in the project Settings tab (admin-only).
- AI-initiated actions (chat deploy/rollback/scale/change_resources; diagnosis → rollback
  recommendation) route through `trust.ProposeAction`: auto-execute when autonomous +
  within boundaries, else recorded as a pending approval (`ai_actions`). Deploys always
  require approval. Execution routes to the deploy service; incident-linked actions post to
  the war-room timeline.
- Approve/reject (engineer+) via the Pending Approvals panel (right column) + navbar badge;
  `action_proposed` WS banner; project Actions tab (AI/human history); Slack proposal with
  a review link.

**Explainability & confidence**
- Every AI decision shows *why*: diagnoses carry a confidence score (0–1) + a structured
  evidence array (`{type, description, data, weight}`) from a second Claude call, stored on
  the incident and rendered as a confidence badge + collapsible evidence list (diagnosis
  dialog, war room, chat).
- Alerts carry a deterministic `evidence_text` (1–2 sentences from the event payload),
  shown under the summary in the alerts panel.
- Risk scores expose every factor's reason + a `top_factor`; the full factor list is in the
  `deploy_risk` WS payload. Shared UI: `components/ai/explainability.tsx`.

**Daily operational summary**
- AI morning briefing per org (`internal/summary`): last-24h deploys, incidents + MTTR,
  alerts, 7-day top recurring failures → Claude paragraph + ≤3 grounded recommendations,
  rendered to markdown and stored (`daily_summaries`, one per org/day).
- Delivered to Slack + emailed to admins/engineers; per-org schedule (time/timezone/
  enabled) via an hourly scheduler tick that fans out per-org generation jobs.
- UI: "Daily summary" card on the projects dashboard, config + "Send test summary" in org
  settings, and a `/orgs/[orgId]/summaries` history page. Supersedes the simpler Slack-only
  digest.

**Slack integration**
- Per-org Slack connect via OAuth v2 (admin); bot token encrypted at rest (`pkg/crypto`).
- Notifications (best-effort): color-coded **alerts** (link to war room), **deploy
  results** (green/red), and a daily **summary** digest (scheduler at 14:00 UTC). Channels
  configurable per purpose on `/settings/integrations`.
- `/opspilot` slash commands: `status`, `incidents`, `deploy [project] [env]`,
  `rollback [project]`, `help` — deploy/rollback post an in-channel Approve confirmation
  handled via `/slack/interactivity`. Request signatures verified (`X-Slack-Signature`).

**Incident war room**
- Incidents are first-class, lifecycle-tracked (`open`→`investigating`→`resolved`) with
  severity, acknowledgement, resolution, and an AI-generated postmortem.
- Auto-created from completed diagnoses (deploy failure + runtime anomaly), deduplicated;
  the AI diagnosis is the first timeline entry and the suggested fix becomes a pending
  action.
- Real-time war room (`/incidents/[id]`): shared timeline feed (AI + human, live over a
  per-incident WebSocket), engineer update composer, AI-actions approve/reject panel,
  acknowledge/resolve. Org incident list (`/incidents`) + navbar open-incident badge.
  Alert emails link to the war room.

**Automated postmortems**
- On resolve, a postmortem is generated **asynchronously** (Asynq job — ADR-014), so the
  resolve action returns instantly; the war room polls and shows a "Generating…" state,
  then a preview. Claude drafts fixed sections (Summary / Timeline / Root Cause /
  Contributing Factors / What Went Well / Action Items) from the timeline, diagnosis, AI
  actions, deployment, and project memory; structured action items are parsed out.
- Editable on `/postmortems/[id]/edit` (markdown + action-item editor, live preview),
  publishable to the org library (`/orgs/postmortems`, SOC2-framed, searchable/filterable),
  exportable as markdown or print-ready HTML (Save-as-PDF). Generation also writes the fix
  to project memory (`successful_fix`) so future diagnoses improve. Navbar "Postmortems" link.

**Infrastructure discovery**
- Scans connected AWS accounts for existing resources (ECS services/clusters, RDS,
  ElastiCache, Lambda, S3, ALBs, SQS) so users onboard without migration. Parallel,
  isolated scanners; idempotent upsert into `discovered_resources`; `ManagedBy=OpsPilot`
  → `is_managed`.
- Triggered automatically on AWS-account connect, on demand (`POST /aws-accounts/:id/scan`),
  and daily via the scheduler. Inventory view (`/orgs/resources`) with type/region
  filters + assign-to-project; per-project Infrastructure tab; scan status on the
  AWS-accounts page.
- Discovered ECS services **assigned to a project** are monitored (health poll + log
  anomaly scan) like OpsPilot-created environments.

**Continuous monitoring**
- Poller (ECS/ALB health, 60s) + LogScanner (CloudWatch anomaly patterns, 5m) →
  `runtime.*` operational events.
- Alert engine: dedup, AI summaries, snooze, auto-resolve on recovery; delivered via
  WebSocket + email; alerts panel + status sidebar in the UI.
- Auto-diagnosis enqueued on runtime failures (events → diagnosis job).
- Watchdog reconciles stuck deploys every 5m.

**Team workspaces & RBAC**
- Organizations (team workspaces) own all tenant data (`projects`, `aws_accounts`,
  `alerts`, `incidents` via `org_id`); every user gets a personal org on first login;
  existing data migrated by `backfillPersonalOrgs`.
- Roles **admin > engineer > viewer** enforced at the middleware layer
  (`LoadProjectMembership` + `RequireRole`, `RequireOrgMembership`); viewers are
  read-only (chat action intents also blocked). Active workspace via `X-Org-Id` header.
- Invite by email (token link, 7-day expiry, emailed via `notify.SendOrgInvite`),
  accept page, member list with role badges, role changes + removal (admin; last-admin
  protected). Navbar workspace switcher + role-aware dashboard (view-only banner +
  guarded actions).

**Platform**
- Billing: plan tiers (free/pro/team), project limit, monthly AI-action metering;
  usage meter in navbar; notification preferences in settings.
- Outbound webhooks (deploy events, HMAC-signed, SSRF-guarded).
- PR preview environments (GitHub webhook → ephemeral `pr-N` env + PR comment).
- Cost intelligence (30-day Cost Explorer summary).
- Proprietary posture: trade-secret prompts external; `Proprietary()` headers; admin
  training-data exports behind `ADMIN_API_KEY`; legal pages; `robots.txt` on API host.
- Security: Clerk auth, tenant-isolation middleware, encrypted GitHub tokens, request IDs.

## 🟡 Partial / thin

- **Onboarding:** functional but minimal — `/projects` shows an empty-state CTA, not a
  live multi-step GitHub→AWS→project checklist. New users can dead-end if GitHub/AWS
  isn't connected first.
- **HTTPS:** plumbed end-to-end (`certificate_arn` field, CF 443 listener,
  `https_enabled`, URL scheme) but optional and depends on the user supplying an ACM cert.
- **Landing-page trust signals:** present but in the "how it works" section rather than
  the hero / above the fold.
- **Resource mutations / autonomy:** CPU/memory changes are *proposed then confirmed*;
  there is no autonomous remediation yet.
- **Platform deployment:** no committed platform Dockerfile / CI manifest; runs via
  `make run` against docker-compose-provided Postgres/Redis.

## 🧪 Experimental / proprietary-in-progress

- Training-data export pipeline (intents + diagnoses) — datasets exist; no model
  fine-tuning loop in-repo.
- Runtime auto-diagnosis quality depends on prompt + memory maturity.

## ⚠️ Known limitations

- **AWS-only**, single-region per environment, Fargate-only.
- **Single binary** — API, worker, and monitors share a process; no horizontal split yet.
- Platform stack is **not** torn down on project deletion (intended; shared).
- `internal/deploy/service.go` is ~2700 lines (maintainability risk; see ROADMAP #refactor).
- Secret env-var values stored plaintext in Postgres (encrypted at rest; no per-secret
  audit / SSM-backed secret store yet).
- Email requires SMTP config; without it, notifications are logged no-ops.
- **Billing is still per-user** (`CheckProjectLimit`/AI metering key off `user_id`), not
  per-org — a known seam now that projects are org-owned.
- **Daily-summary cost-change is stubbed** — `summary.costChange` returns 0 (the
  `CostChangePct` field + 7d-vs-prior-7d Cost Explorer comparison are not yet implemented;
  it's omitted from output when 0). Everything else in the summary is real DB data.
- **Diagnosis auto-proposes a rollback** for every deploy-failure diagnosis (in `suggest`
  mode it's just a pending suggestion). `terminal_command` actions are modeled but not
  auto-executable. `change_resources` execution goes through propose+apply.
- **Slack slash commands aren't tied to a platform user/role** — Slack users aren't
  mapped to Clerk identities, so anyone in a connected workspace can run `/opspilot
  deploy` (workspace connection is admin-gated). The alert "Acknowledge" button is a link
  to the war room (no per-alert incident exists at alert time, so it can't be a true
  action button). Both noted for a future Slack-identity-linking pass.
- Role-aware dashboard guards the action **handlers** + shows a view-only banner;
  per-button `disabled` styling across every control is a follow-up (backend enforces
  regardless, so viewers always get 403).
- **Discovery scan depends on IAM permissions:** the assumed bootstrap role must allow
  the read-only `Describe*`/`List*` calls (RDS, ElastiCache, Lambda, S3, SQS, ELBv2,
  ECS). The current bootstrap template may not grant all of these — missing permissions
  cause individual scanners to log + skip (no crash). Updating the template's policy is
  a follow-up.
- Discovery scans only the regions an account is already used in (env + platform stacks),
  defaulting to `us-east-1` for a fresh account. `cloudfront_distribution`/`ec2_instance`
  types are defined but have no dedicated scanner yet. Only discovered **ECS services**
  feed the monitor.
- **Incident actions are advisory** — approving an AI-proposed action records the decision
  (status + approver + timestamp) but there is no autonomous executor yet; the engineer
  still performs the fix. Markdown in the war room uses a small built-in renderer (headings/
  bold/code/lists), not a full markdown engine. Alert emails link to the incident **list**
  (the specific incident is opened by the diagnosis job that runs after the alert).

## 🔭 Actively developing
Continuous-operation intelligence (monitoring → alert → diagnosis → memory loop) and
the proprietary AI moat (prompt + training pipeline). See `ROADMAP.md`.
