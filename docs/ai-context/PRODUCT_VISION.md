# Product Vision — OpsPilot

## The problem

Shipping and operating a web app on AWS is still gated by DevOps expertise. A
developer who can write the app often cannot, alone:

- stand up a VPC + ECS cluster + ALB + ECR + IAM + CodeBuild correctly,
- wire CI to build a container and roll it out with zero downtime,
- diagnose *why* a deploy failed or a service is crash-looping from raw CloudWatch logs,
- decide when to roll back, scale, or resize compute.

Existing PaaS products (Heroku/Render/Railway/Vercel) solve the first part by running
the app in *their* cloud — which means handing over data, billing, and trust, and
hitting walls on compliance and cost. Infra-as-code tools (Terraform/CDK) solve
correctness but demand exactly the expertise the developer lacks. Observability and
on-call tools (Datadog/PagerDuty) tell a team *that* something is wrong; they don't
diagnose *why*, and they don't act.

**OpsPilot's bet:** the missing layer is not another control panel — it's an
*operator*. A system that runs and watches infrastructure **in the customer's own
AWS account**, reasons over logs/metrics/deploys/code changes the way a senior
engineer would, and is driven by **conversation** — so the interface is intent
("deploy", "what's wrong?") rather than configuration.

## Target users — two populations, not one

