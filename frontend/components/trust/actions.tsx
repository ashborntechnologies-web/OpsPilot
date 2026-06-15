"use client";

import { useState } from "react";
import { cn } from "@/lib/utils";
import { ConfidenceBadge } from "@/components/ai/explainability";
import type { AIAction } from "@/types/api";
import { Bot, User, Rocket, RotateCcw, Scaling, Cpu, Terminal, Loader2 } from "lucide-react";
import type { LucideIcon } from "lucide-react";

export const ACTION_LABEL: Record<string, string> = {
  deploy: "Deploy",
  rollback: "Roll back",
  scale: "Scale",
  change_resources: "Change resources",
  terminal_command: "Terminal command",
};

export const ACTION_ICON: Record<string, LucideIcon> = {
  deploy: Rocket,
  rollback: RotateCcw,
  scale: Scaling,
  change_resources: Cpu,
  terminal_command: Terminal,
};

export const STATUS_COLOR: Record<string, string> = {
  pending_approval: "bg-amber-100 text-amber-700",
  approved: "bg-sky-100 text-sky-700",
  executed: "bg-green-100 text-green-700",
  rejected: "bg-zinc-200 text-zinc-600",
  failed: "bg-red-100 text-red-700",
};

// summarizeParams renders the action's key parameters compactly (e.g. "→ 3 replicas").
function summarizeParams(a: AIAction): string {
  const p = a.parameters ?? {};
  if (a.action_type === "scale" && p.replicas != null) return `to ${p.replicas} replicas`;
  if (a.action_type === "change_resources") return [p.cpu, p.memory].filter(Boolean).join(" / ");
  if (p.env_name) return `on ${p.env_name}`;
  return "";
}

// PendingApprovalCard shows a pending AI action with Approve/Reject. canAct hides the
// buttons for viewers (the backend enforces regardless).
export function PendingApprovalCard({
  action, canAct, onApprove, onReject,
}: {
  action: AIAction;
  canAct: boolean;
  onApprove: (id: string) => Promise<void>;
  onReject: (id: string) => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const Icon = ACTION_ICON[action.action_type] ?? Bot;
  const run = (fn: (id: string) => Promise<void>) => async () => {
    setBusy(true);
    try { await fn(action.id); } finally { setBusy(false); }
  };
  return (
    <div className="rounded-lg border border-amber-200 bg-amber-50/60 p-3">
      <div className="flex items-center gap-2">
        <Bot className="h-3.5 w-3.5 text-indigo-600" />
        <Icon className="h-3.5 w-3.5 text-zinc-500" />
        <span className="text-sm font-semibold">{ACTION_LABEL[action.action_type] ?? action.action_type}</span>
        <span className="text-xs text-muted-foreground">{summarizeParams(action)}</span>
        <ConfidenceBadge score={action.confidence_score} className="ml-auto" />
      </div>
      {action.rationale && <p className="mt-1 text-xs text-muted-foreground">{action.rationale}</p>}
      {action.environment_name && (
        <p className="mt-1 text-[11px] text-muted-foreground">Environment: {action.environment_name}</p>
      )}
      {canAct && (
        <div className="mt-2 flex gap-2">
          <button
            onClick={run(onApprove)} disabled={busy}
            className="inline-flex items-center gap-1 rounded-md bg-zinc-900 px-2.5 py-1 text-xs text-white hover:bg-zinc-800 disabled:opacity-50"
          >
            {busy ? <Loader2 className="h-3 w-3 animate-spin" /> : null} Approve
          </button>
          <button
            onClick={run(onReject)} disabled={busy}
            className="rounded-md border px-2.5 py-1 text-xs hover:bg-zinc-50 disabled:opacity-50"
          >
            Reject
          </button>
        </div>
      )}
    </div>
  );
}

// ActionRow renders one row in the action history (AI vs human icon, status color).
export function ActionRow({ action }: { action: AIAction }) {
  const Icon = ACTION_ICON[action.action_type] ?? Bot;
  const result = (action.result?.message as string) || (action.result?.error as string) || "";
  return (
    <div className="flex items-center gap-3 px-4 py-2.5">
      {action.proposed_by_type === "ai"
        ? <Bot className="h-4 w-4 text-indigo-600 shrink-0" />
        : <User className="h-4 w-4 text-zinc-500 shrink-0" />}
      <Icon className="h-4 w-4 text-zinc-400 shrink-0" />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">{ACTION_LABEL[action.action_type] ?? action.action_type}</span>
          <span className="text-xs text-muted-foreground">{summarizeParams(action)}</span>
        </div>
        <p className="text-xs text-muted-foreground truncate">
          {action.rationale}{result ? ` — ${result}` : ""}
        </p>
        <p className="text-[11px] text-muted-foreground">
          {new Date(action.proposed_at).toLocaleString()}
          {action.approved_by_name ? ` · by ${action.approved_by_name}` : ""}
        </p>
      </div>
      <span className={cn("rounded px-1.5 py-0.5 text-[10px] font-medium capitalize shrink-0", STATUS_COLOR[action.status])}>
        {action.status.replace("_", " ")}
      </span>
    </div>
  );
}
