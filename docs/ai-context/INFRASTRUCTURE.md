# Infrastructure — OpsPilot

Two infrastructure concerns, kept strictly separate:
1. **Platform infra** — what runs OpsPilot itself (the Go binary, Postgres, Redis).
2. **Tenant infra** — what OpsPilot provisions *in the user's AWS account* to run their app.

## Platform runtime

- **Single Go binary** ([`cmd/api/main.go`](../../cmd/api/main.go)) hosting the HTTP
  API, WebSocket hub, Asynq worker, watchdog scheduler, and monitoring goroutines
  (Poller + LogScanner). There is **no Dockerfile for the platform app** — it runs via
  `make run` / `go run ./cmd/api/main.go` (build: `make build` → `bin/api`).
- **Frontend:** Next.js dev server (`frontend/`, `npm run dev`), proxies `/api/v1/*`
  to the backend; WebSockets connect directly via `NEXT_PUBLIC_API_URL`.

### Docker (local dev only)
[`docker-compose.yml`](../../docker-compose.yml) provides backing services, not the app:
- `postgres:16-alpine` (db `convdeploy`, port 5432, named volume `postgres_data`).
- `redis:7-alpine` (port 6379) — Asynq queue + pending-mutation store.
`make docker-up` / `docker-down`. `make test-all` spins these up for the DB-backed suite.

## Deployment process (platform)
Not yet codified in-repo (no platform Dockerfile/CI manifest committed). To run:
`go mod tidy` → `cp .env.example .env` (fill values incl. prompt file paths) →
`make docker-up` → `make run`. Migrations run automatically at startup
(`models.RunMigrations`). The server **fails fast** without required env or prompts.

## Environment configuration ([`.env.example`](../../.env.example))
| Group | Vars | Notes |
|---|---|---|
| Server | `PORT`, `ENV`, `VERSION` | `ENV!=development` → Gin release mode. |
| Auth | `CLERK_SECRET_KEY`, `CLERK_PUBLISHABLE_KEY` | **required** |
| Data | `DATABASE_URL`, `REDIS_URL` | **required** |
| AI | `ANTHROPIC_API_KEY`, `INTENT_CLASSIFIER_PROMPT[_FILE]`, `DIAGNOSIS_PROMPT[_FILE]` | Prompts **required** (trade secret, git-ignored `prompts/*.txt`); server won't boot without them. |
| AWS | `PLATFORM_AWS_ACCOUNT_ID` | Embedded in bootstrap template; caller ARN auto-detected via STS. |
| GitHub | `GITHUB_CLIENT_ID/SECRET`, `GITHUB_REDIRECT_URL` | OAuth + webhooks. |
| Crypto/feature | `ENCRYPTION_KEY` (**required**, ≥16 chars), `FRONTEND_URL`, `PUBLIC_API_URL`, `ALLOW_PRIVATE_WEBHOOKS` | GitHub-token AES key derived via SHA-256; `PUBLIC_API_URL` needed to register PR webhooks. |
| Admin | `ADMIN_API_KEY` | Optional; gates training-data exports (404 when unset). |
| Email | `SMTP_HOST/PORT/USER/PASS/FROM` | Optional; `notify` is a no-op when unset. |

### Secrets hygiene
`scripts/check-secrets.sh` (install as pre-commit hook) blocks commits containing
`AKIA…`, `sk-ant-…`, `sk_live_…`. Filled `.env` and `prompts/*.txt` are git-ignored.

## External dependencies
Postgres, Redis, Clerk, Anthropic (Claude), GitHub (OAuth + webhooks), SMTP (optional),
and the user's AWS account. See `ARCHITECTURE.md` → External integrations.

## Tenant AWS resources (provisioned in the user's account)

### Trust model — BYOC
The user runs a **bootstrap CloudFormation template** (served at
`/cloudformation/bootstrap-template`, rendered with the platform account ID + a
per-tenant external ID) which creates an IAM role OpsPilot can assume. OpsPilot holds
**no long-lived tenant credentials** — it calls `sts:AssumeRole` per request
(`aws.AssumeRoleForEnvironment` → `ClientBundle`), scoped by the external ID. The user
can revoke by deleting the role.

### Platform stack (shared, once per account × region)
Created by CloudFormation ([`internal/aws/cloudformation.go`](../../internal/aws/cloudformation.go);
`aws.yaml` is the bootstrap reference). Contains the **VPC, subnets, ECS (Fargate)
cluster, Application Load Balancer + listener, and security groups**. Recorded in
`platform_stacks` and reused by every environment in that account+region. An optional
ACM `certificate_arn` adds an **HTTPS (443) listener** and sets `https_enabled` (app
URLs then use `https://`).

### Per-environment project resources
Provisioned per environment (`RunProvisionWorkflow`): **ECR repository, IAM task
execution role, CodeBuild project, CloudWatch log group**. Deploy-time, SDK-created
(not CF): **ALB target group, listener rule, ECS service**.

### Deploy pipeline (in the user's account)
`RunDeployWorkflow`: AssumeRole → **CodeBuild** builds a Docker image (Dockerfile +
buildspec generated per framework; GitHub token passed as an SSM SecureString) → pushes
to **ECR** → register **ECS task definition** (env vars injected) → ensure target group
/ listener rule / **ECS service** → wait for stable → health check → live. Build logs
stream to the UI over WebSocket; lifecycle is recorded as `operational_events`.

### Other AWS usage
- **CloudWatch Logs** — app logs (`FetchRecentECSLogs`), scanned by the LogScanner.
- **CloudWatch metrics** — ALB health (`GetALBMetrics`) for the Poller.
- **SSM** — SecureString params (build-time GitHub token) + **exec sessions** for the
  browser terminal (`StartExecSession`).
- **Cost Explorer** — 30-day cost summary (`GetAccountCostSummary`).
- All created resources are tagged via `internal/awstags`.

### Teardown
Project deletion enqueues `RunDeleteProjectCleanup`: delete ECS services, listener
rules, target groups, purge ECR, remove SSM params, deregister GitHub webhook. The
shared platform stack is **not** torn down on project delete (other projects may use it).
