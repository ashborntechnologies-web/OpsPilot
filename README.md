# OpsPilot (ConvDeploy) — AI-Native Conversational Deployment Platform

OpsPilot is an AI operator for AWS infrastructure. A developer connects a GitHub repo and
their **own** AWS account, then deploys and operates real infrastructure by *talking to it*
("deploy to production", "why did the last deploy fail?", "scale to 3"). It also watches
deployments continuously, diagnoses failures, and surfaces the result as incidents,
postmortems, and leadership analytics.

Two properties define the system:

1. **BYOC (Bring Your Own Cloud).** The user's app runs in *their* AWS account. OpsPilot
   never stores long-lived AWS credentials — it assumes a scoped, revocable IAM role
   (per-tenant external ID) via `sts:AssumeRole`.
2. **Intent-first AI.** Claude is used for **classification and diagnosis only** — it never
   touches AWS. The LLM turns natural language into a structured intent; Go code executes
   every infrastructure action. This is the core invariant.

> **Deep docs live in [`docs/ai-context/`](docs/ai-context/)** — architecture, per-module
> backend reference, full DB schema, API contracts, current state, roadmap, and ADRs. Start
> with [`docs/ai-context/ARCHITECTURE.md`](docs/ai-context/ARCHITECTURE.md). This README is
> the quick-start; that directory is the source of truth.

---

## Architecture at a glance

- **Backend:** a single Go binary (Gin HTTP API + Asynq worker + scheduler + monitors all in
  one process), Postgres for state, Redis for the Asynq job queue. Cloud mutations run in the
  user's AWS account via assumed roles (`internal/aws`). Claude via `internal/llm`.
- **Frontend:** Next.js (App Router) with Clerk auth, SWR, and raw WebSocket for live deploy/
  chat/incident streams. It proxies `/api/v1/*` to the backend (no CORS in dev).
- **Substrate:** every meaningful state transition is written to `operational_events`;
  diagnosis, alerts, risk/health scores, and uptime analytics all reason over those events.
- **Tenancy:** organizations (team workspaces) + RBAC (`admin` > `engineer` > `viewer`).

```
Next.js frontend ──/api/v1 (proxied)──▶ Go API (Gin) ──▶ Postgres
       │                                    │
       └────────── WebSocket ───────────────┤──▶ Redis / Asynq (deploy/diagnose/… jobs)
                                            │──▶ Anthropic (Claude) — classify + diagnose
                                            └──▶ User's AWS account (AssumeRole: ECS/ECR/
                                                  CodeBuild/ALB/CloudWatch/…)
```

---

## Prerequisites

- **Go 1.24+**
- **Node.js 20+** and npm (frontend is Next.js 16 / React 19)
- **Docker** (for local Postgres + Redis via `docker compose`)
- A **Clerk** application (publishable + secret keys) — required for auth on both ends
- An **Anthropic API key** — optional locally, but chat/diagnosis are disabled without it
- For real deploys: an **AWS account** + the bootstrap CloudFormation role (set up in-app)

---

## Running the backend

```bash
# 1. Install Go deps
go mod tidy

# 2. Create your env file and fill it in (see "Environment" below)
cp .env.example .env

# 3. Start Postgres + Redis (docker compose)
make docker-up

# 4. Run the API (migrations run automatically on startup)
make run            # = go run ./cmd/api/main.go
```

The API listens on **`:8080`** by default (override with `PORT`). Database migrations are
applied automatically at startup (`pkg/models.RunMigrations`) — there is no separate migrate
step.

**Required env (the server fails fast without these):** `DATABASE_URL`, `REDIS_URL`,
`CLERK_SECRET_KEY`, `CLERK_PUBLISHABLE_KEY`, `ENCRYPTION_KEY`.

### AI prompts (trade secret — required to boot)

