"use client";

import { useEffect, useState, useCallback, useRef } from "react";
import useSWR from "swr";
import Link from "next/link";
import { useParams } from "next/navigation";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Separator } from "@/components/ui/separator";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";
import {
  getProject, listEnvironments, listDeployments,
  createEnvironment, triggerDeploy, rollback, retryProvision, getEnvironmentLogs,
  getDeploymentEvents, wsURL, redeployDeployment, deleteDeployment,
  listEnvVars, upsertEnvVar, deleteEnvVar, revealEnvVar,
  diagnoseDeployment, checkHealth, scaleService,
  listWebhooks, createWebhook, updateWebhook, deleteWebhook,
  terminalWsURL, getProjectCosts, enablePreviews, disablePreviews,
  getHealthScore, listAlerts, snoozeAlert, resolveAlert, cancelDeployment,
  updateProject, getMe, updateNotificationPrefs, deleteProject,
  listProjectResources, listProjectActions, approveAction, rejectAction,
  getEnvironmentTrust, updateEnvironmentTrust,
} from "@/lib/api";
import { ActionRow } from "@/components/trust/actions";
import { EnvTrustSettings } from "@/components/trust/env-trust-settings";
import { EnvSLASettings } from "@/components/analytics/env-sla-settings";
import { StatusSidebar } from "@/components/project/status-sidebar";
import { AlertsPanel } from "@/components/project/alerts-panel";
import { useActiveOrg } from "@/lib/use-org";
import { RESOURCE_ICONS, resourceLabel, resourceStatus } from "@/lib/resources";
import { ConfidenceBadge, EvidenceSection } from "@/components/ai/explainability";
import type { Project, Environment, Deployment, OperationalEvent, WsMessage, EnvVar, Webhook, CostSummary, Alert, RiskScore, UserMe, DiscoveredResource, EvidenceItem, AIAction, TrustLevel, AutonomousBoundaries } from "@/types/api";
import { cn } from "@/lib/utils";
import { toast } from "sonner";
import {
  MessageSquare, Plus, Rocket, RotateCcw,
  CheckCircle2, XCircle, Clock, Loader2, Cloud,
  RefreshCw, Terminal, AlertTriangle, ChevronDown, ChevronRight,
  Trash2, Eye, EyeOff, KeyRound, Activity, Scaling, Webhook as WebhookIcon, ZapOff,
  DollarSign, GitPullRequest, Zap,
} from "lucide-react";
import "@xterm/xterm/css/xterm.css";

const AWS_REGIONS = [
  "us-east-1", "us-east-2", "us-west-1", "us-west-2",
  "eu-west-1", "eu-central-1", "ap-southeast-1", "ap-northeast-1",
];

const STACK_BADGE: Record<Environment["stack_status"], { label: string; variant: "default" | "secondary" | "destructive" | "outline" }> = {
  pending:      { label: "Pending",      variant: "secondary" },
  provisioning: { label: "Provisioning", variant: "outline" },
  ready:        { label: "Ready",        variant: "default" },
  failed:       { label: "Failed",       variant: "destructive" },
};

const DEPLOY_ICON: Record<Deployment["status"], React.ReactNode> = {
  pending:     <Clock className="h-3 w-3 text-zinc-400" />,
  building:    <Loader2 className="h-3 w-3 text-blue-500 animate-spin" />,
  deploying:   <Loader2 className="h-3 w-3 text-indigo-500 animate-spin" />,
  live:        <CheckCircle2 className="h-3 w-3 text-green-500" />,
  failed:      <XCircle className="h-3 w-3 text-red-500" />,
  rolled_back: <RotateCcw className="h-3 w-3 text-zinc-400" />,
};

// Returns 0–3 based on the highest provision step seen in the log messages.
// 0 = queued, 1 = AWS connected, 2 = platform stack, 3 = project resources
function inferProvisionStep(logs: string[]): number {
  const text = logs.join(" ").toLowerCase();
  if (text.includes("project resource") || text.includes("deploying project")) return 3;
  if (text.includes("platform stack") || text.includes("shared platform")) return 2;
  if (text.includes("connecting") || text.includes("aws")) return 1;
  return 0;
}

const PROVISION_STEPS = ["AWS", "Platform stack", "Project resources"];
// Progress percentages for each inferred step (0 = just queued, 3 = resources deploying)
const STEP_PROGRESS = [5, 25, 55, 80];

// Deploy pipeline stages shown on the Overview tab while a deployment is in flight.
const DEPLOY_STAGES = ["Build", "Configure", "Rollout"];
const DEPLOY_STAGE_PROGRESS = [8, 30, 70, 88];

// Returns 0–3 based on the latest deploy progress messages (see backend broadcasts:
// "Starting container build..." → "Build in progress: PHASE" → "Registering task
// definition..." → "Ensuring ALB..." → "Deploying to ECS..." → "x/y tasks running").
function inferDeployStage(logs: string[], dep?: Deployment): number {
  const text = logs.join(" ").toLowerCase();
  if (text.includes("tasks running") || text.includes("deploying to ecs")) return 3;
  if (
    text.includes("task definition") ||
    text.includes("target group") ||
    text.includes("listener rule")
  ) {
    return 2;
  }
  if (text.includes("build")) return 1;
  // No WS messages yet (page opened mid-deploy) — infer from the record's status.
  if (dep?.status === "deploying") return 3;
  if (dep?.status === "building") return 1;
  return 0;
}

const EVENT_LABELS: Record<string, string> = {
  "deploy.started":      "Deploy started",
  "build.started":       "Build started",
  "build.completed":     "Build completed",
  "build.failed":        "Build failed",
  "ecs.rollout.started": "ECS rollout started",
  "ecs.stable":          "Service stable",
  "healthcheck.failed":  "Health check failed",
  "rollback.triggered":  "Rollback triggered",
  "provision.started":   "Provision started",
  "provision.ready":     "Environment ready",
  "provision.failed":    "Provision failed",
};

