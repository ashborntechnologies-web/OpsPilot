# Roadmap — OpsPilot

Organized by status. Keep in sync with `CURRENT_STATE.md`. Vision context — including
the **two-population model** and the **five-tier trust ladder** this roadmap is now
organized around — is in `PRODUCT_VISION.md`. Read that first; this file assumes it.
Last reviewed **2026-06-21**.

## ✅ Implemented (shipped on `main`)

**Flow A (new infrastructure) — complete core loop**
- Conversational deploy/rollback/logs/health/scale/diagnose/cost/resource-change.
- BYOC AWS via assumed role + per-tenant external ID; bootstrap CloudFormation flow.
- Shared platform stacks; per-env provisioning; CodeBuild→ECR→ECS deploy with live
  log/progress streaming; rollback/redeploy/cancel/delete.
- Failure & runtime diagnosis with project-memory injection + feedback capture.
- Continuous monitoring (Poller + LogScanner) → dedup AI-summarized alerts (WS + email)
  → auto-diagnosis; watchdog for stuck deploys.
- Risk score, health score, cost intelligence, PR previews, env vars, webhooks,
  browser terminal, billing/metering (per-org, ADR-017), notification prefs.
- **Onboarding checklist** — live 3-step banner on `/projects` (connect GitHub →
  connect AWS → create project) reflecting real completion state
  (`frontend/components/onboarding/checklist.tsx`).
- **Hero trust signals** — the scoped/revocable IAM-role guarantee is surfaced above
  the fold on the landing page (`app/page.tsx`).

**Trust ladder — Tiers 0/2/3 (partial)**
- Environment trust levels (`suggest`/`supervised`/`autonomous`) with autonomous
  boundaries; `trust.ProposeAction` routes AI-initiated deploy/rollback/scale/
  resource-change through approval or auto-execution (ADR-013).
- Explainability substrate: confidence scores + structured evidence on diagnoses;
  `evidence_text` on alerts; risk-score factor reasons (ADR-012). This is what makes
  any future tier promotion auditable rather than a black box.

**Team, incident, and reporting layer**
- Team workspaces (organizations) + RBAC (admin/engineer/viewer) as the tenant
  boundary (ADR-009) — required before Flow B can exist (an org, not a single user,
  is what earns trust over time).
- Incident war room (real-time shared timeline, AI+human), async postmortem
  generation (ADR-014), Slack integration (OAuth, alerts/deploys/summary, slash
  commands — ADR-011, gated per ADR-018), daily operational summaries, leadership
  analytics (uptime/MTTD/MTTR/SLA, ADR-015), on-call quiet hours (ADR-016).
- Infrastructure discovery scanners (ECS/RDS/ElastiCache/Lambda/S3/ALB/SQS,
  ADR-010) — **the technical foundation Flow B's onboarding will sit on top of.**
- Secret env vars encrypted at the application layer (ADR-019).

**Proprietary posture**
- External prompts, admin-only training-data exports, BUSL licensing, proprietary
  headers, robots.txt.

## 🚧 In progress
- **Continuous-operation intelligence loop** — tighten detect→diagnose→remember→advise
  for Flow A; improve alert summary + diagnosis quality as prompts/memory mature.
- **Training-data moat** — grow the labeled diagnosis/intent datasets toward model
  improvement (export pipeline exists; no fine-tune loop yet).
- **HTTPS adoption** — make cert provisioning smoother (currently requires a
  user-supplied ACM ARN).

## 🔜 Near-term priorities (next)

These are ordered by leverage, not by ease. #1 is the most important thing in this
document — see `PRODUCT_VISION.md` "Current product direction."

1. **Flow B onboarding: read-only IAM role + Tier 0 entry point.** Discovery scanning
   already exists (ADR-010, `internal/discovery`); what's missing is (a) a distinct,
   genuinely read-only-by-construction IAM role/bootstrap path for Flow B, separate
   from the read-write role Flow A requires, and (b) the onboarding UI that connects
   AWS/repos without ever provisioning anything. This is the unlock for every
   customer with existing production infrastructure who won't migrate. **Nothing
   else in this section matters as much as this one.**