The intent-classifier and diagnosis prompts are **not** in source; the server refuses to
start without them. Point the `INTENT_CLASSIFIER_PROMPT_FILE` / `DIAGNOSIS_PROMPT_FILE`
variables at local files (the `prompts/` directory is git-ignored), or set the inline
`INTENT_CLASSIFIER_PROMPT` / `DIAGNOSIS_PROMPT` variables. **Never commit prompt contents.**

---

## Running the frontend

```bash
cd frontend

# 1. Install deps
npm install

# 2. Create your local env file and fill it in
cp .env.local.example .env.local

# 3. Start the dev server
npm run dev
```

The frontend runs on **http://localhost:3000** and proxies `/api/v1/*` to the backend at
`NEXT_PUBLIC_API_URL` (default `http://localhost:8080`), so run the backend first.

**Frontend env (`frontend/.env.local`):**

| Variable | Purpose |
|---|---|
| `NEXT_PUBLIC_CLERK_PUBLISHABLE_KEY` | Clerk publishable key (must match the backend's Clerk app) |
| `CLERK_SECRET_KEY` | Clerk secret key |
| `NEXT_PUBLIC_API_URL` | Backend base URL (`http://localhost:8080` in dev) |
| `NEXT_PUBLIC_CLERK_SIGN_IN_URL` / `_SIGN_UP_URL` | `/sign-in`, `/sign-up` |
| `NEXT_PUBLIC_CLERK_AFTER_SIGN_IN_URL` / `_AFTER_SIGN_UP_URL` | `/projects` |
| `NEXT_PUBLIC_SITE_URL` | Optional — used for `robots.txt` sitemap |

> WebSockets connect **directly** to `NEXT_PUBLIC_API_URL` (they can't traverse the Next
> proxy), so that URL must be reachable from the browser.

---

## Environment reference (backend `.env`)

See [`.env.example`](.env.example) for the full list. Highlights:

**Required:** `DATABASE_URL`, `REDIS_URL`, `CLERK_SECRET_KEY`, `CLERK_PUBLISHABLE_KEY`,
`ENCRYPTION_KEY` (≥16 chars; key for encrypting tokens + secret env vars at rest).

**AI (required to boot):** `INTENT_CLASSIFIER_PROMPT(_FILE)`, `DIAGNOSIS_PROMPT(_FILE)`;
`ANTHROPIC_API_KEY` (chat/diagnosis disabled if unset).

**Optional integrations (each degrades gracefully when unset):**
- `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` — repo connection + PR previews
- `SLACK_CLIENT_ID` / `SLACK_CLIENT_SECRET` / `SLACK_SIGNING_SECRET` — Slack integration
- `SMTP_HOST` / `SMTP_PORT` / `SMTP_USER` / `SMTP_PASS` — email notifications
- `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` — authenticated base-image pulls during builds,
  to avoid Docker Hub's anonymous rate limit (read-only token; see ADR-021)
- `ADMIN_API_KEY` — admin training-data export endpoints
- `FRONTEND_URL` — CORS + WebSocket origin allowlist; `PUBLIC_API_URL` — for OAuth callbacks
- `PLATFORM_AWS_ACCOUNT_ID` — overrides detected platform account for the bootstrap template

---

## Project structure

```
cmd/api/main.go         → composition root: wires every service, mounts routes, starts
                          HTTP server + WS hub + Asynq worker + scheduler + monitors
internal/
  auth/                 → Clerk JWT validation (JWKS)
  conversation/         → intent classification (Claude) → routes to a Go workflow
  deploy/               → deploy/rollback/provision workflows, previews, risk/health,
                          cost, resource mutations, project CRUD (the orchestration core)
  aws/                  → AssumeRole, CloudFormation, ECS/ECR/CodeBuild/ALB/CloudWatch,
                          bootstrap + platform-stack templates, buildspec
  diagnosis/            → log + event-timeline analysis → root cause + fix (Claude)
  memory/               → long-term per-project memory fed back into diagnosis prompts
  events/               → operational-event hub (fan-out to alerts + auto-diagnosis)
  monitor/              → health poller + log scanner + alert engine + on-call quiet hours
  incidents/            → incident war room (real-time AI+human timeline)
  postmortem/           → async AI postmortem generation on incident resolve
  trust/                → AI-action approval gated by per-environment trust levels
  discovery/            → scanners that inventory existing AWS resources (Flow B foundation)
  analytics/            → SLA/uptime, MTTD/MTTR, reliability trends, monthly report
  summary/              → daily operational AI briefing per org
  slack/                → per-org Slack notifications + /opspilot slash commands
  orgs/ billing/        → team workspaces/RBAC; plan limits + AI metering (per-org)
  envvars/ webhooks/    → per-env vars (secrets encrypted); outbound webhooks
  github/ terminal/     → GitHub OAuth/detection; browser shell into ECS (SSM exec)
  queue/ llm/ prompts/  → Asynq jobs; Anthropic client; trade-secret prompt loader
  notify/ export/       → email (SMTP); admin training-data exports
pkg/
  models/               → Postgres schema (migrations-in-code) + entity types
  ws/                   → WebSocket hub (project + incident rooms)
  middleware/           → auth, RBAC, CORS, rate limit, request IDs
  crypto/               → AES-256-GCM for secrets at rest
frontend/               → Next.js App Router UI (see frontend/AGENTS.md)
docs/ai-context/        → living architecture/decision docs (read these)
```

---

## Testing

```bash
make test-unit          # fast, no external deps (~1s)
make test-integration TEST_DATABASE_URL="postgres://convdeploy:convdeploy@localhost:5432/convdeploy?sslmode=disable"
make test-all           # spins up docker compose, runs everything incl. DB-backed tests
make test-ts            # frontend: tsc --noEmit
go vet ./...            # backend static checks
cd frontend && npx tsc --noEmit && npx eslint   # frontend checks
```

E2E suites (`make e2e*`) drive real AWS and need `E2E_*` env vars — see the Makefile.

---

## Contributing

- **Read [`docs/ai-context/CLAUDE.md`](docs/ai-context/CLAUDE.md) first** — it captures the
  core principles, conventions, and invariants (Claude classifies / Go executes; tenant
  isolation; prompts never in source; migrations are append-only; etc.).
- DB schema changes are **append-only** migration strings at the bottom of the slice in
  [`pkg/models/db.go`](pkg/models/db.go) — never edit a shipped migration.
- After a meaningful change, update the relevant `docs/ai-context/` docs and append to
  `CHANGELOG_AI.md`; record significant decisions as an ADR in `DECISIONS.md`.
- **Secrets:** install the pre-commit hook so commits with live credentials are blocked:
  ```bash
  ln -sf ../../scripts/check-secrets.sh .git/hooks/pre-commit
  ```
  It scans staged changes for AWS keys (`AKIA…`), Anthropic keys (`sk-ant-…`), and Clerk live
  keys (`sk_live_…`) and fails the commit if any are found. Never commit `.env`/`.env.local`
  or `prompts/*`.

---

## Key design decisions

- **BYOC** — the user's app runs in their own AWS account; OpsPilot assumes a scoped,
  revocable IAM role and stores no long-lived credentials. (ADR-001)
- **Intent-first** — Claude classifies intent + diagnoses; Go code executes all infra
  actions and validates every destructive parameter. (ADR-002)
- **Operational events as the AI substrate** — reason over structured events, not raw logs.
  (ADR-003)
- **Shared platform stacks** — one VPC/ECS cluster/ALB per AWS account × region, reused by
  every environment in it (not one stack per environment). (ADR-004)
- **Asynq + WebSocket** — long operations are queued jobs; progress streams back over WS.
  (ADR-005)
- **Trade-secret prompts external; proprietary (BUSL-1.1) licensing.** (ADR-006)

Full decision history: [`docs/ai-context/DECISIONS.md`](docs/ai-context/DECISIONS.md).
