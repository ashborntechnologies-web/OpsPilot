# Frontend Reference — OpsPilot

Next.js (App Router) in `frontend/`. **Not stock Next.js** — read
`frontend/AGENTS.md`: APIs/conventions may differ from training data; consult
`node_modules/next/dist/docs/` before writing Next code. Stack: Clerk (auth), SWR
(data fetching), raw `WebSocket` (live streams), Tailwind + shadcn-style primitives
(`components/ui/*`), `xterm.js` (terminal), `sonner` (toasts), `lucide-react` (icons).

> IDE convention: reference code with markdown links `[file](path)`, not backticks.

---

## Routing (`frontend/app`)

| Route | File | Purpose | Auth |
|---|---|---|---|
| `/` | `page.tsx` | Marketing landing (how it works, trust, pricing). | public |
| `/sign-in`, `/sign-up` | `sign-in/[[...sign-in]]`, `sign-up/[[...sign-up]]` | Clerk auth screens. | public |
| `/projects` | `projects/page.tsx` | Project list + delete. Empty-state CTA. | required |
| `/projects/new` | `projects/new/page.tsx` | 3-step create wizard (repo → framework → AWS account). | required |
| `/projects/[id]` | `projects/[id]/page.tsx` | **Main dashboard** (~2260 lines): Overview, Live Logs, Deployments, Env Vars, Terminal, Webhooks, Costs, Settings tabs + live status sidebar + alerts panel. | required + ownership |
| `/projects/[id]/chat` | `projects/[id]/chat/page.tsx` | Conversational interface (WebSocket). | required + ownership |
| `/aws-accounts` | `aws-accounts/page.tsx` | Connect/list/delete AWS accounts (bootstrap CloudFormation flow). | required |
| `/privacy`, `/terms` | `privacy/page.tsx`, `terms/page.tsx` | Legal. | public |

`app/layout.tsx` wraps everything in the Clerk provider + toaster. `error.tsx` /
`global-error.tsx` are error boundaries.

---

## Components

- **`components/layout/navbar.tsx`** — nav + usage meter pill for free-plan users
  (fetches `getMe`, shows `N/limit AI actions`, modal with pricing tiers). `footer.tsx`.
- **`components/project/status-sidebar.tsx`** — left rail (desktop `xl:`): environments,
  recent deployments, alert count. Collapses into a drawer on tablet/mobile.
- **`components/project/alerts-panel.tsx`** — open alerts list with snooze/resolve.
- **`components/project/connect-aws-modal.tsx`** — AWS account connection (bootstrap
  template + role ARN + optional ACM cert).
- **`components/ui/*`** — shadcn-style primitives (button, card, dialog, tabs, input,
  badge, avatar, scroll-area, select, separator, textarea, sonner).

---

## State management

- **Server data:** SWR. The dashboard uses keyed `useSWR` for project/envs/deployments
  (`refresh`), open alerts (30s refresh), and health score (60s refresh). A 5s polling
  fallback runs while a deploy/provision is in flight so status flips even if a WS
  message is missed.
- **Local UI state:** `useState` per tab/dialog. Live streams (`provisionLog`,
  `deployLog`, `buildLogs`, `alerts`, `currentRiskScore`) are appended from WS messages.
- **Auth tokens:** `useAuth().getToken()` (Clerk); refreshed ~every 45s. Tokens are
  kept in a ref for the WS so periodic refresh doesn't churn the socket.

---

## API interactions (`frontend/lib/api.ts`)

- All HTTP via a typed `request<T>(path, token, opts)` helper hitting **relative**
  `/api/v1/*` (Next rewrites to the backend — no CORS). Bearer token from Clerk.
  Errors surface the backend `{error}` string; a non-JSON body implies the API is down.
- Covers every endpoint: projects, environments, AWS accounts, deployments
  (deploy/rollback/redeploy/delete/cancel/events/diagnose), GitHub (repos/branches/
  detect/auth), env vars (+reveal), conversation (+history), webhooks, costs, health
  score, previews, alerts (list/snooze/resolve), `getMe`/`updateNotificationPrefs`,
  diagnosis feedback, bootstrap template. See `API_CONTRACTS.md`.
- Types live in `frontend/types/api.ts` (mirrors `pkg/models/types.go`): `Project`,
  `Environment`, `Deployment`, `Alert`, `RiskScore`, `UserMe`, `WsMessage`,
  `OperationalEvent`, etc.

---

## WebSocket communication

Two hooks/usages, both authenticating via a **first message** (token never in URL):

1. **`lib/use-ws.ts` (`useProjectWS`)** — powers the chat page. Sends
   `{type:"auth", token}` on open; sets `connected` on `auth_ok`. Handles message
   types: `thinking`, `response`, `deploy_progress`/`provision_progress`,
   `deploy_done`/`provision_done`/`*_failed`, `alert`, `alert_resolved`, `deploy_risk`,
   `build_log`, `error`. Exponential-backoff reconnect (≤15s) that survives token
   refresh and backend restarts. `send(message)` posts `{message}`.
2. **Inline WS in the dashboard** (`projects/[id]/page.tsx`) — same auth handshake;
   accumulates provision/deploy logs, build output (collapsible auto-scroll terminal),
   live alerts (+mobile banner), and the pre-deploy risk banner.
3. **Terminal WS** (`terminalWsURL`) — binary datachannel proxied to `xterm.js`.

WS URLs (`wsURL`, `terminalWsURL`) target the backend directly via
`NEXT_PUBLIC_API_URL` (WebSockets can't traverse the Next proxy).

---

## User workflows

- **Onboard:** sign in → `/aws-accounts` (run bootstrap CloudFormation, paste role
  ARN) → `/projects/new` (pick repo, detected framework, account) → create.
- **Deploy:** dashboard Overview → Deploy (confirm) → live stage checklist + progress
  bar + streamed build output; or chat "deploy to production". Risk banner appears if
  the pre-deploy score is high/critical.
- **Operate via chat:** rollback, logs, health, scale, diagnose, costs, resize compute
  (resource change is proposed then confirmed). Status pill summarizes infra health.
- **Recover from failure:** failed deploy → diagnosis dialog (root cause + fix), then
  rollback/redeploy. Diagnoses can be rated (👍 fixed it / helpful / not helpful) —
  feeds the training dataset and project memory.
- **Monitor:** status sidebar + alerts panel show live alerts (snooze/resolve);
  email on alert/deploy per notification prefs (Settings tab).
