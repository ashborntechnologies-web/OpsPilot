# API Contracts — OpsPilot

Base path `/api/v1`. Routes defined in [`cmd/api/main.go`](../../cmd/api/main.go).
Frontend client: [`frontend/lib/api.ts`](../../frontend/lib/api.ts). Errors are
`{"error": "..."}` with a 4xx/5xx status. Success bodies are JSON (or `204`).

## Auth tiers
- **Public:** no auth.
- **RequireAuth:** Clerk JWT (`Authorization: Bearer <jwt>`); upserts the user + ensures
  a personal org.
- **Project tier:** `/projects/:id/...` runs `LoadProjectMembership` (resolves
  project→org→role, 404 for non-members) then per-route `RequireRole(min)` with the
  hierarchy **admin > engineer > viewer**.
- **Org tier:** `/orgs/:orgId/...` runs `RequireOrgMembership(db, roles...)`.
- **ApiKeyAuth:** admin export routes, static `ADMIN_API_KEY` bearer (404 when unset).
- **WS first-message auth:** WebSocket routes authenticate via `{type:"auth",token}` as
  the first frame; the hub verifies **org membership** (any role).

> **`X-Org-Id` header (active workspace):** non-project routes that read/create tenant
> data (`/projects`, `/aws-accounts`) target the org in this header, defaulting to the
> caller's personal org (`middleware.ActiveOrg`). Project routes derive the org from the
> project itself.
>
> Below, `:id` = project UUID. "Owner tier" = RequireAuth + LoadProjectMembership; the
> **Role** column is the minimum role for that route (— = any member / viewer).
> Primary consumer is the Next.js frontend unless noted.

## Public
| Method | Path | Response | Notes |
|---|---|---|---|
| GET | `/robots.txt` (root, not `/api/v1`) | text | Disallow all — API host shouldn't be crawled. |
| GET | `/health` | `{status}` | DB ping; `503` if unreachable. LB health check. |
| GET | `/api/v1/meta` | `{product,version,license,terms,ip_notice}` | Product/IP metadata. |
| GET | `/api/v1/github/callback` | redirect | GitHub OAuth callback. |
| GET | `/api/v1/cloudformation/bootstrap-template?region=` | `{template,script,external_id}` | BYOC setup template; consumed pre-auth by `/aws-accounts`. |
| POST | `/api/v1/github/webhook` | `200` | GitHub PR events (HMAC-verified) → preview envs. Consumer: GitHub. |
| GET | `/api/v1/slack/callback?code=&state=` | redirect | Slack OAuth callback (trust = HMAC-signed state) → store encrypted bot token. Consumer: Slack. |
| POST | `/api/v1/slack/commands` | ephemeral/in-channel JSON | `/opspilot` slash command (trust = `X-Slack-Signature`). Consumer: Slack. |
| POST | `/api/v1/slack/interactivity` | JSON | Block Kit button actions — deploy/rollback approval (signature-verified). Consumer: Slack. |

