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
