"use client";

// StatusSidebar is the left column of the project command center: live
// per-environment task counts, last-deploy times, open-alert badges, and a
// recent-activity feed across all environments.

import { useEffect, useState } from "react";
import { useAuth } from "@clerk/nextjs";
import { Badge } from "@/components/ui/badge";
import { checkHealth, getProjectEvents } from "@/lib/api";
import type { Environment, Deployment, Alert, OperationalEvent } from "@/types/api";
import { cn } from "@/lib/utils";
import { Activity } from "lucide-react";

const STACK_BADGE: Record<Environment["stack_status"], { label: string; variant: "default" | "secondary" | "destructive" | "outline" }> = {
  pending: { label: "Pending", variant: "secondary" },
  provisioning: { label: "Provisioning", variant: "outline" },
  ready: { label: "Ready", variant: "default" },
  failed: { label: "Failed", variant: "destructive" },
};

const EVENT_LABELS: Record<string, string> = {
  "deploy.started": "Deploy started",
  "build.started": "Build started",
  "build.completed": "Build completed",
  "build.failed": "Build failed",
  "ecs.rollout.started": "ECS rollout started",
  "ecs.stable": "Service stable",
  "healthcheck.failed": "Health check failed",
  "rollback.triggered": "Rollback triggered",
  "provision.started": "Provision started",
  "provision.ready": "Environment ready",
  "provision.failed": "Provision failed",
  "runtime.tasks_degraded": "Tasks degraded",
  "runtime.service_down": "Service down",
  "runtime.high_error_rate": "High error rate",
  "runtime.high_latency": "High latency",
  "runtime.service_recovered": "Service recovered",
  "runtime.log_anomaly": "Log anomaly",
  "diagnosis.started": "Diagnosis started",
  "diagnosis.completed": "Diagnosis completed",
};

function timeAgo(iso: string) {
  const s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

const SEVERITY_DOT: Record<string, string> = {
  info: "bg-zinc-400",
  warn: "bg-amber-500",
  error: "bg-red-500",
};

interface EnvHealth {
  running: number;
  desired: number;
}

interface Props {
  projectId: string;
  environments: Environment[];
  deployments: Deployment[];
  alerts: Alert[];
  onSelectEnvironment?: (envId: string) => void;
}

export function StatusSidebar({ projectId, environments, deployments, alerts, onSelectEnvironment }: Props) {
  const { getToken } = useAuth();
  const [health, setHealth] = useState<Record<string, EnvHealth>>({});
  const [events, setEvents] = useState<OperationalEvent[]>([]);

  const realEnvs = environments.filter((e) => !e.is_preview);

  // Live task counts every 30s for ready environments.
  useEffect(() => {
    let cancelled = false;
    async function poll() {
      const token = await getToken();
      if (!token || cancelled) return;
      for (const env of realEnvs.filter((e) => e.stack_status === "ready")) {
        try {
          const h = await checkHealth(token, projectId, env.id);
          if (!cancelled) {
            setHealth((prev) => ({ ...prev, [env.id]: { running: h.running, desired: h.desired } }));
          }
        } catch {
          // env not deployed yet — skip silently
        }
      }
    }
    void poll();
    const t = setInterval(poll, 30_000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectId, environments.length]);

  // Recent activity feed every 30s.
  useEffect(() => {
    let cancelled = false;
    async function load() {
      const token = await getToken();
      if (!token || cancelled) return;
      try {
        const evs = await getProjectEvents(token, projectId, 5);
        if (!cancelled) setEvents(evs);
      } catch {}
    }
    void load();
    const t = setInterval(load, 30_000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectId]);

  const lastDeployFor = (envId: string) =>
    deployments.find((d) => d.environment_id === envId);
  const openAlertsFor = (envId: string) =>
    alerts.filter((a) => a.environment_id === envId && a.status === "open").length;

  return (
    <aside className="w-64 shrink-0 border-r bg-white p-4 space-y-6 overflow-y-auto">
      <div>
        <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">
          Live Status
        </h2>
        {realEnvs.length === 0 && (
          <p className="text-xs text-muted-foreground">No environments yet.</p>
        )}
        <div className="space-y-3">
          {realEnvs.map((env) => {
            const badge = STACK_BADGE[env.stack_status];
            const h = health[env.id];
            const lastDep = lastDeployFor(env.id);
            const alertCount = openAlertsFor(env.id);
            return (
              <button
                key={env.id}
                onClick={() => onSelectEnvironment?.(env.id)}
                className="w-full text-left rounded-lg border p-3 hover:bg-zinc-50 transition-colors"
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="text-sm font-medium capitalize">{env.name}</span>
                  <div className="flex items-center gap-1.5">
                    {alertCount > 0 && (
                      <span className="inline-flex items-center justify-center h-4 min-w-4 px-1 rounded-full bg-red-500 text-white text-[10px] font-semibold">
                        {alertCount}
                      </span>
                    )}
                    <Badge variant={badge.variant} className="text-[10px] px-1.5 py-0">{badge.label}</Badge>
                  </div>
                </div>
                <div className="mt-1.5 text-xs text-muted-foreground space-y-0.5">
                  {h && (
                    <p className={cn(h.running < h.desired && "text-amber-600 font-medium")}>
                      {h.running}/{h.desired} tasks running
                    </p>
                  )}
                  {lastDep && <p>Deployed {timeAgo(lastDep.created_at)}</p>}
                </div>
              </button>
            );
          })}
        </div>
      </div>

      <div>
        <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3 flex items-center gap-1.5">
          <Activity className="h-3 w-3" />
          Recent Activity
        </h2>
        {events.length === 0 ? (
          <p className="text-xs text-muted-foreground">No activity yet.</p>
        ) : (
          <ul className="space-y-2">
            {events.map((ev) => (
              <li key={ev.id} className="flex items-start gap-2 text-xs">
                <span className={cn("mt-1 h-1.5 w-1.5 rounded-full shrink-0", SEVERITY_DOT[ev.severity] ?? "bg-zinc-400")} />
                <div className="min-w-0">
                  <p className="truncate text-foreground">{EVENT_LABELS[ev.event_type] ?? ev.event_type}</p>
                  <p className="text-muted-foreground">{timeAgo(ev.occurred_at)}</p>
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
    </aside>
  );
}
