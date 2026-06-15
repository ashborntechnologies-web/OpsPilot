"use client";

// AlertsPanel is the right column of the project command center: open alerts
// with snooze/resolve actions, plus the latest AI diagnosis insight.

import { useState } from "react";
import { Button } from "@/components/ui/button";
import type { Alert } from "@/types/api";
import { cn } from "@/lib/utils";
import { AlertTriangle, CheckCircle2, Clock, Sparkles } from "lucide-react";

function timeAgo(iso: string) {
  const s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

interface Props {
  alerts: Alert[];
  latestInsight: string | null;
  onSnooze: (alertId: string) => void;
  onResolve: (alertId: string) => void;
}

export function AlertsPanel({ alerts, latestInsight, onSnooze, onResolve }: Props) {
  const [insightExpanded, setInsightExpanded] = useState(false);
  const open = alerts.filter((a) => a.status === "open");

  return (
    <aside className="w-80 shrink-0 border-l bg-white p-4 space-y-6 overflow-y-auto">
      <div>
        <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">
          Open Alerts
        </h2>
        {open.length === 0 ? (
          <div className="flex items-center gap-2 rounded-lg border border-green-200 bg-green-50 px-3 py-2.5">
            <CheckCircle2 className="h-4 w-4 text-green-600 shrink-0" />
            <span className="text-sm text-green-800">All systems healthy</span>
          </div>
        ) : (
          <div className="space-y-3">
            {open.map((alert) => (
              <div
                key={alert.id}
                className={cn(
                  "rounded-lg border p-3",
                  alert.severity === "error"
                    ? "border-red-200 bg-red-50/60"
                    : "border-amber-200 bg-amber-50/60"
                )}
              >
                <div className="flex items-start gap-2">
                  <AlertTriangle
                    className={cn(
                      "h-4 w-4 mt-0.5 shrink-0",
                      alert.severity === "error" ? "text-red-600" : "text-amber-600"
                    )}
                  />
                  <div className="min-w-0 flex-1">
                    <p className="text-sm font-semibold leading-tight">{alert.title}</p>
                    {alert.summary && (
                      <p className="text-xs text-muted-foreground mt-1">{alert.summary}</p>
                    )}
                    {alert.evidence_text && (
                      <p className="text-[11px] text-muted-foreground/80 mt-1 line-clamp-2 italic">{alert.evidence_text}</p>
                    )}
                    <p className="text-[11px] text-muted-foreground mt-1 flex items-center gap-1">
                      <Clock className="h-3 w-3" />
                      {timeAgo(alert.triggered_at)}
                    </p>
                  </div>
                </div>
                <div className="mt-2 flex gap-2">
                  <Button size="sm" variant="outline" className="h-6 px-2 text-xs" onClick={() => onSnooze(alert.id)}>
                    Snooze 1h
                  </Button>
                  <Button size="sm" variant="outline" className="h-6 px-2 text-xs" onClick={() => onResolve(alert.id)}>
                    Resolve
                  </Button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {latestInsight && (
        <div>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3 flex items-center gap-1.5">
            <Sparkles className="h-3 w-3 text-indigo-500" />
            AI Insights
          </h2>
          <div className="rounded-lg border border-indigo-100 bg-indigo-50/40 p-3">
            <p
              className={cn(
                "text-xs leading-relaxed whitespace-pre-wrap",
                !insightExpanded && "line-clamp-3"
              )}
            >
              {latestInsight}
            </p>
            <button
              onClick={() => setInsightExpanded((v) => !v)}
              className="mt-1.5 text-xs text-indigo-600 hover:underline"
            >
              {insightExpanded ? "Collapse" : "Read full analysis"}
            </button>
          </div>
        </div>
      )}
    </aside>
  );
}