2. **Tier 0→1 promotion mechanics.** `PRODUCT_VISION.md` flags this as the single most
   important *undecided* mechanism in the product: concrete thresholds, the
   admin-facing proposal UI ("I've correctly diagnosed N incidents... grant me
   permission to..."), and the audit trail for accept/decline. Needs design work
   before it's an engineering ticket — see Open Decisions below.
3. **Refactor `internal/deploy/service.go`** (~2700 lines) into `workflow.go` /
   `preview.go` / `mutations.go` / `handlers.go`; keep `Service` + constructor in
   `service.go`. Behavior-preserving; needs careful test coverage. Worth doing before
   Flow B work lands more code in the same file.
4. **Secret storage hardening, phase 2.** ADR-019 closed the plaintext-at-rest gap
   with application-layer encryption; the next step (SSM/Secrets Manager-backed
   store with per-secret rotation and audit) remains future work, noted in that ADR.
5. **Slack slash-command authorization gate, phase 2.** ADR-018 shipped the
   default-off `allow_slack_deploys` gate, closing the "any workspace member can
   trigger a deploy" gap. Full Slack↔OpsPilot identity linking (mapping a Slack
   `user_id` to an OpsPilot member and role) is the proper long-term fix and remains
   tracked as future work below.

## 🌅 Long-term vision (future ideas)

Organized against the trust ladder tiers defined in `PRODUCT_VISION.md`.

**Tier 1 — Recommend (AI SRE)**
- Generate reviewable Terraform/CloudFormation diffs, alarm definitions, and runbooks
  for infrastructure OpsPilot doesn't have write access to (Flow B customers, and
  Flow A customers who haven't granted Tier 3 in a given environment).
- This requires understanding a customer's existing IaC conventions (module
  structure, naming, provider versions) well enough to generate a diff that looks
  like it was written by their own team, and a PR-based review flow rather than a
  blob of YAML in a chat window. Sized as real, multi-month scope — not to be
  estimated casually when it's eventually picked up.

**Tier 3 — Execute autonomously, broadened**
- **Autonomous remediation** beyond today's narrow boundaries (replica count,
  approved resource changes) — propose *and apply* fixes for a wider action set,
  still staging-first and still per-environment-earned.
- **Staging-first remediation sequencing** — not just "staging has a higher trust
  ceiling" (already true) but OpsPilot actively proposing "let me try this fix on
  staging first, watch it for N minutes, then propose the same fix for production if
  staging looks healthy" as an explicit, visible workflow rather than an implicit
  policy.

**Tier 4 — Architect**
- Proactive right-sizing and cost/perf recommendations from metrics + cost data.
- Long-term platform/architecture advice based on accumulated operational knowledge
  across both Flow A and Flow B customers. Explicitly depends on Tiers 1–3 being
  mature and on enough accumulated data to make recommendations trustworthy — not
  to be started early.

**Platform/infra**
- **Process/scale-out** — split the Asynq worker + monitors from the API for
  horizontal scaling; multi-region environments.
- **Full Slack↔OpsPilot identity linking** — map Slack `user_id` to an OpsPilot
  member and enforce their actual role on the caller, superseding the
  `allow_slack_deploys` gate shipped in ADR-018.
- **Broader source/runtime support** — beyond GitHub + Fargate as demand warrants
  (still AWS-only; multi-cloud remains a non-goal for now).
- **Code understanding** — read actual diffs/dependencies from GitHub (not just
  commit SHA + message) so diagnosis can correlate a specific code change with a
  specific failure, not just "a deploy happened around this time."

## 🧭 Open decisions (need a real answer before engineering starts)

These are flagged in `PRODUCT_VISION.md` and repeated here because a roadmap item
above depends on each one. Do not write an implementation prompt for the dependent
item until the decision is made.

1. **Tier 0→1 (and 1→2, 2→3) promotion thresholds.** What exactly counts as "earned
   it" — incident count, time window, false-positive rate, some combination? Blocks
   near-term priority #2.
2. **Does a Flow A environment's Tier 3 grant expire or require periodic
   re-confirmation**, or is it durable once given? Affects the audit/UI design for
   trust level changes.
3. **What does "read-only by construction" mean precisely for the Flow B IAM role** —
   is it enforced by IAM policy alone (describe/list/get actions only, no write
   actions in the policy at all), or does the application layer also need a
   belt-and-suspenders check before any AWS write call for Flow B-sourced
   credentials? Blocks near-term priority #1.

> Discipline: validate every new feature against `PRODUCT_VISION.md` (BYOC,
> Claude-classifies/Go-executes, the trust ladder, explicit/admin-confirmed
> promotion only) before building. Update this file + `CURRENT_STATE.md` +
> `CHANGELOG_AI.md` when status changes.