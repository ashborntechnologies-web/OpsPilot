import type { Project, Environment, Deployment, GithubRepo, ConversationMessage, AWSAccount, OperationalEvent, CostSummary } from "@/types/api";

// HTTP requests use relative paths — Next.js rewrites /api/v1/* → backend, so no CORS needed.
async function request<T>(
  path: string,
  token: string,
  options: RequestInit = {}
): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
      ...(options.headers ?? {}),
    },
  });

  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error((body as { error?: string }).error ?? `HTTP ${res.status}`);
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
  body: { label: string; aws_account_id: string; iam_role_arn: string; external_id?: string }
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

export function getBootstrapTemplate(region: string) {
  return fetch(`/api/v1/cloudformation/bootstrap-template?region=${region}`)
    .then((r) => r.json()) as Promise<{ template: string; script: string; external_id?: string; error?: string }>;
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
