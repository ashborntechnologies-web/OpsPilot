# OpsPilot (ConvDeploy) — AI-Native Conversational Deployment Platform

## Setup

```bash
# 1. Clone and install deps
go mod tidy

# 2. Copy env
cp .env.example .env
# Fill in your values — including the AI prompt sources (see below)

# 3. Start Postgres + Redis
make docker-up

# 4. Run server
make run
```

### AI prompts (trade secret)

The intent-classifier and diagnosis prompts are **not** embedded in the source.
The server refuses to start without them. Point the `*_PROMPT_FILE` variables in
`.env` at local prompt files (the `prompts/` directory is git-ignored), or set
the `*_PROMPT` variables with the inline text. Never commit prompt contents.

### Pre-commit secrets check

Install the bundled hook so commits containing live credentials are blocked:

```bash
ln -sf ../../scripts/check-secrets.sh .git/hooks/pre-commit
```

The hook scans staged changes for AWS access keys (`AKIA...`), Anthropic keys
(`sk-ant-...`), and Clerk live keys (`sk_live_...`) and fails the commit with an
explanation if any are found.

## Project Structure

```
cmd/api/              → entrypoint
internal/
  auth/               → Clerk JWT validation
  github/             → OAuth, repo detection, framework detection
  aws/                → CloudFormation, ECS, ECR, IAM role assumption
  conversation/       → Intent classification via Claude API, routing
  diagnosis/          → Log analysis, root cause, memory layer
  deploy/             → Deploy workflow orchestration
  queue/              → Asynq job handlers
pkg/
  models/             → Postgres schema + types
  ws/                 → WebSocket hub
  middleware/         → Auth, CORS
```

## Key Design Decisions

- **BYOC**: User's app runs in their own AWS account. We assume an IAM role.
- **Intent-first**: Claude classifies intent only. Go code executes all infra actions.
- **One CloudFormation stack per environment** (staging / production)
- **Asynq** handles async deploy jobs. WebSocket streams progress back.
- **Diagnosis engine** pulls CloudWatch logs + deployment diff → Claude → root cause + fix
