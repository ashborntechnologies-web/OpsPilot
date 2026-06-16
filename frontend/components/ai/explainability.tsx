"use client";

import { useState } from "react";
import { cn } from "@/lib/utils";
import type { EvidenceItem } from "@/types/api";
import { ChevronDown, ChevronRight, FileSearch } from "lucide-react";

// ConfidenceBadge renders an AI confidence score (0.0–1.0) as a colored percentage pill:
// green >75%, amber 50–75%, red <50%.
export function ConfidenceBadge({ score, className }: { score: number | null | undefined; className?: string }) {
  if (score === null || score === undefined) return null;
  const pct = Math.round(score * 100);
  const color =
    pct > 75 ? "bg-green-100 text-green-700"
    : pct >= 50 ? "bg-amber-100 text-amber-700"
    : "bg-red-100 text-red-700";
  return (
    <span
      className={cn("inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-semibold", color, className)}
      title="AI confidence — how well the available data supports this conclusion"
    >
      {pct}% confidence
    </span>
  );
}

const EVIDENCE_LABEL: Record<string, string> = {
  log_pattern: "Log pattern",
  metric_spike: "Metric spike",
  deploy_correlation: "Deploy correlation",
  memory_match: "Memory match",
  similar_incident: "Similar incident",
};

const EVIDENCE_BORDER: Record<string, string> = {
  log_pattern: "border-l-indigo-400",
  metric_spike: "border-l-rose-400",
  deploy_correlation: "border-l-amber-400",
  memory_match: "border-l-emerald-400",
  similar_incident: "border-l-sky-400",
};

// EvidenceSection is a collapsible list of the signals the AI used. Collapsed by default;
// the toggle shows the signal count. Each item is a colored left-border card with a type
// badge, description, and (optionally) the raw supporting data.
export function EvidenceSection({ items, className }: { items: EvidenceItem[] | undefined | null; className?: string }) {
  const [open, setOpen] = useState(false);
  if (!items || items.length === 0) return null;
  return (
    <div className={className}>
      <button
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1.5 text-xs font-medium text-indigo-700 hover:underline"
      >
        {open ? <ChevronDown className="h-3.5 w-3.5" /> : <ChevronRight className="h-3.5 w-3.5" />}
        <FileSearch className="h-3.5 w-3.5" />
        {open ? "Hide evidence" : `Show evidence (${items.length} signal${items.length === 1 ? "" : "s"})`}
      </button>
      {open && (
        <div className="mt-2 space-y-2">
          {items.map((e, i) => (
            <div key={i} className={cn("rounded-md border-l-2 bg-white px-3 py-2 shadow-sm", EVIDENCE_BORDER[e.type] ?? "border-l-zinc-300")}>
              <div className="flex items-center gap-2">
                <span className="rounded bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium text-zinc-600">
                  {EVIDENCE_LABEL[e.type] ?? e.type}
                </span>
                {typeof e.weight === "number" && e.weight > 0 && (
                  <span className="text-[10px] text-muted-foreground">weight {Math.round(e.weight * 100)}%</span>
                )}
              </div>
              <p className="mt-1 text-sm text-zinc-700">{e.description}</p>
              {e.data && Object.keys(e.data).length > 0 && (
                <pre className="mt-1.5 rounded bg-zinc-950 text-zinc-200 text-[11px] p-2 overflow-x-auto whitespace-pre-wrap">
                  {JSON.stringify(e.data, null, 2)}
                </pre>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
