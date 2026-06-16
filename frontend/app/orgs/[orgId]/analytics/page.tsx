"use client";

import { useState } from "react";
import useSWR from "swr";
import { useParams } from "next/navigation";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { getOrgAnalytics, generateOrgReport } from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import { SLA_BADGE, uptimeColor, fmtMinutes } from "@/lib/analytics";
import { UptimeLineChart, IncidentBarChart } from "@/components/analytics/charts";
import type { OrgAnalytics } from "@/types/api";
import { cn } from "@/lib/utils";
import { toast } from "sonner";
import {
  Loader2, Activity, TrendingUp, TrendingDown, Minus, FileBarChart, Clock, Timer, Rocket, AlertTriangle,
} from "lucide-react";

const WINDOWS = [7, 30, 90];

export default function LeadershipAnalyticsPage() {
  const { orgId } = useParams<{ orgId: string }>();
  const { getToken } = useAuth();
  const { isAdmin } = useActiveOrg();
  const [days, setDays] = useState(30);
  const [generating, setGenerating] = useState(false);

  const { data, isLoading } = useSWR<OrgAnalytics>(
    ["org-analytics", orgId, days],
    async () => {
      const token = await getToken();
      if (!token) throw new Error("no token");
      return getOrgAnalytics(token, orgId, days);
    },
    { refreshInterval: 120_000 }
  );

  async function handleGenerateReport() {
    const token = await getToken();
    if (!token) return;
    setGenerating(true);
    try {
      await generateOrgReport(token, orgId);
      toast.success("Monthly report generated — emailed to admins");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setGenerating(false);
    }
  }

  const m = data?.metrics;

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-6xl mx-auto px-4 py-10">
        <div className="flex items-start justify-between gap-4 mb-6">
          <div>
            <div className="flex items-center gap-2 mb-1">
              <Activity className="h-5 w-5 text-indigo-600" />
              <h1 className="text-2xl font-bold tracking-tight">Engineering analytics</h1>
            </div>
            <p className="text-muted-foreground text-sm">
              SLA &amp; uptime, MTTD/MTTR, and reliability trends across all projects.
            </p>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            <div className="flex items-center rounded-md border bg-white p-0.5">
              {WINDOWS.map((w) => (
                <button
                  key={w}
                  onClick={() => setDays(w)}
                  className={cn("px-2.5 py-1 text-xs rounded", days === w ? "bg-foreground text-background" : "text-muted-foreground hover:text-foreground")}
                >
                  {w}d
                </button>
              ))}
            </div>
            {isAdmin && (
              <Button size="sm" variant="outline" onClick={handleGenerateReport} disabled={generating}>
                {generating ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : <FileBarChart className="h-3.5 w-3.5 mr-1" />}
                Generate report
              </Button>
            )}
          </div>
        </div>

        {isLoading || !m ? (
          <div className="flex items-center justify-center h-64">
            <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
          </div>
        ) : (
          <div className="space-y-6">
            {/* Row 1 — stat cards */}
            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
              <StatCard
                icon={<Activity className="h-4 w-4" />}
                label="Average uptime"
                value={`${m.uptime_pct.toFixed(2)}%`}
                valueClass={uptimeColor(m.uptime_pct, m.sla_target)}
                sub={
                  <span className={cn("rounded px-1.5 py-0.5 text-[10px] font-medium", SLA_BADGE[m.sla_status].cls)}>
                    {SLA_BADGE[m.sla_status].label} · SLA {m.sla_target}%
                  </span>
                }
              />
              <StatCard
                icon={<Timer className="h-4 w-4" />}
                label="MTTR"
                value={fmtMinutes(m.mttr)}
                sub={<Trend current={m.mttr} prev={m.prev_mttr} lowerBetter unit="min" />}
              />
              <StatCard
                icon={<Clock className="h-4 w-4" />}
                label="MTTD"
                value={fmtMinutes(m.mttd)}
                sub={<Trend current={m.mttd} prev={m.prev_mttd} lowerBetter unit="min" />}
              />
              <StatCard
                icon={<Rocket className="h-4 w-4" />}
                label="Deploy success"
                value={`${m.deploy_success_rate.toFixed(0)}%`}
                sub={<Trend current={m.deploy_success_rate} prev={m.prev_deploy_success_rate} unit="pp" />}
              />
            </div>

            {/* Row 2 — charts */}
            <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
              <Card>
                <CardContent className="pt-5">
                  <p className="text-sm font-medium mb-3">Uptime — last {days} days</p>
                  <UptimeLineChart points={m.uptime_trend} slaTarget={m.sla_target} />
                </CardContent>
              </Card>
              <Card>
                <CardContent className="pt-5">
                  <p className="text-sm font-medium mb-3">Incidents per week — last 12 weeks</p>
                  <IncidentBarChart points={m.incident_trend} />
                </CardContent>
              </Card>
            </div>

            {/* Row 3 — project breakdown */}
            <Card>
              <CardContent className="pt-5">
                <p className="text-sm font-medium mb-3">Project breakdown</p>
                {(data?.breakdown ?? []).length === 0 ? (
                  <p className="text-xs text-muted-foreground">No environments yet.</p>
                ) : (
                  <div className="overflow-x-auto">
                    <table className="w-full text-sm">
                      <thead>
                        <tr className="text-left text-[11px] uppercase tracking-wide text-muted-foreground border-b">
                          <th className="py-2 pr-3 font-medium">Project</th>
                          <th className="py-2 pr-3 font-medium">Environment</th>
                          <th className="py-2 pr-3 font-medium text-right">Uptime</th>
                          <th className="py-2 pr-3 font-medium text-right">MTTR</th>
                          <th className="py-2 pr-3 font-medium text-right">Incidents</th>
                          <th className="py-2 font-medium">SLA</th>
                        </tr>
                      </thead>
                      <tbody>
                        {(data?.breakdown ?? []).map((r) => (
                          <tr key={r.environment_id} className="border-b last:border-0">
                            <td className="py-2 pr-3 font-medium">{r.project_name}</td>
                            <td className="py-2 pr-3 capitalize text-muted-foreground">{r.environment_name}</td>
                            <td className={cn("py-2 pr-3 text-right tabular-nums", uptimeColor(r.uptime_pct, r.sla_target))}>
                              {r.uptime_pct.toFixed(2)}%
                            </td>
                            <td className="py-2 pr-3 text-right tabular-nums text-muted-foreground">{fmtMinutes(r.mttr)}</td>
                            <td className="py-2 pr-3 text-right tabular-nums">{r.incident_count}</td>
                            <td className="py-2">
                              <span className={cn("rounded px-1.5 py-0.5 text-[10px] font-medium", SLA_BADGE[r.sla_status].cls)}>
                                {SLA_BADGE[r.sla_status].label}
                              </span>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </CardContent>
            </Card>

            {/* Row 4 — top failure patterns */}
            <Card>
              <CardContent className="pt-5">
                <p className="text-sm font-medium mb-3 flex items-center gap-1.5">
                  <AlertTriangle className="h-4 w-4 text-amber-500" /> Top recurring failures
                </p>
                {m.top_failure_patterns.length === 0 ? (
                  <p className="text-xs text-muted-foreground">No recurring failure patterns recorded.</p>
                ) : (
                  <ol className="space-y-2">
                    {m.top_failure_patterns.map((fp, i) => (
                      <li key={i} className="flex items-start gap-3 text-sm">
                        <span className="text-xs font-mono text-muted-foreground mt-0.5 w-5 shrink-0">#{i + 1}</span>
                        <span className="flex-1">{fp.pattern}</span>
                        <span className="text-xs text-muted-foreground shrink-0" title="Times observed">×{fp.count}</span>
                      </li>
                    ))}
                  </ol>
                )}
              </CardContent>
            </Card>
          </div>
        )}
      </main>
    </div>
  );
}

function StatCard({ icon, label, value, valueClass, sub }: {
  icon: React.ReactNode; label: string; value: string; valueClass?: string; sub?: React.ReactNode;
}) {
  return (
    <Card>
      <CardContent className="pt-5">
        <div className="flex items-center gap-1.5 text-muted-foreground mb-1.5">
          {icon}
          <span className="text-xs font-medium uppercase tracking-wide">{label}</span>
        </div>
        <p className={cn("text-2xl font-bold tabular-nums", valueClass)}>{value}</p>
        <div className="mt-1.5">{sub}</div>
      </CardContent>
    </Card>
  );
}

// Trend shows the delta vs the previous period with a colored arrow. lowerBetter inverts the
// color (e.g. MTTR going down is good).
function Trend({ current, prev, lowerBetter, unit }: { current: number; prev: number; lowerBetter?: boolean; unit: string }) {
  if (prev <= 0 && current <= 0) {
    return <span className="text-[11px] text-muted-foreground">no prior data</span>;
  }
  const delta = current - prev;
  const flat = Math.abs(delta) < 0.05;
  const good = lowerBetter ? delta < 0 : delta > 0;
  const Icon = flat ? Minus : delta > 0 ? TrendingUp : TrendingDown;
  const color = flat ? "text-muted-foreground" : good ? "text-green-600" : "text-red-600";
  const mag = unit === "min" ? fmtMinutes(Math.abs(delta)) : `${Math.abs(delta).toFixed(unit === "pp" ? 0 : 1)}${unit === "pp" ? "pp" : ""}`;
  return (
    <span className={cn("inline-flex items-center gap-1 text-[11px]", color)}>
      <Icon className="h-3 w-3" />
      {flat ? "flat" : mag} vs prev
    </span>
  );
}
