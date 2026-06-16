"use client";

import { useState } from "react";
import useSWR from "swr";
import Link from "next/link";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { listOrgPostmortems } from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import { SEVERITY_BADGE, timeOpen } from "@/lib/incidents";
import type { Postmortem } from "@/types/api";
import { cn } from "@/lib/utils";
import { FileText, Loader2, Search, ShieldCheck, ArrowRight } from "lucide-react";

const SEVERITIES = ["", "error", "warn"];

export default function PostmortemLibraryPage() {
  const { getToken } = useAuth();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id;

  const [q, setQ] = useState("");
  const [severity, setSeverity] = useState("");

  const { data: postmortems, isLoading } = useSWR<Postmortem[]>(
    orgId ? ["org-postmortems", orgId, q, severity] : null,
    async () => {
      const token = await getToken();
      if (!token || !orgId) return [];
      return listOrgPostmortems(token, orgId, { q: q.trim() || undefined, severity: severity || undefined });
    }
  );

  const items = postmortems ?? [];

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-4xl mx-auto px-4 py-10">
        <div className="flex items-center gap-2 mb-1">
          <FileText className="h-5 w-5 text-indigo-600" />
          <h1 className="text-2xl font-bold tracking-tight">Postmortems</h1>
        </div>
        <p className="text-muted-foreground text-sm mb-2">
          Published incident postmortems for {activeOrg?.name ?? "your workspace"}.
        </p>
        {/* Compliance framing — the library doubles as an audit trail. */}
        <div className="flex items-start gap-2 rounded-md border border-indigo-100 bg-indigo-50/50 px-3 py-2 mb-6 text-xs text-indigo-900">
          <ShieldCheck className="h-4 w-4 shrink-0 mt-0.5 text-indigo-600" />
          <span>
            A complete, timestamped record of resolved incidents and their action items —
            useful evidence for SOC 2 (CC7.3 / CC7.4) and similar incident-response controls.
          </span>
        </div>

        {/* Filters */}
        <div className="flex items-center gap-2 mb-4">
          <div className="relative flex-1 max-w-sm">
            <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-muted-foreground" />
            <Input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Search postmortems…"
              className="pl-8 h-9 text-sm"
            />
          </div>
          <div className="flex items-center gap-1">
            {SEVERITIES.map((s) => (
              <button
                key={s || "all"}
                onClick={() => setSeverity(s)}
                className={cn(
                  "rounded-md border px-2.5 py-1 text-xs capitalize",
                  severity === s ? "bg-foreground text-background" : "hover:bg-zinc-50"
                )}
              >
                {s || "All"}
              </button>
            ))}
          </div>
        </div>

        {isLoading ? (
          <div className="flex items-center justify-center h-40">
            <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
          </div>
        ) : items.length === 0 ? (
          <Card className="border-dashed">
            <CardContent className="py-12 text-center">
              <FileText className="h-10 w-10 text-zinc-300 mx-auto mb-3" />
              <p className="font-medium text-sm">No published postmortems</p>
              <p className="text-muted-foreground text-xs mt-1">
                When an incident is resolved, OpsPilot drafts a postmortem. Publish it from the
                war room and it appears here.
              </p>
            </CardContent>
          </Card>
        ) : (
          <div className="rounded-lg border bg-white divide-y">
            {items.map((pm) => (
              <Link
                key={pm.id}
                href={`/postmortems/${pm.id}/edit`}
                className="flex items-center gap-3 px-4 py-3 hover:bg-zinc-50 group"
              >
                {pm.severity && (
                  <span className={cn("rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase shrink-0", SEVERITY_BADGE[pm.severity] ?? "bg-zinc-100 text-zinc-600")}>
                    {pm.severity}
                  </span>
                )}
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium truncate">{pm.title || pm.incident_title || "Postmortem"}</p>
                  <p className="text-xs text-muted-foreground truncate">
                    {pm.project_name}
                    {pm.published_at ? ` · published ${new Date(pm.published_at).toLocaleDateString()}` : ""}
                    {pm.incident_opened ? ` · resolved in ${timeOpen(pm.incident_opened, pm.incident_closed)}` : ""}
                  </p>
                </div>
                <ArrowRight className="h-4 w-4 text-muted-foreground opacity-0 group-hover:opacity-100 shrink-0" />
              </Link>
            ))}
          </div>
        )}
      </main>
    </div>
  );
}
