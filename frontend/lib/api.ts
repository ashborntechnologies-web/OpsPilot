import type { Project, Environment, Deployment, GithubRepo, ConversationMessage, AWSAccount, OperationalEvent, CostSummary } from "@/types/api";

// ACTIVE_ORG_KEY stores the workspace the user has selected in the navbar switcher.
// It is sent as X-Org-Id on every request so org-scoped endpoints (list/create
// projects, AWS accounts) target the right workspace. Absent → backend defaults to
// the user's personal org.
export const ACTIVE_ORG_KEY = "opspilot.activeOrgId";

export function getActiveOrgId(): string | null {
  if (typeof window === "undefined") return null;
  return window.localStorage.getItem(ACTIVE_ORG_KEY);
}

export function setActiveOrgId(orgId: string) {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(ACTIVE_ORG_KEY, orgId);
}

// HTTP requests use relative paths — Next.js rewrites /api/v1/* → backend, so no CORS needed.
async function request<T>(
  path: string,
  token: string,
  options: RequestInit = {}
): Promise<T> {
  const activeOrg = getActiveOrgId();
  const res = await fetch(`/api/v1${path}`, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
      ...(activeOrg ? { "X-Org-Id": activeOrg } : {}),
      ...(options.headers ?? {}),
    },
  });

  if (!res.ok) {
    const body = await res.json().catch(() => null);
    const apiError = (body as { error?: string } | null)?.error;
    if (apiError) {
      throw new Error(apiError);
    }
    // No JSON error body — the response didn't come from a backend handler
    // (proxy 404/502 when the Go API is down, or an unregistered route).
    const method = options.method ?? "GET";
    throw new Error(
      `${method} /api/v1${path} failed with HTTP ${res.status} — is the backend running?`
    );
  }

  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

// ---- Projects ----------------------------------------------------------------

export function listProjects(token: string) {
  return request<Project[]>("/projects", token);
}

export function getProject(token: string, id: string) {
  return request<Project>(`/projects/${id}`, token);
}

export function deleteProject(token: string, id: string) {
  return request<{ message: string }>(`/projects/${id}`, token, { method: "DELETE" });
}

