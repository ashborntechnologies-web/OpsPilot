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
correctness but demand exactly the expertise the developer lacks.

**OpsPilot's bet:** the missing layer is not another control panel — it's an *operator*.
A system that runs the user's infrastructure **in the user's own AWS account** and is
driven by **conversation**, so the interface is intent ("deploy", "what's wrong?")
rather than configuration.

## Target users

- **Primary:** product-focused engineers and small teams who own an AWS account but
  don't want to own the DevOps. They want Heroku ergonomics with AWS ownership.
- **Secondary:** technical founders / solo developers shipping side projects who need
  production-grade deploys without a platform team.
- **Trust profile:** these users will only connect AWS if credentials demonstrably
  never leave their account. Hence BYOC via a revocable, scoped IAM role with a
  per-tenant external ID — the central trust signal of the product.

## The long-term vision: an AI DevOps engineer

OpsPilot is not "ChatGPT in front of a deploy button." The destination is an
autonomous teammate that closes the operational loop:

1. **Detect** — continuously watch infra between deploys (ECS/ALB health poller, log
   anomaly scanner). *(implemented)*
2. **Diagnose** — when something breaks, automatically pull logs + the structured
   event timeline and produce a root cause + suggested fix, using Claude. *(implemented;
   auto-trigger wired via the diagnosis enqueuer)*
3. **Remember** — accumulate per-project memory (recurring failures, confirmed fixes,
   deploy patterns) and feed it back into future diagnoses so the system gets smarter
   about *your* app the longer it runs. *(implemented)*
4. **Advise** — surface risk before a deploy (Friday-afternoon deploy with an open
   alert and a recent failure scores high), summarize alerts in plain English, grade
   deployment health. *(implemented)*
5. **Act** — eventually propose and (with confirmation, staging-first) apply fixes and
   resource changes autonomously. *(partial: resource-change proposals with confirm
   exist; autonomous remediation is future.)*

The **staging-first trust model** is how autonomy earns its way in: the AI proves a
fix on staging before it's allowed near production, and destructive actions always
require explicit confirmation.

## Current product direction

The platform has moved beyond "deploy tool" into **continuous operation**:
monitoring, alerting, memory, risk/health scoring, PR preview environments, cost
intelligence, and a browser terminal into running tasks. The proprietary AI assets
(prompts, training-data pipeline) are treated as the durable moat — diagnosis
feedback is captured as a labeled dataset (`diagnosis_feedback`) and exportable for
model improvement.

## MVP priorities (what must always work)

1. **Connect → Deploy → Live.** GitHub repo + AWS account → first successful
   production deploy with a reachable URL. This is the activation funnel; nothing
   ranks above it.
2. **Conversational core loop:** deploy, rollback, logs, health, scale, diagnose —
   each reachable by chat *and* dashboard button (dashboard is the fallback when AI
   is unavailable).
3. **Trust & safety:** tenant isolation, revocable IAM role, encrypted tokens, no
   secret leakage, destructive actions confirmed.
4. **Honest failure UX:** a failed deploy must explain itself (diagnosis) and offer a
   one-click recovery (rollback / redeploy).
5. **Monitoring that pays for itself:** alerts users actually act on — deduplicated,
   AI-summarized, with email delivery.

## Non-goals (for now)

- Multi-cloud (GCP/Azure). AWS-only.
- Running the user's app in OpsPilot's cloud. BYOC is the model.
- Generic IaC authoring. OpsPilot generates and owns the infra; users don't write CF.
- Embedding/committing the AI prompts. They stay external and proprietary.
