# OpsPilot — AI Context: Primary Instructions

> **Read this entire `docs/ai-context/` directory before making any change.** These
> are living documents derived from the actual implementation. If code and docs
> conflict, the code is the source of truth — update the docs to match (unless told
> otherwise). Never let documentation drift.

Codename: **ConvDeploy** (repo/module name). Product name: **OpsPilot**.
Module path: `github.com/ashborntechnologies-web/OpsPilot`.

---

## What OpsPilot is

An **AI-native conversational deployment platform**. A developer connects a GitHub
repo and their own AWS account, then deploys and operates real infrastructure by
talking to it ("deploy to production", "why did the last deploy fail?", "scale to 3").

Two properties define the system:

1. **BYOC (Bring Your Own Cloud).** The user's app runs in *their* AWS account.
   OpsPilot never holds long-lived AWS credentials — it assumes a scoped IAM role
   the user controls and can revoke, gated by a per-tenant STS external ID.
2. **Intent-first AI.** Claude is used for **classification and diagnosis only**.
   It never touches AWS. Go code executes every infrastructure action. The LLM
   converts natural language → a structured intent; a Go `switch` routes it to a
   real workflow. This is the single most important architectural invariant.

## Where it's going

OpsPilot is becoming an **autonomous AI DevOps engineer**: it already monitors
infrastructure between deploys (health poller + log scanner), turns anomalies into
AI-summarized alerts, auto-triggers diagnosis on runtime failures, and accumulates
**long-term per-project memory** that makes its diagnoses smarter over time. The
direction is: detect → diagnose → propose fix → (eventually) act, with staging-first
trust. See `PRODUCT_VISION.md` and `ROADMAP.md`.

---

## Core engineering principles

- **Claude classifies, Go executes.** Never let the LLM call AWS, mutate the DB, or
  decide a destructive action's parameters. If a workflow needs a number (replica
  count, CPU/memory), the LLM extracts it but Go validates and confirms it. Never
  guess a destructive parameter (see `conversation.ProcessMessage` scale handling).
- **Reason over operational events, not raw logs.** Every meaningful state
  transition is written to `operational_events` as a structured row. Diagnosis,
  alerts, health scores, and risk scores are computed from these events. Prefer
  emitting an event to parsing a log line.
