# ConvDeploy — AI-Native Conversational Deployment Platform

## Setup

```bash
# 1. Clone and install deps
go mod tidy

# 2. Copy env
cp .env.example .env
# Fill in your values

# 3. Start Postgres + Redis
make docker-up

# 4. Run server
make run
```

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