export function createProject(
  token: string,
  body: {
    name: string;
    repo_url: string;
    repo_owner: string;
    repo_name: string;
    framework: string;
    branch?: string | null;
    start_command?: string | null;
    account_id?: string | null;
  }
) {
  return request<Project>("/projects", token, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

// ---- Environments ------------------------------------------------------------

export function listEnvironments(token: string, projectId: string) {
  return request<Environment[]>(`/projects/${projectId}/environments`, token);
}

export function createEnvironment(
  token: string,
  projectId: string,
  body: { name: string; aws_region: string }
) {
  return request<Environment>(`/projects/${projectId}/environments`, token, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function retryProvision(token: string, projectId: string, envId: string) {
  return request<{ message: string }>(
    `/projects/${projectId}/environments/${envId}/retry-provision`,
    token,
    { method: "POST" }
  );
}

// ---- AWS Accounts ------------------------------------------------------------

export function listAWSAccounts(token: string) {
  return request<AWSAccount[]>("/aws-accounts", token);
}

export function connectAWSAccount(
  token: string,
  body: { label: string; aws_account_id: string; iam_role_arn: string; external_id?: string; certificate_arn?: string }
) {
  return request<AWSAccount>("/aws-accounts", token, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function deleteAWSAccount(token: string, id: string) {
  return request<{ message: string }>(`/aws-accounts/${id}`, token, {
    method: "DELETE",
  });
}

// ---- Deployments -------------------------------------------------------------

export function listDeployments(token: string, projectId: string) {
  return request<Deployment[]>(`/projects/${projectId}/deployments`, token);
}

export function triggerDeploy(
  token: string,
  projectId: string,
  envId: string,
  env: string = "production"
) {
  return request<{ message: string }>(
    `/projects/${projectId}/environments/${envId}/deploy?env=${env}`,
    token,
    { method: "POST" }
  );
}

export function rollback(token: string, projectId: string, deploymentId: string) {
  return request<{ message: string }>(
    `/projects/${projectId}/deployments/${deploymentId}/rollback`,
    token,
    { method: "POST" }
  );
}

export function redeployDeployment(token: string, projectId: string, deploymentId: string) {
  return request<{ message: string; deployment: import("@/types/api").Deployment }>(
    `/projects/${projectId}/deployments/${deploymentId}/redeploy`,
    token,
    { method: "POST" }
  );
}

export function deleteDeployment(token: string, projectId: string, deploymentId: string) {
  return request<{ message: string }>(
    `/projects/${projectId}/deployments/${deploymentId}`,
    token,
    { method: "DELETE" }
  );
}

export function getDeploymentEvents(token: string, projectId: string, deploymentId: string) {
  return request<OperationalEvent[]>(
    `/projects/${projectId}/deployments/${deploymentId}/events`,
    token
  );
}

export function diagnoseDeployment(token: string, projectId: string, deploymentId: string) {
  return request<{ diagnosis: string }>(
    `/projects/${projectId}/deployments/${deploymentId}/diagnose`,
    token
  );
}

export function checkHealth(token: string, projectId: string, envId: string) {
  return request<{ status: string; running: number; desired: number; pending: number; url?: string }>(
    `/projects/${projectId}/environments/${envId}/health`,
    token
  );
}

export function scaleService(token: string, projectId: string, envId: string, replicas: number) {
  return request<{ message: string }>(
    `/projects/${projectId}/environments/${envId}/scale`,
    token,
    { method: "POST", body: JSON.stringify({ replicas }) }
  );
}

// ---- GitHub ------------------------------------------------------------------

export function getGithubAuthURL(token: string) {
  return request<{ url: string }>("/github/auth", token);
}

export function listGithubRepos(token: string) {
  return request<GithubRepo[]>("/github/repos", token);
}

export function detectFramework(token: string, owner: string, repo: string) {
  return request<{ framework: string }>(
    `/github/repos/${owner}/${repo}/detect`,
    token
  );
}

export function listGithubBranches(token: string, owner: string, repo: string) {
  return request<string[]>(`/github/repos/${owner}/${repo}/branches`, token);
}

// ---- Env Vars ---------------------------------------------------------------

export function listEnvVars(token: string, projectId: string, envId: string) {
  return request<import("@/types/api").EnvVar[]>(
    `/projects/${projectId}/environments/${envId}/env-vars`, token
  );
}

export function upsertEnvVar(
  token: string, projectId: string, envId: string,
  body: { key: string; value: string; is_secret: boolean }
) {
  return request<import("@/types/api").EnvVar>(
    `/projects/${projectId}/environments/${envId}/env-vars`, token,
    { method: "PUT", body: JSON.stringify(body) }
  );
}

export function deleteEnvVar(token: string, projectId: string, envId: string, varId: string) {
  return request<{ message: string }>(
    `/projects/${projectId}/environments/${envId}/env-vars/${varId}`, token,
    { method: "DELETE" }
  );
}

export function revealEnvVar(token: string, projectId: string, envId: string, varId: string) {
  return request<{ value: string }>(
    `/projects/${projectId}/environments/${envId}/env-vars/${varId}/reveal`, token
  );
}

export function getEnvironmentLogs(token: string, projectId: string, envId: string, lines = 200) {
  return request<{ lines: string[]; log_group: string }>(
    `/projects/${projectId}/environments/${envId}/logs?lines=${lines}`,
    token
  );
}

// ---- Conversation ------------------------------------------------------------

export function sendMessage(token: string, projectId: string, message: string) {
  return request<{ response: string }>(
    `/projects/${projectId}/conversation`,
    token,
    { method: "POST", body: JSON.stringify({ message }) }
  );
}

export function getConversationHistory(token: string, projectId: string) {
  return request<ConversationMessage[]>(
    `/projects/${projectId}/conversation/history`,
    token
  );
}

// ---- CloudFormation ----------------------------------------------------------

export async function getBootstrapTemplate(region: string) {
  const res = await fetch(`/api/v1/cloudformation/bootstrap-template?region=${region}`);
  const body = await res.json().catch(() => null);
  if (!res.ok || !body) {
    throw new Error(
      (body as { error?: string } | null)?.error ??
        `Failed to load the setup template (HTTP ${res.status}) — is the backend running?`
    );
  }
  return body as { template: string; script: string; external_id?: string; error?: string };
}

// ---- Webhooks ----------------------------------------------------------------

export function listWebhooks(token: string, projectId: string) {
  return request<import("@/types/api").Webhook[]>(`/projects/${projectId}/webhooks`, token);
}

export function createWebhook(
  token: string,
  projectId: string,
  body: { url: string; secret?: string; events: string[] }
) {
  return request<import("@/types/api").Webhook>(`/projects/${projectId}/webhooks`, token, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function updateWebhook(
  token: string,
  projectId: string,
  webhookId: string,
  body: { url?: string; secret?: string; events?: string[]; active?: boolean }
) {
  return request<import("@/types/api").Webhook>(
    `/projects/${projectId}/webhooks/${webhookId}`,
    token,
    { method: "PATCH", body: JSON.stringify(body) }
  );
}

export function deleteWebhook(token: string, projectId: string, webhookId: string) {
  return request<{ message: string }>(
    `/projects/${projectId}/webhooks/${webhookId}`,
    token,
    { method: "DELETE" }
  );
}

// ---- Cost Intelligence -------------------------------------------------------

export function getProjectCosts(token: string, projectId: string) {
  return request<CostSummary>(`/projects/${projectId}/costs`, token);
}

// ---- Health Score --------------------------------------------------------------

export interface HealthScore {
  score: number;
  grade: "healthy" | "degraded" | "at_risk" | "critical";
  components: Record<string, number>;
  insights: string[];
}

export function getHealthScore(token: string, projectId: string) {
  return request<HealthScore>(`/projects/${projectId}/health-score`, token);
}

// ---- PR Preview Environments -------------------------------------------------

export function enablePreviews(token: string, projectId: string) {
  return request<{ message: string; webhook_id: number }>(
    `/projects/${projectId}/previews/enable`,
    token,
    { method: "POST" }
  );
}

export function disablePreviews(token: string, projectId: string) {
  return request<{ message: string }>(
    `/projects/${projectId}/previews/disable`,
    token,
    { method: "POST" }
  );
}

// ---- WebSocket URL -----------------------------------------------------------
// WebSocket can't go through the Next.js proxy, so it uses the backend URL directly.
// The bearer token is NOT included in the URL — it is sent as the first WebSocket
// message after the connection opens to keep it out of server logs and browser history.

export function wsURL(projectId: string) {
  const apiUrl = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";
  const base = apiUrl.replace(/^http/, "ws");
  return `${base}/api/v1/ws/${projectId}`;
}

export function terminalWsURL(projectId: string, envId: string) {
  const apiUrl = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";
  const base = apiUrl.replace(/^http/, "ws");
  return `${base}/api/v1/ws/${projectId}/terminal/${envId}`;
}

// ---- Alerts --------------------------------------------------------------------

export function listAlerts(token: string, projectId: string, status: string = "open") {
  return request<import("@/types/api").Alert[]>(
    `/projects/${projectId}/alerts?status=${status}`, token
  );
}

export function snoozeAlert(token: string, projectId: string, alertId: string, durationMinutes = 60) {
  return request<{ message: string; snoozed_until: string }>(
    `/projects/${projectId}/alerts/${alertId}/snooze`, token,
    { method: "POST", body: JSON.stringify({ duration_minutes: durationMinutes }) }
  );
}

export function resolveAlert(token: string, projectId: string, alertId: string) {
  return request<{ message: string }>(
    `/projects/${projectId}/alerts/${alertId}/resolve`, token,
    { method: "POST" }
  );
}

// ---- Deploy cancellation --------------------------------------------------------

export function cancelDeployment(token: string, projectId: string, deploymentId: string) {
  return request<{ message: string }>(
    `/projects/${projectId}/deployments/${deploymentId}/cancel`, token,
    { method: "POST" }
  );
}

// ---- Project settings -----------------------------------------------------------

export function updateProject(
  token: string,
  projectId: string,
  body: { name?: string; branch?: string; start_command?: string; framework?: string }
) {
  return request<Project>(`/projects/${projectId}`, token, {
    method: "PATCH",
    body: JSON.stringify(body),
  });
}

// ---- Account / usage ------------------------------------------------------------

export function getMe(token: string) {
  return request<import("@/types/api").UserMe>("/users/me", token);
}

export function updateNotificationPrefs(
  token: string,
  body: { enabled?: boolean; deploy_failed?: boolean; deploy_succeeded?: boolean; alert_fired?: boolean }
) {
  return request<{ message: string }>("/users/me/notifications", token, {
    method: "PATCH",
    body: JSON.stringify(body),
  });
}

// ---- Diagnosis feedback ----------------------------------------------------------

export function submitDiagnosisFeedback(
  token: string,
  projectId: string,
  deploymentId: string,
  body: { incident_id?: string; rating: "helpful" | "not_helpful" | "partially_helpful"; fixed_issue: boolean; notes?: string }
) {
  return request<{ feedback: unknown; score: number }>(
    `/projects/${projectId}/deployments/${deploymentId}/diagnose/feedback`, token,
    { method: "POST", body: JSON.stringify(body) }
  );
}

export function getProjectEvents(token: string, projectId: string, limit = 5) {
  return request<OperationalEvent[]>(`/projects/${projectId}/events?limit=${limit}`, token);
}

// ---- Organizations (team workspaces) -------------------------------------------

import type { Organization, OrganizationMember, OrgRole } from "@/types/api";

export function listMyOrgs(token: string) {
  return request<Organization[]>("/orgs/me", token);
}

export function createOrg(token: string, body: { name: string; slug?: string }) {
  return request<Organization>("/orgs", token, { method: "POST", body: JSON.stringify(body) });
}

export function listOrgMembers(token: string, orgId: string) {
  return request<OrganizationMember[]>(`/orgs/${orgId}/members`, token);
}

export function createOrgInvite(
  token: string,
  orgId: string,
  body: { email: string; role: OrgRole }
) {
  return request<{ accept_url: string; email_sent: boolean }>(
    `/orgs/${orgId}/invites`, token,
    { method: "POST", body: JSON.stringify(body) }
  );
}

export function updateMemberRole(token: string, orgId: string, userId: string, role: OrgRole) {
  return request<{ message: string; role: OrgRole }>(
    `/orgs/${orgId}/members/${userId}`, token,
    { method: "PATCH", body: JSON.stringify({ role }) }
  );
}

export function removeMember(token: string, orgId: string, userId: string) {
  return request<{ message: string }>(
    `/orgs/${orgId}/members/${userId}`, token,
    { method: "DELETE" }
  );
}

export function acceptInvite(token: string, inviteToken: string) {
  return request<{ message: string; organization: Organization }>(
    `/invites/${inviteToken}`, token
  );
}

// ---- Infrastructure discovery --------------------------------------------------

import type { DiscoveredResource, ResourceType } from "@/types/api";

export function scanAccount(token: string, accountId: string) {
  return request<{ job_id: string; message: string }>(
    `/aws-accounts/${accountId}/scan`, token, { method: "POST" }
  );
}

export function listOrgResources(
  token: string,
  orgId: string,
  filters?: { resource_type?: ResourceType | ""; region?: string; project_id?: string }
) {
  const qs = new URLSearchParams();
  if (filters?.resource_type) qs.set("resource_type", filters.resource_type);
  if (filters?.region) qs.set("region", filters.region);
  if (filters?.project_id) qs.set("project_id", filters.project_id);
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<DiscoveredResource[]>(`/orgs/${orgId}/resources${suffix}`, token);
}

export function listProjectResources(token: string, projectId: string) {
  return request<DiscoveredResource[]>(`/projects/${projectId}/resources`, token);
}

export function assignResource(token: string, resourceId: string, projectId: string | null) {
  return request<{ message: string; project_id: string | null }>(
    `/resources/${resourceId}/assign`, token,
    { method: "PATCH", body: JSON.stringify({ project_id: projectId }) }
  );
}

// ---- Incident war room ---------------------------------------------------------

import type { Incident, IncidentDetail, IncidentTimelineEntry } from "@/types/api";

export function listOrgIncidents(token: string, orgId: string, limit = 100) {
  return request<Incident[]>(`/orgs/${orgId}/incidents?limit=${limit}`, token);
}

export function listProjectIncidents(token: string, projectId: string) {
  return request<Incident[]>(`/projects/${projectId}/incidents`, token);
}

export function getIncident(token: string, incidentId: string) {
  return request<IncidentDetail>(`/incidents/${incidentId}`, token);
}

export function postIncidentTimeline(token: string, incidentId: string, content: string, entryType?: string) {
  return request<IncidentTimelineEntry>(`/incidents/${incidentId}/timeline`, token, {
    method: "POST", body: JSON.stringify({ content, entry_type: entryType }),
  });
}

export function acknowledgeIncident(token: string, incidentId: string) {
  return request<{ message: string; status: string }>(`/incidents/${incidentId}/acknowledge`, token, { method: "POST" });
}

export function resolveIncident(token: string, incidentId: string) {
  return request<{ status: string; postmortem: string }>(`/incidents/${incidentId}/resolve`, token, { method: "POST" });
}

export function savePostmortem(token: string, incidentId: string, postmortem: string) {
  return request<{ message: string }>(`/incidents/${incidentId}/postmortem`, token, {
    method: "POST", body: JSON.stringify({ postmortem }),
  });
}

export function approveIncidentAction(token: string, incidentId: string, actionId: string) {
  return request<{ message: string; status: string }>(`/incidents/${incidentId}/actions/${actionId}/approve`, token, { method: "POST" });
}

export function rejectIncidentAction(token: string, incidentId: string, actionId: string) {
  return request<{ message: string; status: string }>(`/incidents/${incidentId}/actions/${actionId}/reject`, token, { method: "POST" });
}

export function incidentWsURL(incidentId: string) {
  const apiUrl = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";
  const base = apiUrl.replace(/^http/, "ws");
  return `${base}/api/v1/ws/incidents/${incidentId}`;
}

// ---- Slack integration ---------------------------------------------------------

import type { SlackStatus, SlackChannel } from "@/types/api";

export function getSlackStatus(token: string, orgId: string) {
  return request<SlackStatus>(`/orgs/${orgId}/slack`, token);
}

export function getSlackInstallURL(token: string, orgId: string) {
  return request<{ url: string }>(`/orgs/${orgId}/slack/install`, token);
}

export function listSlackChannels(token: string, orgId: string) {
  return request<SlackChannel[]>(`/orgs/${orgId}/slack/channels`, token);
}

export function updateSlackChannels(
  token: string,
  orgId: string,
  body: {
    alert_channel_id?: string | null; alert_channel_name?: string | null;
    deploy_channel_id?: string | null; deploy_channel_name?: string | null;
    summary_channel_id?: string | null; summary_channel_name?: string | null;
  }
) {
  return request<{ message: string }>(`/orgs/${orgId}/slack`, token, {
    method: "PATCH", body: JSON.stringify(body),
  });
}

export function disconnectSlack(token: string, orgId: string) {
  return request<{ message: string }>(`/orgs/${orgId}/slack`, token, { method: "DELETE" });
}