## User-scoped (RequireAuth)
| Method | Path | Request | Response | Consumer |
|---|---|---|---|---|
| GET | `/users/me` | — | `UserMe` (plan, usage, project count, notifications) | navbar, settings |
| PATCH | `/users/me/notifications` | `{enabled?,deploy_failed?,deploy_succeeded?,alert_fired?}` | `{message}` | settings |
| POST | `/actions/:actionId/approve` | — | `{message}` | approvals — **engineer+** (checked against the action's org) |
| POST | `/actions/:actionId/reject` | — | `{message}` | approvals — **engineer+** |
| GET | `/github/auth` | — | `{url}` | new-project / accounts |
| GET | `/github/repos` | — | `GithubRepo[]` | new-project |
| GET | `/github/repos/:owner/:repo/branches` | — | `string[]` | new-project |
| GET | `/github/repos/:owner/:repo/detect` | — | `{framework}` | new-project (AI detect) |
| POST | `/projects` | `{name,repo_url,repo_owner,repo_name,framework,branch?,start_command?,account_id?}` | `Project` | new-project (enforces project limit) |
| GET | `/projects` | — | `Project[]` | projects list |
| GET | `/aws-accounts` | — | `AWSAccount[]` (active org) | accounts page |
| POST | `/aws-accounts` | `{label,aws_account_id,iam_role_arn,external_id?,certificate_arn?}` | `AWSAccount` | accounts page — **admin** of active org; validates AssumeRole |
| DELETE | `/aws-accounts/:id` | — | `{message}` | accounts page — **admin** |
| POST | `/aws-accounts/:id/scan` | — | `{job_id, message}` (202) | accounts page — async discovery scan; **engineer+** in the account's org |
| PATCH | `/resources/:resourceId/assign` | `{project_id}` (null unassigns) | `{message, project_id}` | inventory — **engineer+**; project must be in the resource's org |

## Organizations (RequireAuth)
| Method | Path | Request | Response | Auth |
|---|---|---|---|---|
| POST | `/orgs` | `{name, slug?}` | `Organization` (caller becomes admin) | any user |
| GET | `/orgs/me` | — | `Organization[]` (with caller's `role`) | any user |
| GET | `/invites/:token` | — | `{message, organization}` (redeems invite, adds caller) | any user |
| GET | `/orgs/:orgId/members` | — | `OrganizationMember[]` (with email) | member |
| GET | `/orgs/:orgId/resources?resource_type=&region=&project_id=(uuid\|null)` | — | `DiscoveredResource[]` (discovery inventory) | member |
| GET | `/orgs/:orgId/incidents?limit=&offset=` | — | `Incident[]` (open first, then severity, then recency) | member |
| GET | `/orgs/:orgId/actions?status=pending` | — | `AIAction[]` (pending approvals across the org) | member |
| GET | `/orgs/:orgId/slack` | — | `{connected, configured, integration?}` | member |
| GET | `/orgs/:orgId/slack/channels` | — | `SlackChannel[]` (Slack conversations.list) | member |
| GET | `/orgs/:orgId/slack/install` | — | `{url}` (signed-state OAuth URL) | **admin** |
| PATCH | `/orgs/:orgId/slack` | `{alert/deploy/summary_channel_id+name}` | `{message}` | **admin** |
| DELETE | `/orgs/:orgId/slack` | — | `{message}` (disconnect) | **admin** |
| GET | `/orgs/:orgId/analytics?days=30` | — | `{metrics: ReliabilityMetrics, breakdown: ProjectReliabilityRow[]}` (org-wide leadership dashboard) | member |
| GET | `/orgs/:orgId/reports` | — | `DailySummaryRecord[]` (monthly health reports, `is_monthly=true`) | member |
| POST | `/orgs/:orgId/reports/generate?month=YYYY-MM` | — | `MonthlyReport` (generate + email admins; month defaults to current) | **admin** |
| GET | `/orgs/:orgId/summaries?limit=30` | — | `DailySummaryRecord[]` (daily only, `is_monthly=false`) | member |
| GET | `/orgs/:orgId/summaries/latest` | — | `{summary}` (most recent or null) | member |
| POST | `/orgs/:orgId/summaries/generate` | — | `DailySummary` (generate + deliver today; testing) | **admin** |
| PATCH | `/orgs/:orgId/summary-config` | `{summary_time?,summary_timezone?,summary_enabled?}` | `{message}` | **admin** |

## Incident war room (RequireAuth; org membership + role checked per handler)
`:incidentId` resolves its own org. Reads need any member; mutations need engineer+.
| Method | Path | Request | Response | Role |
|---|---|---|---|---|
| GET | `/incidents/:incidentId` | — | `{incident, timeline, actions}` | member |
| POST | `/incidents/:incidentId/timeline` | `{content, entry_type?}` | `IncidentTimelineEntry` (broadcast to war room) | engineer+ |
| POST | `/incidents/:incidentId/acknowledge` | — | `{status:"investigating"}` | engineer+ |
| POST | `/incidents/:incidentId/resolve` | — | `{status:"resolved", postmortem_generating:true}` (enqueues async generation — ADR-014) | engineer+ |
| GET | `/incidents/:incidentId/postmortem` | — | `Postmortem`, or `404 {generating:bool}` while still generating | member |
| POST | `/incidents/:incidentId/actions/:actionId/approve` | — | `{status}` | engineer+ |
| POST | `/incidents/:incidentId/actions/:actionId/reject` | — | `{status}` | engineer+ |
| GET (project) | `/projects/:id/incidents` | — | `Incident[]` | member (project tier) |

## Postmortems (RequireAuth; org membership + role checked per handler)
`:postmortemId` resolves its own org. Generation is async (ADR-014); poll the incident
endpoint above until it returns a `Postmortem`.
| Method | Path | Request | Response | Role |
|---|---|---|---|---|
| GET | `/postmortems/:postmortemId` | — | `Postmortem` (for the editor) | member |
| PATCH | `/postmortems/:postmortemId` | `{content_markdown?, title?, action_items?}` | `{message}` | engineer+ |
| POST | `/postmortems/:postmortemId/publish` | — | `{message, status:"published"}` | engineer+ |
| GET | `/postmortems/:postmortemId/export?format=md\|pdf` | — | markdown attachment, or print-ready HTML (`pdf`) | member |
| GET | `/orgs/:orgId/postmortems` | `?project_id=&severity=&from=&to=&q=` | `Postmortem[]` (published only — the org library) | member |
| POST | `/orgs/:orgId/invites` | `{email, role}` | `{invite, accept_url, email_sent}` | **admin** |
| PATCH | `/orgs/:orgId/members/:userId` | `{role}` | `{message, role}` | **admin** (can't demote last admin) |
| DELETE | `/orgs/:orgId/members/:userId` | — | `{message}` | **admin** (can't remove last admin) |

## Project-scoped (Owner tier — under `/projects/:id`)
**Roles:** GET routes require any membership (viewer+). **engineer** covers deploy,
rollback, redeploy, cancel, delete-deployment, scale, env-var write/delete/reveal, alert
snooze/resolve, diagnose feedback, conversation, webhooks, previews. **admin** covers
`POST /environments`, `retry-provision`, `DELETE ""` (project), and `PATCH ""`
(settings). Viewers get 403 on any of these; the chat WS also blocks viewer action
intents.

| Method | Path | Request | Response |
|---|---|---|---|
| GET | `` | — | `Project` |
| PATCH | `` *(admin)* | `{name?,branch?,start_command?,framework?}` | `Project` |
| DELETE | `` *(admin)* | — | `{message}` (async AWS cleanup) |
| POST | `/environments` *(admin)* | `{name,aws_region}` | `Environment` (auto-enqueues provision) |
| GET | `/environments` | — | `Environment[]` |
| POST | `/environments/:envId/retry-provision` | — | `{message}` |
| GET | `/environments/:envId/logs?lines=` | — | `{lines,log_group}` |
| GET/PUT/DELETE | `/environments/:envId/env-vars[/:varId]` | `{key,value,is_secret}` | `EnvVar` / `{message}` |
| GET | `/environments/:envId/env-vars/:varId/reveal` | — | `{value}` (secret plaintext) |
| GET | `/environments/:envId/health` | — | `{status,running,desired,pending,url?}` |
| POST | `/environments/:envId/scale` | `{replicas}` | `{message}` |
| POST | `/environments/:envId/deploy?env=` | — | `{message}` (rate-limited; enqueues; broadcasts `deploy_risk`) |
| GET | `/deployments` | — | `Deployment[]` |
| POST | `/deployments/:deployId/rollback` | — | `{message}` |
| POST | `/deployments/:deployId/redeploy` | — | `{message,deployment}` |
| DELETE | `/deployments/:deployId` | — | `{message}` |
| POST | `/deployments/:deployId/cancel` | — | `{message}` (stops CodeBuild) |
| GET | `/deployments/:deployId/events` | — | `OperationalEvent[]` |
| GET | `/deployments/:deployId/diagnose` | — | `{diagnosis}` (AI root cause) |
| POST | `/deployments/:deployId/diagnose/feedback` | `{rating,fixed_issue,notes?}` | `{feedback,score}` |
| GET | `/diagnose/feedback-summary` | — | aggregate feedback stats |
| GET | `/events?limit=` | — | `OperationalEvent[]` (activity feed) |
| GET | `/alerts?status=open\|resolved\|all&limit=` | — | `Alert[]` |
| POST | `/alerts/:alertId/snooze` | `{duration_minutes}` | `{message,snoozed_until}` |
| POST | `/alerts/:alertId/resolve` | — | `{message}` |
| GET | `/costs` | — | `CostSummary` (30-day, Cost Explorer) |
| GET | `/health-score` | — | `HealthScore{score,grade,components,insights}` |
| GET | `/analytics?days=30` | — | `{metrics: ReliabilityMetrics}` (project reliability) |
| GET | `/uptime?days=90` | — | `{uptime: UptimePoint[], days}` (daily uptime for charts) |
| GET | `/environments/:envId/sla` | — | `ServiceSLA` (default 99.9% when unset) |
| PUT | `/environments/:envId/sla` *(engineer+)* | `{target_uptime_pct, measurement_window_days?}` | `{message}` |
| GET | `/resources` | — | `DiscoveredResource[]` (assigned to this project; managed + discovered) |
| GET | `/actions?limit=50` | — | `AIAction[]` (action history) |
| GET | `/environments/:envId/trust` | — | `{trust_level, autonomous_boundaries}` |
| PATCH | `/environments/:envId/trust` *(admin)* | `{trust_level?, autonomous_boundaries?}` | `{message}` |
| POST | `/previews/enable` | — | `{message,webhook_id}` (installs GitHub webhook) |
| POST | `/previews/disable` | — | `{message}` |
| GET/POST/PATCH/DELETE | `/webhooks[/:webhookId]` | `{url,secret?,events,active?}` | `Webhook` / `{message}` |
| POST | `/conversation` | `{message}` | `{response}` (rate-limited; REST fallback for chat) |
| GET | `/conversation/history` | — | `ConversationMessage[]` |

## WebSocket
| Path | Auth | Purpose |
|---|---|---|
| `/api/v1/ws/:projectId` | first-message token + ownership | Chat + live deploy/provision/alert/risk/build-log stream. Server→client `WsMessage{type,payload}`; client→server `{message}`. |
| `/api/v1/ws/:projectId/terminal/:envId` | first-message token + ownership | Binary SSM exec datachannel ↔ xterm. |
| `/api/v1/ws/incidents/:incidentId` | first-message token + incident-org membership | War-room broadcast: `incident_timeline`, `incident_update`, `incident_action`. Broadcast-only (updates posted via HTTP). |

**`WsMessage.type` values:** `auth_ok`, `thinking`, `response`, `deploy_progress`,
`deploy_done`, `deploy_failed`, `provision_progress`, `provision_done`,
`provision_failed`, `alert`, `alert_resolved`, `deploy_risk`, `build_log`, `runtime_event`,
`incident_timeline`, `incident_update`, `incident_action`, `action_proposed`,
`action_updated`, `error`. (`payload` is a string; alert/risk/incident/action payloads are
JSON strings.)

## Admin (ApiKeyAuth — trade-secret datasets)
| Method | Path | Response |
|---|---|---|
| GET | `/api/v1/admin/export/intents?since=` | JSONL intent training rows (from `conversations`) |
| GET | `/api/v1/admin/export/diagnoses?since=` | JSONL diagnosis training rows (from `diagnosis_feedback`) |

## Rate limits
- `/conversation`: 10 req/min (burst 5) — each hits Claude.
- `/environments/:envId/deploy`: 5 req/min (burst 2) — each triggers CodeBuild.
Per-user token bucket (`pkg/middleware/ratelimit.go`).