- **Tenant isolation is non-negotiable.** Every `/projects/:id/...` route sits behind
  `RequireProjectOwnership`. New handlers under that group must NOT re-check ownership
  (it's already guaranteed) but must NEVER be mounted outside it. Cross-tenant access
  by guessing a UUID must be impossible. WebSocket subscriptions re-verify ownership.
- **Trade-secret prompts never enter source.** The intent-classifier and diagnosis
  prompts load from env/file at startup (`internal/prompts`); the server refuses to
  boot without them. Never inline prompt text, never commit `prompts/*.txt`.
- **Degrade, don't break.** AI outage → fall back to dashboard buttons + helpful
  message. SMTP unset → `notify` becomes a logging no-op. Metering failure → don't
  block the user. Every external dependency has a graceful-degradation path.
- **Async work goes through Asynq; progress comes back over WebSocket.** Deploys,
  provisions, rollbacks, project deletion, and diagnosis are queue jobs. The HTTP
  handler enqueues and returns immediately; the worker streams progress via the WS hub.

## Coding conventions & patterns

- **Go services** are structs named `Service` with a `NewService(...)` constructor,
  one per `internal/<domain>/` package. Optional collaborators are injected via
  `Set*` methods after construction (e.g. `deploySvc.SetMemoryService`) to break
  init cycles. Wiring happens exactly once in `cmd/api/main.go`.
- **HTTP handlers** are methods named `Handle<Verb><Noun>` returning via Gin. They
  validate input, call a context-taking business method, and map errors to JSON
  `{"error": "..."}`. The business logic method (no `c *gin.Context`) is reusable by
  the conversation engine and queue workers.
- **Interfaces for seams:** `aws.AWSProvider`, `github.GitHubProvider`, `deploy.Enqueuer`,
  `events.AlertEvaluator`, `events.DiagnosisEnqueuer`. Mocks live in `internal/testutil`.
- **DB access** is raw `pgx` (no ORM). Schema is a list of idempotent migration
  strings in `pkg/models/db.go` run at startup — **append only, never edit a shipped
  migration**. Add new ones to the bottom of the `migrations` slice.
- **Frontend** is Next.js App Router (`frontend/`). Note `frontend/AGENTS.md`: this is
  **not** stock Next.js — read `node_modules/next/dist/docs/` before writing Next code.
  Reference files with markdown links, not backticks (IDE convention).
- **Secrets in JSON:** struct fields holding secrets use `json:"-"` (e.g.
  `GithubToken`, `ExternalID`, webhook `Secret`, `github_webhook_secret`). Secret env
  vars are redacted in list responses and only revealed via a dedicated endpoint.

## Important constraints

- Module is licensed **BUSL-1.1** and self-identifies as proprietary (the
  `Proprietary()` middleware stamps headers; `/api/v1/meta` carries an IP notice).
  Training-data export endpoints are trade-secret datasets behind `ADMIN_API_KEY`.
- Server fails fast at startup if required env is missing (`validateEnv`) or prompts
  are unconfigured (`prompts.MustLoad`). Required: `DATABASE_URL`, `REDIS_URL`,
  `CLERK_SECRET_KEY`, `CLERK_PUBLISHABLE_KEY`, `ENCRYPTION_KEY`.
- GitHub tokens are AES-encrypted at rest with a key derived (SHA-256) from
  `ENCRYPTION_KEY`. Outbound webhooks are SSRF-guarded (no private/loopback targets
  unless `ALLOW_PRIVATE_WEBHOOKS=true`).
- One **platform stack** (VPC/ECS cluster/ALB) is shared per AWS account × region;
  many project environments reuse it. Don't provision per-project VPCs.

## How to approach changes here

1. **Read `docs/ai-context/` fully.** Validate the change against `ARCHITECTURE.md`
   and `PRODUCT_VISION.md` before writing code.
2. **Find the seam.** Most features touch one `internal/<domain>` package + a route in
   `main.go` + a frontend API call. New async work = a new Asynq task + handler.
3. **Verify claims against code, not memory.** This codebase changes fast; a review or
   doc may be stale. Grep/read before asserting something is missing or broken.
4. **After the change:** update the relevant AI-context docs and append to
   `CHANGELOG_AI.md`. If you introduced a major pattern or subsystem, add an ADR to
   `DECISIONS.md` and update this file with any new permanent instruction.
5. **Tests:** unit tests need no deps (`make test-unit`); DB-backed tests want
   `TEST_DATABASE_URL` (`make test-integration`). Build with `go build ./cmd/api/`.

## Document map

| File | Purpose |
|------|---------|
| `CLAUDE.md` | This file — permanent instructions, principles, conventions. |
| `PRODUCT_VISION.md` | Problem, users, the AI-DevOps-engineer vision, MVP priorities. |
| `ARCHITECTURE.md` | System/backend/frontend architecture, all flows, diagrams. |
| `BACKEND.md` | Per-module reference: purpose, files, structs, services, deps. |
| `FRONTEND.md` | Routing, components, state, API + WebSocket usage, workflows. |
| `DATABASE_SCHEMA.md` | Every table, relationship, field purpose, data lifecycle. |
| `API_CONTRACTS.md` | Every endpoint: method, auth, request/response, consumers. |
| `INFRASTRUCTURE.md` | Docker, deploy process, env config, AWS resources. |
| `CURRENT_STATE.md` | What works / partial / experimental / known limits. |
| `ROADMAP.md` | Implemented / in-progress / future, by horizon. |
| `DECISIONS.md` | Architectural Decision Records. |
| `CHANGELOG_AI.md` | Running log of meaningful changes + assumption shifts. |