Earlier versions of this document described a single target user (a small team that
doesn't want to own DevOps). That's still true, but it's only half the market, and
treating it as the whole market caps growth artificially: most companies past the
earliest stage already have production infrastructure they are not going to migrate
to try a new tool. OpsPilot now explicitly serves two populations, and the product is
designed as **one trust model with two entry points**, not two products.

- **Population A — building new.** Solo founders and small teams (2–10 engineers)
  standing up new applications. No existing AWS footprint worth protecting, no
  dedicated DevOps engineer, no migration risk to weigh. They want Heroku ergonomics
  with AWS ownership, and they're willing to let OpsPilot operate from day one because
  there's nothing yet for it to break.
- **Population B — already running.** Engineering teams (typically 10+ engineers,
  often with an existing platform/DevOps function) who already have production
  infrastructure, CI/CD, IaC, and monitoring on AWS. They will not migrate. They will
  not hand an unproven AI write access to production. They *will* let a new tool watch
  read-only if it's clearly not at risk of breaking anything and clearly useful from
  day one.
- **Trust profile (both populations):** credentials must demonstrably never leave the
  customer's AWS account. BYOC via a revocable, scoped IAM role with a per-tenant
  external ID is the central trust signal of the product, and it's what makes serving
  Population B possible at all — without a revocable, auditable, scoped role, no
  serious engineering org would connect their account in the first place.

## The long-term vision: an AI DevOps engineer that earns its access

OpsPilot is not "ChatGPT in front of a deploy button." The destination is an
autonomous teammate that closes the full operational loop — detect, diagnose,
remember, advise, and eventually act — for *any* AWS infrastructure, not just
infrastructure OpsPilot itself provisioned.

1. **Detect** — continuously watch infra (ECS/ALB health poller, log anomaly
   scanner), including infrastructure OpsPilot did not create. *(implemented for
   OpsPilot-managed resources; discovery of existing/unmanaged resources is the
   Population B entry point — see below.)*
2. **Diagnose** — when something breaks, automatically pull logs + the structured
   event timeline + project memory and produce a root cause, a confidence score, and
   structured evidence, using Claude. *(implemented)*
3. **Remember** — accumulate per-project memory (recurring failures, confirmed fixes,
   deploy patterns) and feed it back into future diagnoses so the system gets smarter
   about *this specific* infrastructure the longer it runs, for either population.
   *(implemented)*
4. **Advise** — surface risk before a deploy, summarize alerts in plain English with
   evidence, grade deployment and infrastructure health. *(implemented for deploys
   OpsPilot performs; advisory-only recommendations against existing infrastructure —
   e.g. generating a reviewable Terraform diff — is future scope, see Capability Tier
   1 below.)*
5. **Act** — propose and, within explicit boundaries, execute fixes and resource
   changes. *(partial — see the trust ladder below; this is the dimension that
   differs most between the two populations and is never granted by default.)*

The mechanism that governs step 5 — for both populations — is the **trust ladder**.

## The trust ladder

Access to act is never assumed; it is earned, explicit, and revocable at any time.
The ladder is the same five tiers regardless of which population a customer entered
through. What differs is *where each population starts* and *how fast they're
expected to move*.

| Tier | Name | What OpsPilot can do | Default for Population A | Default for Population B |
|---|---|---|---|---|
| 0 | **Observe** | Read-only: discover, monitor, diagnose, recommend. Cannot act. | N/A — A starts at Tier 2 because OpsPilot provisioned the infra itself | **Starting tier.** Connect AWS/repos/monitoring read-only, no write access at all |
| 1 | **Recommend (AI SRE)** | Generates reviewable artifacts — Terraform/CloudFormation diffs, alarm definitions, runbooks — for a human to apply elsewhere | N/A, see note below | Earned after a defined trust-building period (see *Promotion mechanics*) |
| 2 | **Execute with approval (AI DevOps Engineer)** | Proposes a specific action (deploy, rollback, scale, resource change) and executes it only after explicit human approval | **Starting tier** — this is what "OpsPilot deploys your app" already means | Earned after Tier 1 recommendations are consistently accepted |
| 3 | **Execute autonomously (within bounds)** | Executes a defined, narrow action set without per-action approval (e.g. "can scale 1–5 replicas, cannot touch production CPU/memory") | Earned per-environment, never default — see *Population A still climbs* below | Earned per-environment, after a track record at Tier 2 |
| 4 | **Architect** | Recommends architectural changes, cost/reliability optimizations, and long-term platform direction based on accumulated operational knowledge | Future scope for both populations | Future scope for both populations |

This table already exists partially in the codebase as **environment trust levels**
(`suggest` / `supervised` / `autonomous` on the `environments` table, enforced via
`trust.ProposeAction`). That implementation maps to Tiers 0/2/3 of this ladder. Tier 1
(generating IaC artifacts for external review) and Tier 4 (architectural advice) are
**not yet built** and are explicitly future scope — see Non-goals.

### Why Population A doesn't "skip" the ladder

Population A customers let OpsPilot execute deploys from day one, which looks like
starting at Tier 2 immediately. That's correct — there's no existing production to
protect, so the risk calculus is different. But **Tier 3 (autonomous execution) is
never a Population A default either.** A founder trusting OpsPilot to deploy their
app on day one is not the same as trusting it to make unsupervised production changes
six months later without ever being asked. Tier 3 is earned per-environment, by
track record, for both populations — the starting point differs, the top of the
ladder does not move.

### Promotion mechanics (open design question — flagged, not yet decided)

"Trust is established over time" is not a mechanism on its own; it needs concrete,
visible triggers or this tier never actually advances past Tier 0/2 for anyone. The
direction we intend to build, **not yet implemented**:

- Promotion is **milestone-triggered and admin-confirmed**, never automatic and
  silent. OpsPilot should not promote itself.
- OpsPilot tracks its own track record per environment (diagnoses made, confidence
  scores, human accept/reject decisions on proposed actions — `ai_actions` and
  `diagnosis_feedback` already capture the raw data for this).
- When a track-record threshold is crossed (e.g. "12 correctly-diagnosed incidents
  over 60 days, zero false positives," exact thresholds TBD), OpsPilot **proactively
  asks** the org admin for the next tier, stating the evidence: *"I've correctly
  diagnosed 12 incidents over the last 60 days with no false positives. Would you
  like to grant me permission to generate Terraform changes for review?"*
- The admin accepts, declines, or asks for more time. The decision and the evidence
  behind it are recorded in the audit trail (`ai_actions`, environment trust level
  history).
- Promotion is **per capability, per environment** — accepting Tier 1 for staging
  does not imply Tier 1 for production, and accepting "can scale" does not imply
  "can change resource sizing."

This needs to be designed concretely (exact thresholds, the proposal UI, what
"correctly diagnosed" means operationally) before it's built. It is the single most
important open mechanism in this document — without it, "trust is earned over time"
is a slogan, not a feature.

## The two onboarding flows

### Flow A — New infrastructure ("deploy with me")

1. Connect GitHub repo.
2. OpsPilot provisions AWS infrastructure (VPC/ECS/ALB/ECR/CodeBuild via the existing
   BYOC bootstrap + per-tenant IAM role).
3. First deploy, live URL, continuous monitoring from that moment forward.
4. Capability grows over time (Tier 3 per environment, eventually Tier 4) without the
   user ever re-onboarding — it's the same product, just more capable as trust is
   granted.

This is the flow that exists today. *(implemented)*

### Flow B — Existing infrastructure ("watch me, then earn it")

1. Connect AWS account(s), source repos, and (future) existing monitoring — explicitly
   **read-only**. No bootstrap stack is required to provision anything; the IAM role
   requested for this flow should be describe/list/read permissions only, distinct
   from the read-write role Flow A requires.
2. OpsPilot runs infrastructure discovery: enumerates existing resources (ECS
   services, RDS, ElastiCache, Lambda, S3, ALBs, SQS — scanners already exist in
   `internal/discovery`), maps dependencies, and starts monitoring what it finds
   without changing anything.
3. OpsPilot operates at **Tier 0 only**: diagnosis, alerting, root-cause analysis,
   architectural understanding — all advisory, nothing written back to AWS.
4. Over the defined trust-building period (see *Promotion mechanics*), OpsPilot earns
   Tier 1 (generates reviewable IaC/alarm/runbook artifacts), then Tier 2 (executes
   specific approved actions), then potentially Tier 3 per environment.

**Flow B's onboarding read-only IAM role and its discovery-to-Tier-1 promotion path
are not yet fully built.** Discovery scanning exists; the explicit "read-only by
construction" role separation, and the Tier 1 IaC-generation capability, are scoped
future work (see Non-goals).

### Why this is one product, not two

Both flows terminate in the same five-tier ladder. Flow A starts mid-ladder because
there's nothing yet to protect; Flow B starts at the bottom because there's everything
to protect. A Flow A customer who grows from 2 engineers to 50 doesn't need to
"graduate" to some enterprise product — they're already inside the same trust
mechanism Flow B customers climb. This is what makes the dual entry point a single
coherent system rather than market segmentation dressed up as strategy.

## Current product direction

The platform has moved beyond "deploy tool" into **continuous operation** for
infrastructure it manages: monitoring, alerting, memory, risk/health scoring, PR
preview environments, cost intelligence, incident war rooms, postmortems, a Slack
integration, and a browser terminal into running tasks. Team workspaces and
role-based access exist, so this is usable by a real engineering team, not just a
solo user. The proprietary AI assets (prompts, training-data pipeline) are treated as
the durable moat — diagnosis feedback is captured as a labeled dataset
(`diagnosis_feedback`) and exportable for model improvement.

The next major direction is **Flow B**: discovery already exists; what's missing is
the read-only-by-construction onboarding path, the Tier 0→1 promotion mechanism, and
eventually Tier 1 IaC generation. This is the highest-leverage unsolved problem in the
product — it's what turns "tool startups can adopt" into "platform any engineering
org can adopt without migration risk."

## MVP priorities (what must always work)

1. **Connect → Deploy → Live (Flow A).** GitHub repo + AWS account → first successful
   production deploy with a reachable URL. This is the activation funnel for
   Population A; nothing ranks above it for that population.
2. **Connect → Discover → Diagnose (Flow B).** AWS account (read-only) → infrastructure
   mapped → first useful, accurate diagnosis with zero write access used. This is the
   activation funnel for Population B; it must work with zero risk of ever touching
   production, or the entire pitch to this population collapses.
3. **Conversational core loop:** deploy, rollback, logs, health, scale, diagnose —
   each reachable by chat *and* dashboard button (dashboard is the fallback when AI
   is unavailable). Applies fully to Flow A; applies to the read-only subset (logs,
   health, diagnose) for Flow B until a customer is promoted past Tier 0.
4. **Trust & safety:** tenant isolation, revocable IAM role, encrypted tokens, no
   secret leakage, destructive actions confirmed, every tier promotion explicit and
   auditable.
5. **Honest failure UX:** a failed deploy or detected incident must explain itself
   (diagnosis with confidence + evidence) and offer a clear next step (one-click
   recovery where OpsPilot has execute access, a clear recommendation where it
   doesn't).
6. **Monitoring that pays for itself:** alerts users actually act on — deduplicated,
   AI-summarized with evidence, delivered where the team already works (email, Slack).

## Non-goals (for now)

- Multi-cloud (GCP/Azure). AWS-only.
- Running the user's app in OpsPilot's cloud. BYOC is the model, for both flows.
- **Tier 1 IaC generation** (Terraform/CloudFormation diffs for external review) —
  real, scoped future work, not yet sized. Do not assume this exists when planning
  near-term features.
- **Tier 4 architectural advisory** — future scope, depends on Tier 1–3 being mature
  and on accumulated operational data that doesn't exist yet.
- **Automatic/silent tier promotion** — promotion is always explicit and
  admin-confirmed. Do not build anything that raises an environment's trust tier
  without a human decision in the loop.
- Embedding/committing the AI prompts. They stay external and proprietary.