function timeAgo(iso: string) {
  const s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export default function ProjectPage() {
  const { id } = useParams<{ id: string }>();
  const { getToken } = useAuth();

  // Role in the active workspace. Viewers get read-only UI; the backend enforces
  // this regardless, so these guards are UX (clear messaging), not the security boundary.
  const { isViewer, canAct, isAdmin } = useActiveOrg();
  const VIEW_ONLY_MSG = "You have view-only (viewer) access to this workspace. Ask an admin for the engineer role to perform this action.";
  function blockIfViewer(): boolean {
    if (isViewer) { toast.error(VIEW_ONLY_MSG); return true; }
    return false;
  }

  const [project, setProject] = useState<Project | null>(null);
  const [environments, setEnvironments] = useState<Environment[]>([]);
  const [deployments, setDeployments] = useState<Deployment[]>([]);
  const [deploying, setDeploying] = useState<string | null>(null);
  const [retrying, setRetrying] = useState<string | null>(null);

  // live provision messages received via WebSocket
  const [provisionLog, setProvisionLog] = useState<string[]>([]);
  // live deploy progress messages received via WebSocket (cleared when a deploy finishes)
  const [deployLog, setDeployLog] = useState<string[]>([]);
  // raw CodeBuild output streamed during builds
  const [buildLogs, setBuildLogs] = useState<string[]>([]);
  const [showBuildOutput, setShowBuildOutput] = useState(false);
  // open alerts (SWR-backed + live WS updates)
  const [alerts, setAlerts] = useState<Alert[]>([]);
  // banner shown on small screens when an alert fires while the panel is hidden
  const [alertBanner, setAlertBanner] = useState<Alert | null>(null);
  // latest pre-deploy risk score (from the deploy_risk WS message)
  const [currentRiskScore, setCurrentRiskScore] = useState<RiskScore | null>(null);
  // mobile/tablet status drawer
  const [statusDrawerOpen, setStatusDrawerOpen] = useState(false);
  // settings tab state
  const [settingsName, setSettingsName] = useState("");
  const [settingsBranch, setSettingsBranch] = useState("");
  const [settingsStartCmd, setSettingsStartCmd] = useState("");
  const [settingsFramework, setSettingsFramework] = useState("");
  const [savingSettings, setSavingSettings] = useState(false);
  const [me, setMe] = useState<UserMe | null>(null);
  const [savingPrefs, setSavingPrefs] = useState(false);
  const [deletingProject, setDeletingProject] = useState(false);

  // deployment event timeline
  const [expandedDepId, setExpandedDepId] = useState<string | null>(null);
  const [depEvents, setDepEvents] = useState<Record<string, OperationalEvent[]>>({});
  const [loadingDepEvents, setLoadingDepEvents] = useState<string | null>(null);

  // redeploy / delete
  const [redeploying, setRedeploying] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Deployment | null>(null);
  const [deleteConfirmText, setDeleteConfirmText] = useState("");
  const [deleting, setDeleting] = useState(false);

  // env creation dialog
  const [envDialog, setEnvDialog] = useState<"staging" | "production" | null>(null);
  const [envRegion, setEnvRegion] = useState("us-east-1");
  const [creatingEnv, setCreatingEnv] = useState(false);

  // logs tab
  const [logsEnvId, setLogsEnvId] = useState<string>("");
  const [logLines, setLogLines] = useState<string[]>([]);
  const [loadingLogs, setLoadingLogs] = useState(false);
  const logsEndRef = useRef<HTMLDivElement>(null);

  // env vars tab
  const [envVarEnvId, setEnvVarEnvId] = useState<string>("");
  const [envVars, setEnvVars] = useState<EnvVar[]>([]);
  const [loadingEnvVars, setLoadingEnvVars] = useState(false);
  const [newKey, setNewKey] = useState("");
  const [newValue, setNewValue] = useState("");
  const [newIsSecret, setNewIsSecret] = useState(false);
  const [savingEnvVar, setSavingEnvVar] = useState(false);
  const [showSecretValues, setShowSecretValues] = useState<Record<string, boolean>>({});
  // Plaintext secret values fetched on demand via the reveal endpoint — the list
  // API intentionally redacts them.
  const [revealedValues, setRevealedValues] = useState<Record<string, string>>({});

  // diagnose
  const [diagnosing, setDiagnosing] = useState<string | null>(null);
  const [diagnosisResult, setDiagnosisResult] = useState<string | null>(null);
  const [diagnosisConfidence, setDiagnosisConfidence] = useState<number | null>(null);
  const [diagnosisEvidence, setDiagnosisEvidence] = useState<EvidenceItem[]>([]);

  // health
  const [healthData, setHealthData] = useState<Record<string, { status: string; running: number; desired: number; pending: number; url?: string }>>({});
  const [checkingHealth, setCheckingHealth] = useState<string | null>(null);

  // scale
  const [scaleTarget, setScaleTarget] = useState<Environment | null>(null);
  const [scaleReplicas, setScaleReplicas] = useState(1);
  const [scaling, setScaling] = useState(false);

  // terminal
  const [terminalEnvId, setTerminalEnvId] = useState<string>("");
  // Bumping the nonce re-runs the terminal effect → fresh SSM session (Reconnect button).
  const [terminalNonce, setTerminalNonce] = useState(0);
  const [terminalDisconnected, setTerminalDisconnected] = useState(false);
  const terminalRef = useRef<HTMLDivElement>(null);

  // webhooks
  const [hooksList, setHooksList] = useState<Webhook[]>([]);
  const [loadingHooks, setLoadingHooks] = useState(false);
  const [hookDialog, setHookDialog] = useState(false);
  const [newHookUrl, setNewHookUrl] = useState("");
  const [newHookSecret, setNewHookSecret] = useState("");
  const [newHookEvents, setNewHookEvents] = useState<string[]>([]);
  const [savingHook, setSavingHook] = useState(false);

  // cost intelligence
  const [costs, setCosts] = useState<CostSummary | null>(null);
  // infrastructure (assigned discovered + managed resources)
  const [resources, setResources] = useState<DiscoveredResource[] | null>(null);
  const [loadingResources, setLoadingResources] = useState(false);
  // AI actions (trust/approval): all (history) + pending (right-column approvals)
  const [actions, setActions] = useState<AIAction[] | null>(null);
  const [actionBanner, setActionBanner] = useState(false);
  const pendingActions = (actions ?? []).filter((a) => a.status === "pending_approval");
  const [loadingCosts, setLoadingCosts] = useState(false);

  // PR previews
  const [togglingPreviews, setTogglingPreviews] = useState(false);

  const refresh = useCallback(async () => {
    const token = await getToken();
    if (!token) return;
    const [proj, envs, deps] = await Promise.all([
      getProject(token, id),
      listEnvironments(token, id),
      listDeployments(token, id),
    ]);
    setProject(proj);
    setEnvironments(envs ?? []);
    setDeployments(deps ?? []);
    // Auto-select tab targets once environments arrive: logs prefers a ready env;
    // env vars accepts any env (you can set vars before deploying).
    const ready = (envs ?? []).find((e) => e.stack_status === "ready");
    if (ready) setLogsEnvId((prev) => prev || ready.id);
    if (envs?.length) setEnvVarEnvId((prev) => prev || envs[0].id);
  }, [id, getToken]);

  // Initial load (and revalidate-on-focus) via SWR; refresh stays available for
  // event-driven re-fetches (WS messages, after mutations, provisioning poll).
  const { isLoading: loading } = useSWR(["project-page", id], refresh);

  // Open alerts — refreshed every 30s; WS messages update the list live between polls.
  useSWR(
    ["alerts", id],
    async () => {
      const token = await getToken();
      if (!token) return [];
      return listAlerts(token, id, "open");
    },
    { refreshInterval: 30_000, onSuccess: (data) => setAlerts(data ?? []) }
  );

  // AI actions (trust/approval) — initial load + 30s refresh; WS updates in between.
  useSWR(
    ["actions", id],
    async () => {
      const token = await getToken();
      if (!token) return [];
      return listProjectActions(token, id);
    },
    { refreshInterval: 30_000, onSuccess: (data) => setActions(data ?? []) }
  );

  // Settings form mirrors the project record; /users/me feeds notification toggles.
  const settingsSeeded = useRef(false);
  useEffect(() => {
    if (!project || settingsSeeded.current) return;
    settingsSeeded.current = true;
    setSettingsName(project.name);
    setSettingsBranch(project.branch ?? "");
    setSettingsStartCmd(project.start_command ?? "");
    setSettingsFramework(project.framework);
    void (async () => {
      const token = await getToken();
      if (!token) return;
      try {
        setMe(await getMe(token));
      } catch {}
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [project]);

  // Deployment health score — refreshed every minute while the page is open.
  const { data: healthScore } = useSWR(
    ["health-score", id],
    async () => {
      const token = await getToken();
      if (!token) return null;
      return getHealthScore(token, id);
    },
    { refreshInterval: 60_000 }
  );

  // WebSocket — open while the page is mounted so we receive provision_progress in real time.
  useEffect(() => {
    let ws: WebSocket | null = null;
    let closed = false;
    getToken().then((token) => {
      if (!token || closed) return;
      ws = new WebSocket(wsURL(id));
      ws.onopen = () => {
        ws?.send(JSON.stringify({ type: "auth", token }));
      };
      ws.onmessage = (ev) => {
        try {
          const msg = JSON.parse(ev.data) as WsMessage;
          if (msg.type === "provision_progress") {
            setProvisionLog((prev) => [...prev.slice(-49), msg.payload]);
          } else if (msg.type === "provision_done" || msg.type === "provision_failed") {
            setProvisionLog((prev) => [...prev.slice(-49), msg.payload]);
            refresh().catch(() => {});
          } else if (msg.type === "deploy_progress") {
            setDeployLog((prev) => [...prev.slice(-49), msg.payload]);
          } else if (msg.type === "deploy_done" || msg.type === "deploy_failed") {
            setDeployLog([]);
            setBuildLogs([]);
            setCurrentRiskScore(null);
            if (msg.type === "deploy_done") toast.success(msg.payload);
            else toast.error(msg.payload);
            refresh().catch(() => {});
          } else if (msg.type === "build_log") {
            setBuildLogs((prev) => [...prev.slice(-299), msg.payload]);
          } else if (msg.type === "alert") {
            try {
              const alert = JSON.parse(msg.payload) as Alert;
              setAlerts((prev) => [alert, ...prev.filter((a) => a.id !== alert.id)]);
              setAlertBanner(alert);
              toast.error(`${alert.title}${alert.summary ? " — " + alert.summary : ""}`);
            } catch {}
          } else if (msg.type === "alert_resolved") {
            try {
              const resolved = JSON.parse(msg.payload) as { id: string; alert_type: string };
              setAlerts((prev) => prev.filter((a) => a.id !== resolved.id));
              setAlertBanner((prev) => (prev?.id === resolved.id ? null : prev));
              toast.success(`${resolved.alert_type.replace(/_/g, " ")} resolved`);
            } catch {}
          } else if (msg.type === "deploy_risk") {
            try {
              setCurrentRiskScore(JSON.parse(msg.payload) as RiskScore);
            } catch {}
          } else if (msg.type === "action_proposed") {
            setActionBanner(true);
            loadActions().catch(() => {});
          } else if (msg.type === "action_updated") {
            loadActions().catch(() => {});
          }
        } catch {}
      };
    });
    return () => {
      closed = true;
      ws?.close();
    };
  }, [id, getToken, refresh]);

  // Terminal — mount xterm.js and open SSM datachannel proxy when an env is selected.
  useEffect(() => {
    if (!terminalEnvId || !terminalRef.current) return;
    setTerminalDisconnected(false);

    let term: import("@xterm/xterm").Terminal | null = null;
    let ws: WebSocket | null = null;
    let closed = false;

    (async () => {
      const { Terminal: XTerm } = await import("@xterm/xterm");
      const { FitAddon } = await import("@xterm/addon-fit");

      if (closed || !terminalRef.current) return;

      term = new XTerm({ cursorBlink: true, convertEol: true, fontSize: 13, fontFamily: "monospace" });
      const fitAddon = new FitAddon();
      term.loadAddon(fitAddon);
      term.open(terminalRef.current);
      fitAddon.fit();

      const token = await getToken();
      if (!token || closed) { term.dispose(); return; }

      ws = new WebSocket(terminalWsURL(id, terminalEnvId));
      ws.binaryType = "arraybuffer";

      ws.onopen = () => ws?.send(JSON.stringify({ type: "auth", token }));

      ws.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          try {
            const msg = JSON.parse(ev.data) as { type: string; payload?: string };
            if (msg.type === "error") term?.write(`\r\n\x1b[31m${msg.payload}\x1b[0m\r\n`);
            if (msg.type === "closed") term?.write("\r\n\x1b[33m[session closed]\x1b[0m\r\n");
          } catch {}
        } else {
          term?.write(new Uint8Array(ev.data as ArrayBuffer));
        }
      };

      ws.onclose = () => {
        term?.write("\r\n\x1b[33m[disconnected]\x1b[0m\r\n");
        if (!closed) setTerminalDisconnected(true);
      };

      term.onData((data) => ws?.readyState === WebSocket.OPEN && ws.send(data));
      term.onResize(({ cols, rows }) =>
        ws?.readyState === WebSocket.OPEN &&
        ws.send(JSON.stringify({ type: "resize", cols, rows }))
      );

      const ro = new ResizeObserver(() => fitAddon.fit());
      if (terminalRef.current) ro.observe(terminalRef.current);

      // Store ro cleanup in a closure var so the return below can access it.
      (ws as WebSocket & { _ro?: ResizeObserver })._ro = ro;
    })();

    return () => {
      closed = true;
      (ws as (WebSocket & { _ro?: ResizeObserver }) | null)?._ro?.disconnect();
      ws?.close();
      term?.dispose();
    };
  }, [terminalEnvId, terminalNonce, id, getToken]);

  // Polling — re-fetch every 5 s while an environment is provisioning or a
  // deployment is in flight, so status flips even if WebSocket messages are missed.
  const hasActivity =
    environments.some((e) => e.stack_status === "provisioning") ||
    deployments.some((d) => ["pending", "building", "deploying"].includes(d.status));
  useEffect(() => {
    if (!hasActivity) return;
    const t = setInterval(() => { refresh().catch(() => {}); }, 5000);
    return () => clearInterval(t);
  }, [hasActivity, refresh]);

  // The most recent in-flight deployment, if any — drives the Overview progress card.
  const activeDeployment = deployments.find((d) =>
    ["pending", "building", "deploying"].includes(d.status)
  );

  async function handleSnoozeAlert(alertId: string) {
    if (blockIfViewer()) return;
    const token = await getToken();
    if (!token) return;
    try {
      await snoozeAlert(token, id, alertId, 60);
      setAlerts((prev) => prev.filter((a) => a.id !== alertId));
      setAlertBanner((prev) => (prev?.id === alertId ? null : prev));
      toast.success("Alert snoozed for 1 hour");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    }
  }

  async function handleResolveAlert(alertId: string) {
    if (blockIfViewer()) return;
    const token = await getToken();
    if (!token) return;
    try {
      await resolveAlert(token, id, alertId);
      setAlerts((prev) => prev.filter((a) => a.id !== alertId));
      setAlertBanner((prev) => (prev?.id === alertId ? null : prev));
      toast.success("Alert resolved");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    }
  }

  async function handleCancelDeployment(dep: Deployment) {
    if (blockIfViewer()) return;
    if (!window.confirm(`Cancel the in-progress deployment of ${dep.commit_sha.slice(0, 8)}?`)) return;
    const token = await getToken();
    if (!token) return;
    try {
      await cancelDeployment(token, id, dep.id);
      toast.success("Deployment cancelled");
      await refresh();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    }
  }

  async function handleSaveSettings() {
    const token = await getToken();
    if (!token) return;
    setSavingSettings(true);
    try {
      const updated = await updateProject(token, id, {
        name: settingsName || undefined,
        branch: settingsBranch,
        start_command: settingsStartCmd,
        framework: settingsFramework || undefined,
      });
      setProject(updated);
      toast.success("Project settings saved");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setSavingSettings(false);
    }
  }

  async function handleSavePrefs(patch: { deploy_failed?: boolean; deploy_succeeded?: boolean; alert_fired?: boolean }) {
    const token = await getToken();
    if (!token || !me) return;
    setSavingPrefs(true);
    const next = { ...me, notifications: { ...me.notifications, ...patch } };
    setMe(next);
    try {
      await updateNotificationPrefs(token, patch);
    } catch (e: unknown) {
      toast.error((e as Error).message);
      setMe(me); // revert optimistic update
    } finally {
      setSavingPrefs(false);
    }
  }

  async function handleDeleteProject() {
    if (!project) return;
    const typed = window.prompt(`Type the project name (${project.name}) to confirm deletion:`);
    if (typed !== project.name) {
      if (typed !== null) toast.error("Name did not match — deletion cancelled");
      return;
    }
    const token = await getToken();
    if (!token) return;
    setDeletingProject(true);
    try {
      await deleteProject(token, id);
      toast.success("Project deleted");
      window.location.href = "/projects";
    } catch (e: unknown) {
      toast.error((e as Error).message);
      setDeletingProject(false);
    }
  }

  async function handleCreateEnv() {
    if (blockIfViewer()) return;
    if (!envDialog) return;
    const token = await getToken();
    if (!token) return;
    setCreatingEnv(true);
    try {
      await createEnvironment(token, id, { name: envDialog, aws_region: envRegion });
      await refresh();
      toast.success(`${envDialog} environment created — provisioning infrastructure...`);
      setEnvDialog(null);
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setCreatingEnv(false);
    }
  }

  async function handleDeploy(env: Environment) {
    if (blockIfViewer()) return;
    const branch = project?.branch || "default branch";
    if (!window.confirm(`Deploy the latest commit on ${branch} to ${env.name}?`)) {
      return;
    }
    const token = await getToken();
    if (!token) return;
    setDeploying(env.id);
    try {
      const { message } = await triggerDeploy(token, id, env.id, env.name);
      toast.success(message);
      setTimeout(() => refresh().catch(() => {}), 2000);
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setDeploying(null);
    }
  }

  async function handleRetry(env: Environment) {
    const token = await getToken();
    if (!token) return;
    setRetrying(env.id);
    try {
      await retryProvision(token, id, env.id);
      await refresh();
      toast.success("Provisioning restarted");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setRetrying(null);
    }
  }

  async function handleRollback(dep: Deployment) {
    if (blockIfViewer()) return;
    const token = await getToken();
    if (!token) return;
    try {
      const { message } = await rollback(token, id, dep.id);
      toast.success(message);
      setTimeout(refresh, 2000);
    } catch (e: unknown) {
      toast.error((e as Error).message);
    }
  }

  async function fetchLogs() {
    if (!logsEnvId) return;
    const token = await getToken();
    if (!token) return;
    setLoadingLogs(true);
    try {
      const { lines } = await getEnvironmentLogs(token, id, logsEnvId, 300);
      setLogLines(lines ?? []);
      setTimeout(() => logsEndRef.current?.scrollIntoView({ behavior: "smooth" }), 50);
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to fetch logs");
    } finally {
      setLoadingLogs(false);
    }
  }

  async function handleRedeploy(dep: Deployment) {
    if (blockIfViewer()) return;
    const token = await getToken();
    if (!token) return;
    setRedeploying(dep.id);
    try {
      const { message } = await redeployDeployment(token, id, dep.id);
      toast.success(message);
      setTimeout(refresh, 2000);
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setRedeploying(null);
    }
  }

  function handleDeleteOpen(dep: Deployment) {
    setDeleteTarget(dep);
    setDeleteConfirmText("");
  }

  async function handleDeleteConfirm() {
    if (!deleteTarget) return;
    const token = await getToken();
    if (!token) return;
    setDeleting(true);
    try {
      await deleteDeployment(token, id, deleteTarget.id);
      toast.success("Deployment deleted");
      setDeleteTarget(null);
      await refresh();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setDeleting(false);
    }
  }

  async function handleToggleTimeline(dep: Deployment) {
    if (expandedDepId === dep.id) {
      setExpandedDepId(null);
      return;
    }
    setExpandedDepId(dep.id);
    if (depEvents[dep.id]) return;
    const token = await getToken();
    if (!token) return;
    setLoadingDepEvents(dep.id);
    try {
      const evs = await getDeploymentEvents(token, id, dep.id);
      setDepEvents((prev) => ({ ...prev, [dep.id]: evs ?? [] }));
    } catch {
      setDepEvents((prev) => ({ ...prev, [dep.id]: [] }));
    } finally {
      setLoadingDepEvents(null);
    }
  }

  async function fetchEnvVars(envId = envVarEnvId) {
    if (!envId) return;
    const token = await getToken();
    if (!token) return;
    setLoadingEnvVars(true);
    try {
      const vars = await listEnvVars(token, id, envId);
      setEnvVars(vars ?? []);
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to fetch env vars");
    } finally {
      setLoadingEnvVars(false);
    }
  }

  // Auto-load env vars as soon as an environment is selected, so the tab shows
  // real data (not a misleading empty state) on first click.
  const envVarsLoadedFor = useRef<string>("");
  useEffect(() => {
    if (!envVarEnvId || envVarsLoadedFor.current === envVarEnvId) return;
    envVarsLoadedFor.current = envVarEnvId;
    void fetchEnvVars(envVarEnvId);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [envVarEnvId]);

  // Show/hide a secret value. The list API redacts secrets, so the first reveal
  // fetches the plaintext from the dedicated reveal endpoint; hiding is local.
  async function toggleRevealSecret(v: EnvVar) {
    if (showSecretValues[v.id]) {
      setShowSecretValues((p) => ({ ...p, [v.id]: false }));
      return;
    }
    if (revealedValues[v.id] === undefined) {
      const token = await getToken();
      if (!token) return;
      try {
        const { value } = await revealEnvVar(token, id, envVarEnvId, v.id);
        setRevealedValues((p) => ({ ...p, [v.id]: value }));
      } catch (e: unknown) {
        toast.error((e as Error).message ?? "Failed to reveal value");
        return;
      }
    }
    setShowSecretValues((p) => ({ ...p, [v.id]: true }));
  }

  async function handleSaveEnvVar() {
    if (blockIfViewer()) return;
    if (!newKey.trim() || !newValue.trim() || !envVarEnvId) return;
    const token = await getToken();
    if (!token) return;
    setSavingEnvVar(true);
    try {
      await upsertEnvVar(token, id, envVarEnvId, {
        key: newKey.trim(),
        value: newValue.trim(),
        is_secret: newIsSecret,
      });
      setNewKey("");
      setNewValue("");
      setNewIsSecret(false);
      await fetchEnvVars();
      toast.success("Env var saved — redeploy to apply.");
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to save env var");
    } finally {
      setSavingEnvVar(false);
    }
  }

  async function handleDeleteEnvVar(v: EnvVar) {
    if (blockIfViewer()) return;
    const token = await getToken();
    if (!token) return;
    try {
      await deleteEnvVar(token, id, envVarEnvId, v.id);
      setEnvVars((prev) => prev.filter((e) => e.id !== v.id));
      toast.success("Env var deleted — redeploy to apply.");
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to delete env var");
    }
  }

  // ── Diagnose ────────────────────────────────────────────────────────────────
  async function handleDiagnose(dep: Deployment) {
    const token = await getToken();
    if (!token) return;
    setDiagnosing(dep.id);
    try {
      const res = await diagnoseDeployment(token, id, dep.id);
      setDiagnosisResult(res.diagnosis);
      setDiagnosisConfidence(res.confidence_score);
      setDiagnosisEvidence(res.evidence ?? []);
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Diagnosis failed");
    } finally {
      setDiagnosing(null);
    }
  }

  // ── Health check ────────────────────────────────────────────────────────────
  async function handleCheckHealth(env: Environment) {
    const token = await getToken();
    if (!token) return;
    setCheckingHealth(env.id);
    try {
      const result = await checkHealth(token, id, env.id);
      setHealthData((prev) => ({ ...prev, [env.id]: result }));
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Health check failed");
    } finally {
      setCheckingHealth(null);
    }
  }

  // ── Scale ────────────────────────────────────────────────────────────────────
  async function handleScale() {
    if (blockIfViewer()) return;
    if (!scaleTarget) return;
    const token = await getToken();
    if (!token) return;
    setScaling(true);
    try {
      const { message } = await scaleService(token, id, scaleTarget.id, scaleReplicas);
      toast.success(message);
      setScaleTarget(null);
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Scale failed");
    } finally {
      setScaling(false);
    }
  }

  // ── Webhooks ─────────────────────────────────────────────────────────────────
  async function loadHooks() {
    const token = await getToken();
    if (!token) return;
    setLoadingHooks(true);
    try {
      const data = await listWebhooks(token, id);
      setHooksList(data ?? []);
    } catch {
      toast.error("Failed to load webhooks");
    } finally {
      setLoadingHooks(false);
    }
  }

  async function handleCreateHook() {
    if (!newHookUrl || newHookEvents.length === 0) return;
    const token = await getToken();
    if (!token) return;
    setSavingHook(true);
    try {
      const hook = await createWebhook(token, id, {
        url: newHookUrl,
        secret: newHookSecret || undefined,
        events: newHookEvents,
      });
      setHooksList((prev) => [hook, ...prev]);
      setHookDialog(false);
      setNewHookUrl(""); setNewHookSecret(""); setNewHookEvents([]);
      toast.success("Webhook created");
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to create webhook");
    } finally {
      setSavingHook(false);
    }
  }

  async function handleDeleteHook(hookId: string) {
    const token = await getToken();
    if (!token) return;
    try {
      await deleteWebhook(token, id, hookId);
      setHooksList((prev) => prev.filter((w) => w.id !== hookId));
      toast.success("Webhook deleted");
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to delete webhook");
    }
  }

  async function handleToggleHook(hook: Webhook) {
    const token = await getToken();
    if (!token) return;
    try {
      const updated = await updateWebhook(token, id, hook.id, { active: !hook.active });
      setHooksList((prev) => prev.map((w) => (w.id === hook.id ? updated : w)));
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to update webhook");
    }
  }

  async function loadCosts() {
    const token = await getToken();
    if (!token) return;
    setLoadingCosts(true);
    try {
      const data = await getProjectCosts(token, id);
      setCosts(data);
    } catch {
      toast.error("Failed to load cost data");
    } finally {
      setLoadingCosts(false);
    }
  }

  async function loadResources() {
    const token = await getToken();
    if (!token) return;
    setLoadingResources(true);
    try {
      setResources(await listProjectResources(token, id));
    } catch {
      toast.error("Failed to load infrastructure");
    } finally {
      setLoadingResources(false);
    }
  }

  async function loadActions() {
    const token = await getToken();
    if (!token) return;
    try {
      setActions(await listProjectActions(token, id));
    } catch {
      toast.error("Failed to load actions");
    }
  }

  async function handleApproveAction(actionId: string) {
    const token = await getToken();
    if (!token) return;
    try {
      await approveAction(token, actionId);
      toast.success("Action approved");
      await loadActions();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    }
  }

  async function handleRejectAction(actionId: string) {
    const token = await getToken();
    if (!token) return;
    try {
      await rejectAction(token, actionId);
      toast.success("Action rejected");
      await loadActions();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    }
  }

  async function handleTogglePreviews() {
    if (blockIfViewer()) return;
    const token = await getToken();
    if (!token) return;
    setTogglingPreviews(true);
    try {
      if (project?.previews_enabled) {
        await disablePreviews(token, id);
        toast.success("PR preview environments disabled");
      } else {
        await enablePreviews(token, id);
        toast.success("PR preview environments enabled — push a PR to test it!");
      }
      await refresh();
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to toggle previews");
    } finally {
      setTogglingPreviews(false);
    }
  }

  const readyEnvs = environments.filter((e) => e.stack_status === "ready" && !e.is_preview);
  const selectedLogEnv = environments.find((e) => e.id === logsEnvId);

  if (loading) {
    return (
      <div className="min-h-screen bg-zinc-50">
        <Navbar />
        <div className="flex items-center justify-center h-64">
          <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
        </div>
      </div>
    );
  }

  if (!project) return null;

  const existingEnvNames = environments.filter((e) => !e.is_preview).map((e) => e.name);

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />

      {/* Delete deployment confirmation dialog */}
      <Dialog open={!!deleteTarget} onOpenChange={(v) => !v && setDeleteTarget(null)}>
        <DialogContent className="max-w-sm">
          <DialogHeader>
            <DialogTitle>Delete deployment</DialogTitle>
            <DialogDescription>
              This permanently removes the deployment record and its event history. This action cannot be undone.
            </DialogDescription>
          </DialogHeader>
          {deleteTarget && (
            <div className="space-y-4 pt-1">
              <div className="rounded-md border bg-zinc-50 px-3 py-2 text-sm">
                <p className="font-mono font-medium">{deleteTarget.commit_sha.slice(0, 8)}</p>
                {deleteTarget.commit_message && (
                  <p className="text-xs text-muted-foreground mt-0.5 truncate">{deleteTarget.commit_message}</p>
                )}
              </div>
              <div className="space-y-1.5">
                <Label className="text-xs">
                  Type <span className="font-mono font-semibold">{deleteTarget.commit_sha.slice(0, 8)}</span> to confirm
                </Label>
                <Input
                  value={deleteConfirmText}
                  onChange={(e) => setDeleteConfirmText(e.target.value)}
                  placeholder={deleteTarget.commit_sha.slice(0, 8)}
                  className="font-mono text-sm"
                  autoComplete="off"
                />
              </div>
            </div>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)}>Cancel</Button>
            <Button
              variant="destructive"
              disabled={deleteConfirmText !== deleteTarget?.commit_sha.slice(0, 8) || deleting}
              onClick={handleDeleteConfirm}
            >
              {deleting ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <Trash2 className="h-4 w-4 mr-2" />}
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Diagnosis result dialog */}
      <Dialog open={!!diagnosisResult} onOpenChange={(v) => !v && setDiagnosisResult(null)}>
        <DialogContent className="max-w-2xl max-h-[80vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              AI Diagnosis
              <ConfidenceBadge score={diagnosisConfidence} />
            </DialogTitle>
            <DialogDescription>Analysis of the last failed deployment</DialogDescription>
          </DialogHeader>
          <pre className="text-xs bg-zinc-950 text-zinc-100 rounded-lg p-4 whitespace-pre-wrap font-mono overflow-x-auto">
            {diagnosisResult}
          </pre>
          <EvidenceSection items={diagnosisEvidence} />
          <DialogFooter>
            <Button variant="outline" onClick={() => setDiagnosisResult(null)}>Close</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Scale dialog */}
      <Dialog open={!!scaleTarget} onOpenChange={(v) => !v && setScaleTarget(null)}>
        <DialogContent className="max-w-sm">
          <DialogHeader>
            <DialogTitle className="capitalize">Scale {scaleTarget?.name}</DialogTitle>
            <DialogDescription>Set the number of ECS task replicas.</DialogDescription>
          </DialogHeader>
          <div className="space-y-3 pt-1">
            <div className="space-y-1.5">
              <Label className="text-xs">Replicas (0 – 10)</Label>
              <Input
                type="number" min={0} max={10}
                value={scaleReplicas}
                onChange={(e) => setScaleReplicas(Number(e.target.value))}
              />
            </div>
          </div>
          <DialogFooter className="pt-2">
            <Button variant="outline" onClick={() => setScaleTarget(null)} disabled={scaling}>Cancel</Button>
            <Button onClick={handleScale} disabled={scaling}>
              {scaling ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
              {scaling ? "Scaling..." : "Apply"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Add webhook dialog */}
      <Dialog open={hookDialog} onOpenChange={(v) => { if (!v) { setHookDialog(false); setNewHookUrl(""); setNewHookSecret(""); setNewHookEvents([]); } }}>
        <DialogContent className="max-w-sm">
          <DialogHeader>
            <DialogTitle>Add Webhook</DialogTitle>
            <DialogDescription>ConvDeploy will POST to this URL on deploy events.</DialogDescription>
          </DialogHeader>
          <div className="space-y-3 pt-1">
            <div className="space-y-1.5">
              <Label className="text-xs">URL *</Label>
              <Input placeholder="https://hooks.example.com/..." value={newHookUrl} onChange={(e) => setNewHookUrl(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">Secret (optional — used for HMAC-SHA256 signature)</Label>
              <Input placeholder="••••••••" type="password" value={newHookSecret} onChange={(e) => setNewHookSecret(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">Events *</Label>
              {(["deploy.started", "deploy.succeeded", "deploy.failed"] as const).map((ev) => (
                <label key={ev} className="flex items-center gap-2 text-sm cursor-pointer">
                  <input
                    type="checkbox"
                    checked={newHookEvents.includes(ev)}
                    onChange={(e) => setNewHookEvents(e.target.checked ? [...newHookEvents, ev] : newHookEvents.filter((x) => x !== ev))}
                    className="h-3.5 w-3.5"
                  />
                  <span className="font-mono text-xs">{ev}</span>
                </label>
              ))}
            </div>
          </div>
          <DialogFooter className="pt-2">
            <Button variant="outline" onClick={() => setHookDialog(false)} disabled={savingHook}>Cancel</Button>
            <Button onClick={handleCreateHook} disabled={savingHook || !newHookUrl || newHookEvents.length === 0}>
              {savingHook ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
              {savingHook ? "Saving..." : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Environment creation dialog */}
      <Dialog open={!!envDialog} onOpenChange={(v) => !v && setEnvDialog(null)}>
        <DialogContent className="max-w-sm">
          <DialogHeader>
            <DialogTitle className="capitalize">Add {envDialog} environment</DialogTitle>
          </DialogHeader>
          <div className="space-y-4 pt-1">
            <div className="space-y-1.5">
              <Label className="text-xs">AWS Region</Label>
              <select
                value={envRegion}
                onChange={(e) => setEnvRegion(e.target.value)}
                className="w-full h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
              >
                {AWS_REGIONS.map((r) => (
                  <option key={r} value={r}>{r}</option>
                ))}
              </select>
            </div>
            <Button className="w-full" onClick={handleCreateEnv} disabled={creatingEnv}>
              {creatingEnv ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : null}
              {creatingEnv ? "Creating..." : "Create & Provision"}
            </Button>
          </div>
        </DialogContent>
      </Dialog>

      {/* Mobile alert banner — shown when an alert fires and the side panel is hidden */}
      {alertBanner && (
        <div className="lg:hidden sticky top-0 z-40 bg-red-600 text-white px-4 py-2.5 flex items-center justify-between gap-2 text-sm">
          <span className="truncate">⚠️ {alertBanner.title}{alertBanner.summary ? ` — ${alertBanner.summary}` : ""}</span>
          <span className="flex gap-2 shrink-0">
            <button className="underline" onClick={() => setStatusDrawerOpen(true)}>View</button>
            <button className="opacity-80" onClick={() => setAlertBanner(null)}>Dismiss</button>
          </span>
        </div>
      )}

      {/* Tablet/mobile status drawer */}
      {statusDrawerOpen && (
        <div className="xl:hidden fixed inset-0 z-50 flex">
          <div className="absolute inset-0 bg-black/40" onClick={() => setStatusDrawerOpen(false)} />
          <div className="relative z-10 h-full overflow-y-auto bg-white shadow-xl">
            <StatusSidebar
              projectId={id}
              environments={environments}
              deployments={deployments}
              alerts={alerts}
              onSelectEnvironment={() => setStatusDrawerOpen(false)}
            />
          </div>
        </div>
      )}

      <div className="flex items-stretch">
        {/* LEFT — live status (desktop only) */}
        <div className="hidden xl:block sticky top-0 self-start h-screen">
          <StatusSidebar
            projectId={id}
            environments={environments}
            deployments={deployments}
            alerts={alerts}
          />
        </div>

      <main className="flex-1 min-w-0 max-w-5xl mx-auto px-4 py-10">
        {/* Header */}
        <div className="flex items-start justify-between mb-6 gap-3 flex-wrap">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">{project.name}</h1>
            <p className="text-muted-foreground text-sm mt-1">
              {project.repo_owner}/{project.repo_name}
              {project.branch && <span className="ml-2 font-mono text-xs bg-zinc-100 px-1.5 py-0.5 rounded">{project.branch}</span>}
            </p>
          </div>
          <div className="flex gap-2">
            <Button variant="outline" className="xl:hidden" onClick={() => setStatusDrawerOpen(true)}>
              <Activity className="h-4 w-4 mr-2" />
              Status
              {alerts.length > 0 && (
                <span className="ml-1.5 inline-flex items-center justify-center h-4 min-w-4 px-1 rounded-full bg-red-500 text-white text-[10px] font-semibold">
                  {alerts.length}
                </span>
              )}
            </Button>
            <Button variant="outline" nativeButton={false} render={<Link href={`/projects/${id}/chat`} />}>
              <MessageSquare className="h-4 w-4 mr-2" />
              Open Chat
            </Button>
          </div>
        </div>

        {/* AI proposed an action — persistent banner until reviewed. */}
        {actionBanner && pendingActions.length > 0 && (
          <div className="mb-6 rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 flex items-center justify-between gap-2 text-sm text-amber-900">
            <span>🤖 OpsPilot has proposed {pendingActions.length === 1 ? "an action" : `${pendingActions.length} actions`} — review and approve in the Pending Approvals panel.</span>
            <button className="underline shrink-0" onClick={() => setActionBanner(false)}>Dismiss</button>
          </div>
        )}

        {/* View-only banner — shown to viewers; action buttons are disabled. */}
        {isViewer && (
          <div className="mb-6 rounded-lg border border-zinc-200 bg-zinc-50 px-4 py-3 flex items-center gap-2 text-sm text-zinc-600">
            <EyeOff className="h-4 w-4 shrink-0" />
            <span>You have <strong>view-only</strong> access to this workspace. Deploy, rollback, scale, and other actions are disabled — ask an admin for the engineer role.</span>
          </div>
        )}

        {/* No AWS account banner */}
        {!project.account_id && (
          <div className="mb-6 rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 flex items-center justify-between">
            <div className="flex items-center gap-2 text-sm text-amber-800">
              <Cloud className="h-4 w-4 shrink-0" />
              <span>No AWS account linked — connect one to start deploying.</span>
            </div>
            <Button size="sm" variant="outline" nativeButton={false} render={<Link href="/aws-accounts" />}>
              Manage Accounts
            </Button>
          </div>
        )}

        {/* High-risk deploy banner (advisory, from the deploy_risk WS message) */}
        {currentRiskScore && (currentRiskScore.level === "high" || currentRiskScore.level === "critical") && (
          <div className="mb-6 rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-900">
            <p
              className="font-medium"
              title={`Score calculated from ${currentRiskScore.factors.filter((f) => f.points > 0).length} factor(s). See the breakdown below.`}
            >
              ⚠️ {currentRiskScore.level === "critical" ? "Critical" : "High"} risk deploy (score {currentRiskScore.score}/100)
            </p>
            {(currentRiskScore.explanation || currentRiskScore.top_factor) && (
              <p className="mt-0.5">{currentRiskScore.explanation || currentRiskScore.top_factor}</p>
            )}
            <ul className="mt-1.5 text-xs space-y-0.5 text-amber-800">
              {currentRiskScore.factors.filter((f) => f.points > 0).map((f) => (
                <li key={f.name}>• {f.reason}</li>
              ))}
            </ul>
          </div>
        )}

        <Tabs defaultValue="overview">
          <TabsList className="mb-6">
            <TabsTrigger value="overview">Overview</TabsTrigger>
            <TabsTrigger value="logs">Live Logs</TabsTrigger>
            <TabsTrigger value="deployments">Deployments</TabsTrigger>
            <TabsTrigger value="env-vars" onClick={() => fetchEnvVars()}>Env Vars</TabsTrigger>
            <TabsTrigger value="terminal">Terminal</TabsTrigger>
            <TabsTrigger value="webhooks" onClick={() => loadHooks()}>Webhooks</TabsTrigger>
            <TabsTrigger value="infrastructure" onClick={() => loadResources()}>Infrastructure</TabsTrigger>
            <TabsTrigger value="costs" onClick={() => loadCosts()}>Costs</TabsTrigger>
            <TabsTrigger value="actions" onClick={() => loadActions()}>Actions</TabsTrigger>
            <TabsTrigger value="settings">Settings</TabsTrigger>
          </TabsList>

          {/* ── Overview (environments) ── */}
          <TabsContent value="overview">
            <div className="space-y-4">
              {activeDeployment && (() => {
                const stage = inferDeployStage(deployLog, activeDeployment);
                const latestMsg =
                  deployLog.length > 0
                    ? deployLog[deployLog.length - 1]
                    : activeDeployment.status === "pending"
                      ? "Queued — waiting for a build worker..."
                      : "Deployment in progress...";
                return (
                  <Card className="border-indigo-200 bg-indigo-50/40">
                    <CardContent className="py-4 space-y-3">
                      <div className="flex items-center justify-between gap-3 flex-wrap">
                        <div className="flex items-center gap-2 text-sm font-medium">
                          <Loader2 className="h-4 w-4 animate-spin text-indigo-600" />
                          Deploying{" "}
                          <span className="font-mono text-xs bg-white border rounded px-1.5 py-0.5">
                            {activeDeployment.commit_sha.slice(0, 8)}
                          </span>
                          {activeDeployment.commit_message && (
                            <span className="text-muted-foreground font-normal truncate max-w-[18rem]">
                              {activeDeployment.commit_message}
                            </span>
                          )}
                        </div>
                        <Badge variant="outline" className="capitalize">{activeDeployment.status}</Badge>
                      </div>

                      {/* Stage checklist */}
                      <div className="flex items-center gap-2 text-xs">
                        {DEPLOY_STAGES.map((label, i) => {
                          const stepNum = i + 1;
                          const done = stage > stepNum;
                          const current = stage === stepNum;
                          return (
                            <div key={label} className="flex items-center gap-2">
                              {i > 0 && <div className={cn("h-px w-6", stage >= stepNum ? "bg-indigo-400" : "bg-zinc-300")} />}
                              <span
                                className={cn(
                                  "flex items-center gap-1.5",
                                  done && "text-green-700",
                                  current && "text-indigo-700 font-medium",
                                  !done && !current && "text-muted-foreground"
                                )}
                              >
                                {done ? (
                                  <CheckCircle2 className="h-3.5 w-3.5" />
                                ) : current ? (
                                  <Loader2 className="h-3.5 w-3.5 animate-spin" />
                                ) : (
                                  <Clock className="h-3.5 w-3.5" />
                                )}
                                {label}
                              </span>
                            </div>
                          );
                        })}
                      </div>

                      {/* Progress bar + latest message */}
                      <div className="space-y-1.5">
                        <div className="h-1.5 w-full rounded-full bg-zinc-200 overflow-hidden">
                          <div
                            className="h-full rounded-full bg-indigo-500 transition-all duration-700"
                            style={{ width: `${DEPLOY_STAGE_PROGRESS[stage]}%` }}
                          />
                        </div>
                        <p className="text-xs text-muted-foreground">{latestMsg}</p>
                      </div>

                      {/* Live build output (CodeBuild logs streamed over WS) */}
                      {buildLogs.length > 0 && (
                        <div>
                          <button
                            onClick={() => setShowBuildOutput((v) => !v)}
                            className="text-xs text-indigo-700 hover:underline"
                          >
                            {showBuildOutput ? "Hide build output" : `Show build output (${buildLogs.length} lines)`}
                          </button>
                          {showBuildOutput && (
                            <pre
                              className="mt-2 rounded-md bg-zinc-950 text-zinc-200 text-[11px] font-mono p-3 overflow-y-auto whitespace-pre-wrap"
                              style={{ maxHeight: 300 }}
                              ref={(el) => { if (el) el.scrollTop = el.scrollHeight; }}
                            >
                              {buildLogs.slice(-50).join("\n")}
                            </pre>
                          )}
                        </div>
                      )}

                      <div className="flex justify-end">
                        <Button
                          size="sm"
                          variant="outline"
                          className="h-7 text-xs text-red-600 hover:text-red-700"
                          onClick={() => handleCancelDeployment(activeDeployment)}
                        >
                          Cancel deployment
                        </Button>
                      </div>
                    </CardContent>
                  </Card>
                );
              })()}

              {healthScore && environments.length > 0 && (
                <Card>
                  <CardContent className="py-4">
                    <div className="flex items-center justify-between gap-4 flex-wrap">
                      <div className="flex items-center gap-3">
                        <div
                          className={cn(
                            "flex h-12 w-12 items-center justify-center rounded-full text-base font-bold",
                            healthScore.grade === "healthy" && "bg-green-100 text-green-700",
                            healthScore.grade === "degraded" && "bg-amber-100 text-amber-700",
                            healthScore.grade === "at_risk" && "bg-orange-100 text-orange-700",
                            healthScore.grade === "critical" && "bg-red-100 text-red-700"
                          )}
                        >
                          {healthScore.score}
                        </div>
                        <div>
                          <p className="text-sm font-medium">
                            Deployment health:{" "}
                            <span className="capitalize">{healthScore.grade.replace("_", " ")}</span>
                          </p>
                          <p className="text-xs text-muted-foreground">
                            Based on recent deploy success, environment status, and incidents
                          </p>
                        </div>
                      </div>
                      {healthScore.insights.length > 0 && (
                        <ul className="text-xs text-muted-foreground space-y-0.5 max-w-md">
                          {healthScore.insights.slice(0, 2).map((insight) => (
                            <li key={insight}>• {insight}</li>
                          ))}
                        </ul>
                      )}
                    </div>
                  </CardContent>
                </Card>
              )}

              {environments.length === 0 && (
                <Card className="border-dashed">
                  <CardContent className="flex flex-col items-center justify-center py-12 text-center">
                    <Rocket className="h-10 w-10 text-zinc-300 mb-3" />
                    <p className="font-medium text-sm">No environments yet</p>
                    <p className="text-muted-foreground text-xs mt-1 mb-4">
                      Create a staging or production environment to get started.
                    </p>
                    <div className="flex gap-2">
                      {!existingEnvNames.includes("staging") && (
                        <Button size="sm" variant="outline" onClick={() => setEnvDialog("staging")}>
                          <Plus className="h-3 w-3 mr-1" /> Staging
                        </Button>
                      )}
                      {!existingEnvNames.includes("production") && (
                        <Button size="sm" onClick={() => setEnvDialog("production")}>
                          <Plus className="h-3 w-3 mr-1" /> Production
                        </Button>
                      )}
                    </div>
                  </CardContent>
                </Card>
              )}

              {environments.filter((env) => !env.is_preview).map((env) => {
                const badge = STACK_BADGE[env.stack_status];
                return (
                  <Card key={env.id}>
                    <CardHeader>
                      <div className="flex items-center justify-between">
                        <div className="flex items-center gap-3">
                          <CardTitle className="text-base capitalize">{env.name}</CardTitle>
                          <Badge variant={badge.variant}>{badge.label}</Badge>
                          {env.stack_status === "provisioning" && (
                            <Loader2 className="h-3 w-3 animate-spin text-muted-foreground" />
                          )}
                        </div>
                        <div className="flex gap-2">
                          {env.stack_status === "ready" && (
                            <>
                              <Button
                                size="sm"
                                variant="outline"
                                disabled={checkingHealth === env.id}
                                onClick={() => handleCheckHealth(env)}
                                title="Check ECS service health"
                              >
                                {checkingHealth === env.id
                                  ? <Loader2 className="h-3 w-3 mr-1 animate-spin" />
                                  : <Activity className="h-3 w-3 mr-1" />
                                }
                                Health
                              </Button>
                              <Button
                                size="sm"
                                variant="outline"
                                onClick={() => { setScaleTarget(env); setScaleReplicas(1); }}
                                title="Scale ECS service"
                              >
                                <Scaling className="h-3 w-3 mr-1" />
                                Scale
                              </Button>
                              <Button
                                size="sm"
                                onClick={() => handleDeploy(env)}
                                disabled={deploying === env.id}
                              >
                                {deploying === env.id
                                  ? <Loader2 className="h-3 w-3 mr-1 animate-spin" />
                                  : <Rocket className="h-3 w-3 mr-1" />
                                }
                                Deploy
                              </Button>
                            </>
                          )}
                          {env.stack_status === "failed" && (
                            <Button
                              size="sm"
                              variant="destructive"
                              disabled={retrying === env.id}
                              onClick={() => handleRetry(env)}
                            >
                              {retrying === env.id
                                ? <Loader2 className="h-3 w-3 mr-1 animate-spin" />
                                : <RotateCcw className="h-3 w-3 mr-1" />
                              }
                              Retry
                            </Button>
                          )}
                        </div>
                      </div>
                      {env.alb_dns && (
                        <CardDescription>
                          <a
                            href={`http://${env.alb_dns}`}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="text-indigo-600 hover:underline text-xs"
                          >
                            http://{env.alb_dns}
                          </a>
                        </CardDescription>
                      )}
                    </CardHeader>

                    {env.stack_status === "provisioning" && (
                      <CardContent className="pt-0">
                        {/* Step indicators */}
                        <div className="flex items-center gap-1 mb-3">
                          {PROVISION_STEPS.map((label, i) => {
                            const step = i + 1;
                            const current = inferProvisionStep(provisionLog);
                            const done = current > step;
                            const active = current === step || (current === 0 && step === 1);
                            return (
                              <div key={label} className="flex items-center gap-1 flex-1 min-w-0">
                                <div className={`flex items-center gap-1 text-xs shrink-0 ${
                                  done   ? "text-green-600" :
                                  active ? "text-blue-600"  : "text-zinc-400"
                                }`}>
                                  {done
                                    ? <CheckCircle2 className="h-3 w-3 shrink-0" />
                                    : active
                                      ? <Loader2 className="h-3 w-3 shrink-0 animate-spin" />
                                      : <div className="h-3 w-3 rounded-full border border-current shrink-0" />
                                  }
                                  <span className="truncate">{label}</span>
                                </div>
                                {i < PROVISION_STEPS.length - 1 && (
                                  <div className="flex-1 h-px bg-zinc-200 mx-1" />
                                )}
                              </div>
                            );
                          })}
                        </div>

                        {/* Progress bar */}
                        <div className="h-1.5 bg-zinc-100 rounded-full overflow-hidden mb-3">
                          <div
                            className="h-full bg-blue-500 rounded-full transition-all duration-700 ease-out"
                            style={{ width: `${STEP_PROGRESS[inferProvisionStep(provisionLog)]}%` }}
                          />
                        </div>

                        {/* Latest log line */}
                        <p className="text-xs text-muted-foreground truncate">
                          {provisionLog.length > 0
                            ? provisionLog[provisionLog.length - 1]
                            : "Waiting for provisioning job to start…"
                          }
                        </p>
                        <p className="text-xs text-zinc-400 mt-1">
                          First-time setup takes ~3–5 min (VPC + ECS cluster + ALB).
                        </p>
                      </CardContent>
                    )}

                    {env.stack_status === "ready" && (
                      <CardContent>
                        <div className="grid grid-cols-2 sm:grid-cols-3 gap-3 text-xs">
                          {[
                            ["ECS Cluster", env.ecs_cluster_name],
                            ["ECR Repo", env.ecr_repo_uri?.split("/").pop()],
                            ["CodeBuild", env.codebuild_project_name],
                            ["Log Group", env.log_group_name],
                            ["Region", env.aws_region],
                          ].map(([label, value]) => (
                            value && (
                              <div key={label} className="flex flex-col gap-0.5">
                                <span className="text-muted-foreground">{label}</span>
                                <span className="font-mono truncate">{value}</span>
                              </div>
                            )
                          ))}
                        </div>
                        {healthData[env.id] && (
                          <div className="mt-3 pt-3 border-t flex items-center gap-4 text-xs">
                            <span className={`font-medium ${healthData[env.id].status === "running" ? "text-green-600" : "text-amber-600"}`}>
                              {healthData[env.id].status}
                            </span>
                            <span className="text-muted-foreground">
                              Running <strong>{healthData[env.id].running}</strong>
                              {" / "}Desired <strong>{healthData[env.id].desired}</strong>
                              {healthData[env.id].pending > 0 && <> · Pending <strong>{healthData[env.id].pending}</strong></>}
                            </span>
                            {healthData[env.id].url && (
                              <a href={healthData[env.id].url} target="_blank" rel="noopener noreferrer"
                                className="text-indigo-600 hover:underline font-mono truncate">
                                {healthData[env.id].url}
                              </a>
                            )}
                          </div>
                        )}
                      </CardContent>
                    )}
                  </Card>
                );
              })}

              {environments.length > 0 && (
                <div className="flex gap-2 flex-wrap">
                  {!existingEnvNames.includes("staging") && (
                    <Button size="sm" variant="outline" onClick={() => setEnvDialog("staging")}>
                      <Plus className="h-3 w-3 mr-1" /> Add Staging
                    </Button>
                  )}
                  {!existingEnvNames.includes("production") && (
                    <Button size="sm" variant="outline" onClick={() => setEnvDialog("production")}>
                      <Plus className="h-3 w-3 mr-1" /> Add Production
                    </Button>
                  )}
                </div>
              )}

              {/* PR Preview Environments section */}
              {environments.some((e) => e.is_preview) && (
                <div className="space-y-2">
                  <p className="text-sm font-medium text-muted-foreground flex items-center gap-1.5">
                    <GitPullRequest className="h-3.5 w-3.5" /> PR Preview Environments
                  </p>
                  {environments.filter((e) => e.is_preview).map((env) => (
                    <Card key={env.id} className="border-dashed">
                      <CardHeader className="py-3">
                        <div className="flex items-center justify-between">
                          <div className="flex items-center gap-2">
                            <Badge variant="outline" className="text-xs font-mono">PR #{env.pr_number}</Badge>
                            <span className="text-sm font-mono text-muted-foreground">{env.pr_branch}</span>
                            <Badge variant={env.stack_status === "ready" ? "default" : "secondary"} className="text-xs">
                              {env.stack_status}
                            </Badge>
                          </div>
                          {env.stack_status === "ready" && env.alb_dns && env.pr_number && (
                            <a
                              href={`http://${env.alb_dns}/pr-${env.pr_number}/`}
                              target="_blank" rel="noopener noreferrer"
                              className="text-xs text-indigo-600 hover:underline font-mono"
                            >
                              Open preview →
                            </a>
                          )}
                        </div>
                        {env.pr_head_sha && (
                          <CardDescription className="text-xs font-mono">{env.pr_head_sha.slice(0, 8)}</CardDescription>
                        )}
                      </CardHeader>
                    </Card>
                  ))}
                </div>
              )}

              {/* PR Previews toggle */}
              {project.account_id && (
                <Card className="border-dashed bg-zinc-50/50">
                  <CardContent className="flex items-center justify-between py-4">
                    <div className="flex items-center gap-3">
                      <Zap className={`h-5 w-5 ${project.previews_enabled ? "text-green-500" : "text-zinc-400"}`} />
                      <div>
                        <p className="text-sm font-medium">PR Preview Environments</p>
                        <p className="text-xs text-muted-foreground mt-0.5">
                          {project.previews_enabled
                            ? "Enabled — a preview deploys on every PR push."
                            : "Auto-deploy a live preview for every pull request on your own AWS."}
                        </p>
                      </div>
                    </div>
                    <Button
                      size="sm"
                      variant={project.previews_enabled ? "destructive" : "default"}
                      disabled={togglingPreviews || !environments.some((e) => e.stack_status === "ready" && !e.is_preview)}
                      onClick={handleTogglePreviews}
                    >
                      {togglingPreviews ? <Loader2 className="h-3 w-3 mr-1 animate-spin" /> : null}
                      {project.previews_enabled ? "Disable" : "Enable"}
                    </Button>
                  </CardContent>
                </Card>
              )}
            </div>
          </TabsContent>

          {/* ── Live Logs ── */}
          <TabsContent value="logs">
            {readyEnvs.length === 0 ? (
              <div className="flex flex-col items-center justify-center py-16 text-center">
                <Cloud className="h-10 w-10 text-zinc-300 mb-3" />
                <p className="font-medium text-sm">No ready environments</p>
                <p className="text-muted-foreground text-xs mt-1">Deploy to an environment first to see logs.</p>
              </div>
            ) : (
              <div className="space-y-4">
                <div className="flex items-center gap-3">
                  <select
                    value={logsEnvId}
                    onChange={(e) => { setLogsEnvId(e.target.value); setLogLines([]); }}
                    className="h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
                  >
                    {readyEnvs.map((e) => (
                      <option key={e.id} value={e.id}>{e.name} ({e.aws_region})</option>
                    ))}
                  </select>
                  <Button size="sm" onClick={fetchLogs} disabled={loadingLogs}>
                    {loadingLogs
                      ? <Loader2 className="h-3.5 w-3.5 mr-1.5 animate-spin" />
                      : <RefreshCw className="h-3.5 w-3.5 mr-1.5" />
                    }
                    {logLines.length === 0 ? "Load Logs" : "Refresh"}
                  </Button>
                  {selectedLogEnv?.log_group_name && (
                    <span className="text-xs text-muted-foreground font-mono hidden sm:block">
                      {selectedLogEnv.log_group_name}
                    </span>
                  )}
                </div>

                <div className="rounded-lg border bg-zinc-950 overflow-hidden">
                  <div className="px-4 py-2 border-b border-zinc-800 flex items-center justify-between">
                    <span className="text-xs text-zinc-400">Application logs (last 300 lines)</span>
                    {logLines.length > 0 && (
                      <span className="text-xs text-zinc-500">{logLines.length} lines</span>
                    )}
                  </div>
                  <div className="h-[500px] overflow-y-auto p-4 font-mono text-xs text-zinc-300 leading-relaxed">
                    {logLines.length === 0 ? (
                      <p className="text-zinc-600 text-center pt-8">
                        {loadingLogs ? "Loading..." : "Click \"Load Logs\" to fetch application logs"}
                      </p>
                    ) : (
                      logLines.map((line, i) => (
                        <div key={i} className="whitespace-pre-wrap break-all hover:bg-zinc-900 px-1 -mx-1 rounded">
                          {line}
                        </div>
                      ))
                    )}
                    <div ref={logsEndRef} />
                  </div>
                </div>
                <p className="text-xs text-muted-foreground">
                  These are the last 300 lines from your running ECS tasks. Click Refresh to pull the latest.
                </p>
              </div>
            )}
          </TabsContent>

          {/* ── Deployments ── */}
          <TabsContent value="deployments">
            {deployments.length === 0 ? (
              <div className="flex flex-col items-center justify-center py-16 text-center">
                <Rocket className="h-10 w-10 text-zinc-300 mb-3" />
                <p className="font-medium text-sm">No deployments yet</p>
                <p className="text-muted-foreground text-xs mt-1">
                  Deploy from the Overview tab or through the chat.
                </p>
              </div>
            ) : (
              <div className="rounded-lg border bg-white overflow-hidden">
                {deployments.map((dep, i) => {
                  const isExpanded = expandedDepId === dep.id;
                  const events = depEvents[dep.id] ?? [];
                  return (
                    <div key={dep.id}>
                      {i > 0 && <Separator />}
                      <div className="flex items-center justify-between px-4 py-3">
                        <div className="flex items-center gap-3 min-w-0">
                          {DEPLOY_ICON[dep.status]}
                          <div className="min-w-0">
                            <p className="text-sm font-mono font-medium">{dep.commit_sha.slice(0, 8)}</p>
                            {dep.commit_message && (
                              <p className="text-xs text-muted-foreground truncate max-w-xs">
                                {dep.commit_message}
                              </p>
                            )}
                          </div>
                        </div>
                        <div className="flex items-center gap-2 shrink-0">
                          <span className="text-xs text-muted-foreground">{timeAgo(dep.created_at)}</span>
                          <Badge
                            variant={dep.status === "live" ? "default" : dep.status === "failed" ? "destructive" : "secondary"}
                            className="text-xs"
                          >
                            {dep.status}
                          </Badge>
                          {dep.status === "live" && (
                            <Button size="sm" variant="outline" onClick={() => handleRollback(dep)}>
                              <RotateCcw className="h-3 w-3 mr-1" />
                              Rollback
                            </Button>
                          )}
                          {dep.status === "failed" && (
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={diagnosing === dep.id}
                              onClick={() => handleDiagnose(dep)}
                            >
                              {diagnosing === dep.id
                                ? <Loader2 className="h-3 w-3 mr-1 animate-spin" />
                                : <Activity className="h-3 w-3 mr-1" />
                              }
                              Diagnose
                            </Button>
                          )}
                          {(dep.status === "live" || dep.status === "failed" || dep.status === "rolled_back") && (
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={redeploying === dep.id}
                              onClick={() => handleRedeploy(dep)}
                            >
                              {redeploying === dep.id
                                ? <Loader2 className="h-3 w-3 mr-1 animate-spin" />
                                : <RefreshCw className="h-3 w-3 mr-1" />
                              }
                              Redeploy
                            </Button>
                          )}
                          {(dep.status === "building" || dep.status === "deploying" || dep.status === "pending") && (
                            <Button
                              size="sm"
                              variant="outline"
                              className="text-red-600 hover:text-red-700"
                              onClick={() => handleCancelDeployment(dep)}
                            >
                              <XCircle className="h-3 w-3 mr-1" />
                              Cancel
                            </Button>
                          )}
                          {dep.status !== "building" && dep.status !== "deploying" && (
                            <Button
                              size="sm"
                              variant="ghost"
                              className="text-red-500 hover:text-red-600 hover:bg-red-50 px-2"
                              onClick={() => handleDeleteOpen(dep)}
                            >
                              <Trash2 className="h-3 w-3" />
                            </Button>
                          )}
                          <button
                            onClick={() => handleToggleTimeline(dep)}
                            className="text-muted-foreground hover:text-foreground transition-colors p-0.5"
                            title="Toggle event timeline"
                          >
                            {isExpanded
                              ? <ChevronDown className="h-4 w-4" />
                              : <ChevronRight className="h-4 w-4" />
                            }
                          </button>
                        </div>
                      </div>

                      {dep.failure_reason && (
                        <div className="px-4 pb-3">
                          <pre className="text-xs text-red-600 bg-red-50 rounded px-2 py-1 whitespace-pre-wrap font-mono overflow-x-auto max-h-48 overflow-y-auto">
                            {dep.failure_reason}
                          </pre>
                        </div>
                      )}

                      {isExpanded && (
                        <div className="px-4 pb-4 pt-1 bg-zinc-50 border-t border-zinc-100">
                          {loadingDepEvents === dep.id ? (
                            <div className="flex items-center gap-2 py-3 text-xs text-muted-foreground">
                              <Loader2 className="h-3 w-3 animate-spin" />
                              Loading timeline...
                            </div>
                          ) : events.length === 0 ? (
                            <p className="py-3 text-xs text-muted-foreground">No events recorded for this deployment.</p>
                          ) : (
                            <ol className="ml-1 mt-3">
                              {events.map((ev, idx) => (
                                <li key={ev.id} className="flex items-start gap-3 pb-3">
                                  <div className="flex flex-col items-center shrink-0">
                                    <span className={`mt-0.5 h-2 w-2 rounded-full ${
                                      ev.severity === "error" ? "bg-red-500" :
                                      ev.severity === "warn"  ? "bg-amber-500" :
                                      "bg-zinc-400"
                                    }`} />
                                    {idx < events.length - 1 && (
                                      <span className="w-px bg-zinc-200 mt-1" style={{ minHeight: "14px" }} />
                                    )}
                                  </div>
                                  <div className="min-w-0 -mt-0.5 pb-0.5">
                                    <p className="text-xs font-medium leading-none">
                                      {EVENT_LABELS[ev.event_type] ?? ev.event_type}
                                    </p>
                                    {ev.payload?.reason != null && (
                                      <p className="text-xs text-muted-foreground mt-1 break-words">
                                        {String(ev.payload.reason)}
                                      </p>
                                    )}
                                    <p className="text-xs text-zinc-400 mt-0.5">
                                      {new Date(ev.occurred_at).toLocaleTimeString()}
                                    </p>
                                  </div>
                                </li>
                              ))}
                            </ol>
                          )}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </TabsContent>

          {/* ── Env Vars ── */}
          <TabsContent value="env-vars">
            {environments.length === 0 ? (
              <div className="flex flex-col items-center justify-center py-16 text-center">
                <KeyRound className="h-10 w-10 text-zinc-300 mb-3" />
                <p className="font-medium text-sm">No environments yet</p>
                <p className="text-muted-foreground text-xs mt-1">Create an environment first.</p>
              </div>
            ) : (
              <div className="space-y-5">
                <div className="flex items-center gap-3">
                  <select
                    value={envVarEnvId}
                    onChange={(e) => { setEnvVarEnvId(e.target.value); setEnvVars([]); setShowSecretValues({}); setRevealedValues({}); fetchEnvVars(e.target.value); }}
                    className="h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
                  >
                    {environments.map((e) => (
                      <option key={e.id} value={e.id}>{e.name} ({e.aws_region})</option>
                    ))}
                  </select>
                  <p className="text-xs text-muted-foreground">
                    Env vars are injected into your container at deploy time. <strong>Redeploy</strong> after any change.
                  </p>
                </div>

                {/* Add new var */}
                <div className="rounded-lg border bg-white p-4 space-y-3">
                  <p className="text-sm font-medium">Add / update variable</p>
                  <div className="flex gap-2 flex-wrap">
                    <Input
                      placeholder="KEY"
                      value={newKey}
                      onChange={(e) => setNewKey(e.target.value)}
                      className="font-mono text-sm flex-1 min-w-32"
                    />
                    <Input
                      placeholder="value"
                      value={newValue}
                      onChange={(e) => setNewValue(e.target.value)}
                      type={newIsSecret ? "password" : "text"}
                      className="font-mono text-sm flex-1 min-w-48"
                    />
                    <button
                      type="button"
                      onClick={() => setNewIsSecret((v) => !v)}
                      className={`flex items-center gap-1.5 px-3 rounded-md border text-xs transition-colors ${newIsSecret ? "bg-amber-50 border-amber-300 text-amber-800" : "border-input text-muted-foreground hover:bg-zinc-50"}`}
                      title="Toggle secret"
                    >
                      {newIsSecret ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
                      {newIsSecret ? "Secret" : "Plain"}
                    </button>
                    <Button
                      size="sm"
                      disabled={!newKey.trim() || !newValue.trim() || savingEnvVar}
                      onClick={handleSaveEnvVar}
                    >
                      {savingEnvVar ? <Loader2 className="h-3.5 w-3.5 animate-spin mr-1" /> : <Plus className="h-3.5 w-3.5 mr-1" />}
                      Save
                    </Button>
                  </div>
                </div>

                {/* Existing vars */}
                <div className="rounded-lg border bg-white overflow-hidden">
                  {loadingEnvVars ? (
                    <div className="flex items-center gap-2 px-4 py-6 text-sm text-muted-foreground">
                      <Loader2 className="h-4 w-4 animate-spin" /> Loading…
                    </div>
                  ) : envVars.length === 0 ? (
                    <p className="px-4 py-6 text-sm text-muted-foreground text-center">No env vars set for this environment.</p>
                  ) : (
                    envVars.map((v, i) => (
                      <div key={v.id}>
                        {i > 0 && <Separator />}
                        <div className="flex items-center gap-3 px-4 py-2.5">
                          <span className="font-mono text-sm font-medium flex-1 truncate">{v.key}</span>
                          <span className="font-mono text-sm text-muted-foreground flex-1 truncate">
                            {v.is_secret
                              ? (showSecretValues[v.id] ? (revealedValues[v.id] ?? "•••••••••") : "•••••••••")
                              : v.value}
                          </span>
                          <div className="flex items-center gap-1 shrink-0">
                            {v.is_secret && (
                              <Badge variant="outline" className="text-xs px-1.5 py-0">secret</Badge>
                            )}
                            {v.is_secret && (
                              <button
                                onClick={() => toggleRevealSecret(v)}
                                className="p-1 text-muted-foreground hover:text-foreground"
                                title={showSecretValues[v.id] ? "Hide value" : "Show value"}
                              >
                                {showSecretValues[v.id] ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
                              </button>
                            )}
                            <button
                              onClick={() => handleDeleteEnvVar(v)}
                              className="p-1 text-red-400 hover:text-red-600"
                              title="Delete"
                            >
                              <Trash2 className="h-3.5 w-3.5" />
                            </button>
                          </div>
                        </div>
                      </div>
                    ))
                  )}
                </div>
              </div>
            )}
          </TabsContent>

          {/* ── Terminal ── */}
          <TabsContent value="terminal">
            <div className="space-y-4">
              {readyEnvs.length === 0 ? (
                <div className="flex flex-col items-center justify-center py-16 text-center">
                  <Terminal className="h-10 w-10 text-zinc-300 mb-3" />
                  <p className="font-medium text-sm">No running environments</p>
                  <p className="text-muted-foreground text-xs mt-1">Deploy first to access a terminal.</p>
                </div>
              ) : (
                <>
                  <div className="flex items-center gap-3">
                    <select
                      value={terminalEnvId}
                      onChange={(e) => setTerminalEnvId(e.target.value)}
                      className="h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
                    >
                      <option value="">— select environment —</option>
                      {readyEnvs.map((e) => (
                        <option key={e.id} value={e.id}>{e.name} ({e.aws_region})</option>
                      ))}
                    </select>
                    {terminalEnvId && !terminalDisconnected && (
                      <span className="text-xs text-muted-foreground">
                        Connecting to running ECS task via SSM…
                      </span>
                    )}
                    {terminalEnvId && terminalDisconnected && (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => setTerminalNonce((n) => n + 1)}
                      >
                        <RotateCcw className="h-3 w-3 mr-1" />
                        Reconnect
                      </Button>
                    )}
                  </div>
                  <div
                    ref={terminalRef}
                    className="rounded-lg overflow-hidden border border-zinc-800"
                    style={{ height: 420, background: "#0f0f0f" }}
                  />
                  {!terminalEnvId && (
                    <p className="text-xs text-muted-foreground text-center pt-2">
                      Select an environment to open an interactive shell in the running container.
                    </p>
                  )}
                </>
              )}
            </div>
          </TabsContent>
          {/* ── Webhooks ── */}
          <TabsContent value="webhooks">
            <div className="space-y-5">
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-sm font-medium">Webhooks</p>
                  <p className="text-xs text-muted-foreground mt-0.5">
                    ConvDeploy POSTs to these URLs on deploy events. Requests are signed with HMAC-SHA256 when a secret is set.
                  </p>
                </div>
                <Button size="sm" onClick={() => setHookDialog(true)}>
                  <Plus className="h-3 w-3 mr-1" /> Add Webhook
                </Button>
              </div>

              {loadingHooks ? (
                <div className="flex items-center gap-2 py-6 text-sm text-muted-foreground">
                  <Loader2 className="h-4 w-4 animate-spin" /> Loading…
                </div>
              ) : hooksList.length === 0 ? (
                <div className="flex flex-col items-center justify-center py-16 text-center rounded-lg border border-dashed">
                  <WebhookIcon className="h-10 w-10 text-zinc-300 mb-3" />
                  <p className="font-medium text-sm">No webhooks configured</p>
                  <p className="text-muted-foreground text-xs mt-1">Add a webhook to get notified on deploy events.</p>
                </div>
              ) : (
                <div className="rounded-lg border bg-white overflow-hidden">
                  {hooksList.map((hook, i) => (
                    <div key={hook.id}>
                      {i > 0 && <Separator />}
                      <div className="flex items-start justify-between px-4 py-3 gap-3">
                        <div className="min-w-0 flex-1">
                          <p className="text-sm font-mono truncate">{hook.url}</p>
                          <div className="flex items-center gap-1.5 mt-1 flex-wrap">
                            {hook.events.map((ev) => (
                              <Badge key={ev} variant="outline" className="text-xs px-1.5 py-0 font-mono">{ev}</Badge>
                            ))}
                          </div>
                        </div>
                        <div className="flex items-center gap-2 shrink-0">
                          <button
                            onClick={() => handleToggleHook(hook)}
                            className={`flex items-center gap-1 text-xs px-2 py-0.5 rounded-full border transition-colors ${
                              hook.active
                                ? "bg-green-50 border-green-300 text-green-700"
                                : "bg-zinc-100 border-zinc-300 text-zinc-500"
                            }`}
                            title={hook.active ? "Disable webhook" : "Enable webhook"}
                          >
                            {hook.active
                              ? <><CheckCircle2 className="h-3 w-3" /> Active</>
                              : <><ZapOff className="h-3 w-3" /> Inactive</>
                            }
                          </button>
                          <button
                            onClick={() => handleDeleteHook(hook.id)}
                            className="p-1 text-red-400 hover:text-red-600"
                            title="Delete webhook"
                          >
                            <Trash2 className="h-3.5 w-3.5" />
                          </button>
                        </div>
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </TabsContent>

          {/* ── Infrastructure (assigned managed + discovered resources) ── */}
          <TabsContent value="infrastructure">
            <div className="space-y-4">
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-sm font-medium">Infrastructure</p>
                  <p className="text-xs text-muted-foreground">
                    AWS resources assigned to this project — OpsPilot-managed and discovered.
                    Assign more from the{" "}
                    <Link href="/orgs/resources" className="text-indigo-600 hover:underline">inventory</Link>.
                  </p>
                </div>
                <Button size="sm" variant="outline" onClick={loadResources} disabled={loadingResources}>
                  {loadingResources ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5 mr-1" />}
                  Refresh
                </Button>
              </div>

              {loadingResources && resources === null ? (
                <div className="flex items-center justify-center py-12">
                  <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
                </div>
              ) : (resources ?? []).length === 0 ? (
                <Card className="border-dashed">
                  <CardContent className="flex flex-col items-center justify-center py-12 text-center">
                    <Cloud className="h-10 w-10 text-zinc-300 mb-3" />
                    <p className="font-medium text-sm">No infrastructure assigned</p>
                    <p className="text-muted-foreground text-xs mt-1">
                      Discovered AWS resources you assign to this project will show here.
                    </p>
                  </CardContent>
                </Card>
              ) : (
                <div className="rounded-lg border bg-white divide-y">
                  {(resources ?? []).map((r) => {
                    const Icon = RESOURCE_ICONS[r.resource_type] ?? Cloud;
                    return (
                      <div key={r.id} className="flex items-center gap-3 px-4 py-2.5">
                        <Icon className="h-4 w-4 text-zinc-500 shrink-0" />
                        <div className="min-w-0 flex-1">
                          <div className="flex items-center gap-2">
                            <span className="text-sm font-medium truncate">{r.resource_name || r.resource_id}</span>
                            {r.is_managed && (
                              <span className="rounded bg-indigo-100 px-1.5 py-0.5 text-[10px] font-medium text-indigo-700">OpsPilot</span>
                            )}
                          </div>
                          <p className="text-xs text-muted-foreground">
                            {resourceLabel(r.resource_type)} · {r.region || "global"}
                          </p>
                        </div>
                        <Badge variant="outline" className="text-xs capitalize shrink-0">{resourceStatus(r)}</Badge>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          </TabsContent>

          {/* ── Costs ── */}
          <TabsContent value="costs">
            <div className="space-y-5">
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-sm font-medium">AWS Cost Intelligence</p>
                  <p className="text-xs text-muted-foreground mt-0.5">
                    Last 30 days · ConvDeploy-managed services only · 24h data lag
                  </p>
                </div>
                <Button size="sm" variant="outline" onClick={loadCosts} disabled={loadingCosts}>
                  {loadingCosts ? <Loader2 className="h-3.5 w-3.5 mr-1.5 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5 mr-1.5" />}
                  {costs ? "Refresh" : "Load Costs"}
                </Button>
              </div>

              {!project.account_id ? (
                <div className="flex flex-col items-center justify-center py-16 text-center rounded-lg border border-dashed">
                  <Cloud className="h-10 w-10 text-zinc-300 mb-3" />
                  <p className="font-medium text-sm">No AWS account linked</p>
                  <p className="text-muted-foreground text-xs mt-1">Connect an AWS account to see cost data.</p>
                </div>
              ) : !costs ? (
                <div className="flex flex-col items-center justify-center py-16 text-center rounded-lg border border-dashed">
                  <DollarSign className="h-10 w-10 text-zinc-300 mb-3" />
                  <p className="font-medium text-sm">{loadingCosts ? "Loading costs..." : "Click Load Costs to fetch data"}</p>
                  <p className="text-muted-foreground text-xs mt-1">Requires Cost Explorer to be enabled in your AWS account.</p>
                </div>
              ) : (
                <>
                  {/* Total */}
                  <Card>
                    <CardContent className="flex items-center justify-between py-5">
                      <div className="flex items-center gap-3">
                        <div className="h-10 w-10 rounded-full bg-green-100 flex items-center justify-center">
                          <DollarSign className="h-5 w-5 text-green-600" />
                        </div>
                        <div>
                          <p className="text-xs text-muted-foreground">Total (last 30 days)</p>
                          <p className="text-2xl font-bold">${costs.total_monthly_cost.toFixed(2)}</p>
                        </div>
                      </div>
                      <div className="text-xs text-muted-foreground text-right">
                        <p>{costs.period_start}</p>
                        <p>→ {costs.period_end}</p>
                      </div>
                    </CardContent>
                  </Card>

                  {/* Per-service breakdown */}
                  <div className="rounded-lg border bg-white overflow-hidden">
                    {Object.entries(costs.by_service)
                      .filter(([, v]) => v > 0.001)
                      .sort(([, a], [, b]) => b - a)
                      .map(([svc, amount], i) => (
                        <div key={svc}>
                          {i > 0 && <Separator />}
                          <div className="flex items-center justify-between px-4 py-3">
                            <div className="min-w-0 flex-1">
                              <p className="text-sm">{svc}</p>
                              <div className="mt-1.5 h-1.5 bg-zinc-100 rounded-full overflow-hidden">
                                <div
                                  className="h-full bg-indigo-400 rounded-full"
                                  style={{ width: `${Math.min(100, (amount / costs.total_monthly_cost) * 100)}%` }}
                                />
                              </div>
                            </div>
                            <span className="text-sm font-mono font-medium ml-4 shrink-0">
                              ${amount.toFixed(2)}
                            </span>
                          </div>
                        </div>
                      ))}
                    {Object.values(costs.by_service).every((v) => v < 0.001) && (
                      <p className="px-4 py-6 text-sm text-muted-foreground text-center">No costs recorded in this period.</p>
                    )}
                  </div>

                  {costs.total_monthly_cost > 50 && (
                    <div className="rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 flex items-start gap-2 text-sm text-amber-800">
                      <AlertTriangle className="h-4 w-4 shrink-0 mt-0.5" />
                      <span>Tip: scale idle environments to 0 replicas to eliminate compute costs while keeping infrastructure in place.</span>
                    </div>
                  )}
                </>
              )}
            </div>
          </TabsContent>

          {/* ── Actions (AI + human action history) ── */}
          <TabsContent value="actions">
            <div className="space-y-4">
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-sm font-medium">Action history</p>
                  <p className="text-xs text-muted-foreground">AI-proposed and approved actions, their status, and who approved them.</p>
                </div>
                <Button size="sm" variant="outline" onClick={loadActions}>
                  <RefreshCw className="h-3.5 w-3.5 mr-1" /> Refresh
                </Button>
              </div>
              {(actions ?? []).length === 0 ? (
                <Card className="border-dashed">
                  <CardContent className="py-12 text-center">
                    <Activity className="h-10 w-10 text-zinc-300 mx-auto mb-3" />
                    <p className="font-medium text-sm">No actions yet</p>
                    <p className="text-muted-foreground text-xs mt-1">AI-proposed deploys, rollbacks, and scaling will appear here.</p>
                  </CardContent>
                </Card>
              ) : (
                <div className="rounded-lg border bg-white divide-y">
                  {(actions ?? []).map((a) => <ActionRow key={a.id} action={a} />)}
                </div>
              )}
            </div>
          </TabsContent>

          {/* ── Settings ── */}
          <TabsContent value="settings">
            <div className="max-w-2xl space-y-6">
              <Card>
                <CardHeader>
                  <CardTitle className="text-base">Project</CardTitle>
                  <CardDescription className="text-xs">
                    Changes apply to the next deployment.
                  </CardDescription>
                </CardHeader>
                <CardContent className="space-y-4">
                  <div className="space-y-1.5">
                    <Label className="text-xs">Name</Label>
                    <Input value={settingsName} onChange={(e) => setSettingsName(e.target.value)} />
                  </div>
                  <div className="space-y-1.5">
                    <Label className="text-xs">Repository branch</Label>
                    <Input
                      value={settingsBranch}
                      onChange={(e) => setSettingsBranch(e.target.value)}
                      placeholder="default branch"
                      className="font-mono text-sm"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label className="text-xs">Start command</Label>
                    <Input
                      value={settingsStartCmd}
                      onChange={(e) => setSettingsStartCmd(e.target.value)}
                      placeholder="e.g. node server.js"
                      className="font-mono text-sm"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label className="text-xs">Framework</Label>
                    <select
                      value={settingsFramework}
                      onChange={(e) => setSettingsFramework(e.target.value)}
                      className="w-full h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
                    >
                      {["fastapi","flask","django","python","nodejs","express","nextjs","nestjs","remix","nuxtjs","svelte","astro","react-spa","vite","go","rails","spring","static"].map((f) => (
                        <option key={f} value={f}>{f}</option>
                      ))}
                    </select>
                  </div>
                  <Button onClick={handleSaveSettings} disabled={savingSettings}>
                    {savingSettings ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : null}
                    Save changes
                  </Button>
                </CardContent>
              </Card>

              <Card>
                <CardHeader>
                  <CardTitle className="text-base">Notifications</CardTitle>
                  <CardDescription className="text-xs">
                    Email notifications for this account ({me?.email ?? "..."}).
                  </CardDescription>
                </CardHeader>
                <CardContent className="space-y-3">
                  {me ? (
                    <>
                      {([
                        ["Email me when a deploy fails", "deploy_failed", me.notifications.deploy_failed],
                        ["Email me when an alert fires", "alert_fired", me.notifications.alert_fired],
                        ["Email me when a deploy succeeds", "deploy_succeeded", me.notifications.deploy_succeeded],
                      ] as [string, "deploy_failed" | "alert_fired" | "deploy_succeeded", boolean][]).map(([label, key, value]) => (
                        <label key={key} className="flex items-center justify-between gap-3 text-sm cursor-pointer">
                          <span>{label}</span>
                          <input
                            type="checkbox"
                            checked={value}
                            disabled={savingPrefs}
                            onChange={(e) => handleSavePrefs({ [key]: e.target.checked })}
                            className="h-4 w-4 accent-indigo-600"
                          />
                        </label>
                      ))}
                    </>
                  ) : (
                    <p className="text-xs text-muted-foreground">Loading preferences…</p>
                  )}
                </CardContent>
              </Card>

              {/* AI trust levels per environment */}
              <div>
                <h3 className="text-sm font-semibold mb-2">AI Trust Levels</h3>
                <EnvTrustSettings projectId={id} environments={environments} canEdit={isAdmin} />
              </div>

              {/* SLA targets per environment (feeds the analytics dashboard) */}
              <div>
                <h3 className="text-sm font-semibold mb-2">SLA Targets</h3>
                <EnvSLASettings projectId={id} environments={environments} canEdit={canAct} />
              </div>

              <Card className="border-red-200">
                <CardHeader>
                  <CardTitle className="text-base text-red-700">Danger Zone</CardTitle>
                  <CardDescription className="text-xs">
                    Deletes the project, its deployments, and conversation history. AWS resources are torn down in the background.
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  <Button variant="outline" className="text-red-600 hover:text-red-700 border-red-300" disabled={deletingProject} onClick={handleDeleteProject}>
                    {deletingProject ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <Trash2 className="h-4 w-4 mr-2" />}
                    Delete project
                  </Button>
                </CardContent>
              </Card>
            </div>
          </TabsContent>
        </Tabs>
      </main>

        {/* RIGHT — alerts + AI insights (desktop/tablet) */}
        <div className="hidden lg:block sticky top-0 self-start h-screen">
          <AlertsPanel
            alerts={alerts}
            latestInsight={diagnosisResult}
            onSnooze={handleSnoozeAlert}
            onResolve={handleResolveAlert}
            pendingActions={pendingActions}
            canAct={canAct}
            onApproveAction={handleApproveAction}
            onRejectAction={handleRejectAction}
          />
        </div>
      </div>
    </div>
  );
}
