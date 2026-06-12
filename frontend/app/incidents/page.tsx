"use client";

import useSWR from "swr";
import Link from "next/link";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Card, CardContent } from "@/components/ui/card";
import { listOrgIncidents } from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import type { Incident } from "@/types/api";
import { cn } from "@/lib/utils";
import { SEVERITY_BADGE, STATUS_BADGE, timeOpen } from "@/lib/incidents";
import { Loader2, Siren, CheckCircle2, Clock } from "lucide-react";

export default function IncidentsPage() {
  const { getToken } = useAuth();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id;

  const { data: incidents, isLoading } = useSWR<Incident[]>(
    orgId ? ["org-incidents", orgId] : null,
    async () => {
      const token = await getToken();
      if (!token || !orgId) return [];
      return listOrgIncidents(token, orgId);
    },
    { refreshInterval: 30_000 }
  );

  const open = (incidents ?? []).filter((i) => i.status !== "resolved");
  const resolved = (incidents ?? []).filter((i) => i.status === "resolved");

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-4xl mx-auto px-4 py-10">
        <div className="flex items-center gap-2 mb-1">
          <Siren className="h-5 w-5 text-red-600" />
          <h1 className="text-2xl font-bold tracking-tight">Incidents</h1>
        </div>
        <p className="text-muted-foreground text-sm mb-6">
          Shared war rooms where the team investigates incidents alongside the AI.
        </p>

        {isLoading ? (
          <div className="flex items-center justify-center h-40">
            <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
          </div>
        ) : (incidents ?? []).length === 0 ? (
          <Card className="border-dashed">
            <CardContent className="py-12 text-center">
              <CheckCircle2 className="h-10 w-10 text-green-400 mx-auto mb-3" />
              <p className="font-medium text-sm">No incidents</p>
              <p className="text-muted-foreground text-xs mt-1">
                When a deploy fails or the monitor detects a runtime anomaly, an incident opens here.
              </p>
            </CardContent>
          </Card>
        ) : (
          <div className="space-y-6">
            {open.length > 0 && (
              <Section title={`Open (${open.length})`} incidents={open} />
            )}
            {resolved.length > 0 && (
              <Section title="Resolved" incidents={resolved} muted />
            )}
          </div>
        )}
      </main>
    </div>
  );
}

function Section({ title, incidents, muted }: { title: string; incidents: Incident[]; muted?: boolean }) {
  return (
    <div>
      <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-2">{title}</h2>
      <div className="rounded-lg border bg-white divide-y">
        {incidents.map((i) => (
          <Link
            key={i.id}
            href={`/incidents/${i.id}`}
            className={cn("flex items-center gap-3 px-4 py-3 hover:bg-zinc-50", muted && "opacity-70")}
          >
            <span className={cn("rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase shrink-0", SEVERITY_BADGE[i.severity])}>
              {i.severity}
            </span>
            <div className="min-w-0 flex-1">
              <p className="text-sm font-medium truncate">{i.title || "Untitled incident"}</p>
              <p className="text-xs text-muted-foreground truncate">
                {i.project_name}{i.environment_name ? ` · ${i.environment_name}` : ""}
                {i.acknowledged_by_name ? ` · ack ${i.acknowledged_by_name}` : ""}
              </p>
            </div>
            <span className={cn("rounded px-1.5 py-0.5 text-[10px] font-medium capitalize shrink-0", STATUS_BADGE[i.status])}>
              {i.status}
            </span>
            <span className="flex items-center gap-1 text-xs text-muted-foreground shrink-0 w-20 justify-end">
              {i.status === "resolved" ? <CheckCircle2 className="h-3 w-3" /> : <Clock className="h-3 w-3" />}
              {timeOpen(i.created_at, i.resolved_at)}
            </span>
          </Link>
        ))}
      </div>
    </div>
  );
}
