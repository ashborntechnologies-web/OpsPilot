# Roadmap — OpsPilot

Organized by status. Keep in sync with `CURRENT_STATE.md`. Vision context in
`PRODUCT_VISION.md`. Last reviewed **2026-06-11**.

## ✅ Implemented (shipped on `main`)
- Conversational deploy/rollback/logs/health/scale/diagnose/cost/resource-change.
- BYOC AWS via assumed role + per-tenant external ID; bootstrap CloudFormation flow.
- Shared platform stacks; per-env provisioning; CodeBuild→ECR→ECS deploy with live
  log/progress streaming; rollback/redeploy/cancel/delete.
- Failure & runtime diagnosis with project-memory injection + feedback capture.
- Continuous monitoring (Poller + LogScanner) → dedup AI-summarized alerts (WS + email)
  → auto-diagnosis; watchdog for stuck deploys.
- Risk score, health score, cost intelligence, PR previews, env vars, webhooks,
  browser terminal, billing/metering, notification prefs.
- Proprietary posture (external prompts, admin exports, legal, headers, robots.txt).

## 🚧 In progress
- **Continuous-operation intelligence loop** — tighten detect→diagnose→remember→advise;
  improve alert summary + diagnosis quality as prompts/memory mature.
- **Training-data moat** — grow the labeled diagnosis/intent datasets toward model
  improvement (export pipeline exists; no fine-tune loop yet).
- **HTTPS adoption** — make cert provisioning smoother (currently requires a
  user-supplied ACM ARN).

## 🔜 Near-term priorities (next)
1. **Onboarding checklist** — live 3-step banner on `/projects`
   (connect GitHub → connect AWS → create project) reflecting real completion state, to
   raise activation. *(product gap noted in CURRENT_STATE)*
2. **Hero trust signals** — surface the scoped/revocable IAM-role guarantee above the
   fold on the landing page.
3. **Refactor `internal/deploy/service.go`** (~2700 lines) into `workflow.go` /
   `preview.go` / `mutations.go` / `handlers.go`; keep `Service` + constructor in
   `service.go`. Behavior-preserving; needs careful test coverage.
4. **Secret storage hardening** — move secret env vars toward an SSM-backed store with
   per-secret audit (currently plaintext-at-rest in Postgres).

## 🌅 Long-term vision (future ideas)
- **Autonomous remediation** — propose *and apply* fixes (staging-first, confirmation
  for production); close the detect→act loop.
- **Staging-first trust model** — AI proves a fix on staging before it's allowed near
  production; graduated autonomy as confidence grows.
- **Process/scale-out** — split the Asynq worker + monitors from the API for horizontal
  scaling; multi-region environments.
- **Deeper cost/perf advice** — proactive right-sizing recommendations from metrics +
  cost data.
- **Broader source/runtime support** — beyond GitHub + Fargate as demand warrants
  (still AWS-only; multi-cloud remains a non-goal for now).

> Discipline: validate every new feature against `PRODUCT_VISION.md` (BYOC,
> Claude-classifies/Go-executes, staging-first) before building. Update this file +
> `CURRENT_STATE.md` + `CHANGELOG_AI.md` when status changes.
