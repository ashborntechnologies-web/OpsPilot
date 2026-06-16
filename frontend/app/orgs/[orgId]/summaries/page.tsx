"use client";

import { useState } from "react";
import useSWR from "swr";
import { useParams } from "next/navigation";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Card, CardContent } from "@/components/ui/card";
import { listSummaries } from "@/lib/api";
import { Markdown } from "@/lib/markdown";
import type { DailySummaryRecord } from "@/types/api";
import { Loader2, Sun, ChevronDown, ChevronRight } from "lucide-react";

function fmtDate(d: string) {
  // d is YYYY-MM-DD; render without timezone shifting.
  const [y, m, day] = d.split("-").map(Number);
  return new Date(y, (m ?? 1) - 1, day ?? 1).toLocaleDateString(undefined, {
    weekday: "long", month: "short", day: "numeric", year: "numeric",
  });
}

export default function SummaryHistoryPage() {
  const { orgId } = useParams<{ orgId: string }>();
  const { getToken } = useAuth();
  const [expanded, setExpanded] = useState<string | null>(null);

  const { data: summaries, isLoading } = useSWR<DailySummaryRecord[]>(
    ["summaries", orgId],
    async () => {
      const token = await getToken();
      if (!token) return [];
      return listSummaries(token, orgId, 60);
    }
  );

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-3xl mx-auto px-4 py-10">
        <div className="flex items-center gap-2 mb-1">
          <Sun className="h-5 w-5 text-amber-500" />
          <h1 className="text-2xl font-bold tracking-tight">Daily summaries</h1>
        </div>
        <p className="text-muted-foreground text-sm mb-6">AI-generated morning briefings for this workspace.</p>

        {isLoading ? (
          <div className="flex items-center justify-center h-40"><Loader2 className="h-5 w-5 animate-spin text-muted-foreground" /></div>
        ) : (summaries ?? []).length === 0 ? (
          <Card className="border-dashed">
            <CardContent className="py-12 text-center">
              <Sun className="h-10 w-10 text-zinc-300 mx-auto mb-3" />
              <p className="font-medium text-sm">No summaries yet</p>
              <p className="text-muted-foreground text-xs mt-1">The first briefing appears after your configured delivery time.</p>
            </CardContent>
          </Card>
        ) : (
          <div className="space-y-2">
            {(summaries ?? []).map((s) => {
              const open = expanded === s.id;
              return (
                <Card key={s.id}>
                  <button
                    onClick={() => setExpanded(open ? null : s.id)}
                    className="w-full flex items-center justify-between gap-3 px-4 py-3 text-left"
                  >
                    <span className="flex items-center gap-2 text-sm font-medium">
                      {open ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
                      {fmtDate(s.summary_date)}
                    </span>
                    <span className="flex gap-1.5 shrink-0">
                      {s.delivered_slack && <span className="rounded bg-zinc-100 px-1.5 py-0.5 text-[10px] text-zinc-600">Slack</span>}
                      {s.delivered_email && <span className="rounded bg-zinc-100 px-1.5 py-0.5 text-[10px] text-zinc-600">Email</span>}
                    </span>
                  </button>
                  {open && (
                    <CardContent className="pt-0 border-t">
                      <Markdown content={s.content_markdown} className="pt-3" />
                    </CardContent>
                  )}
                </Card>
              );
            })}
          </div>
        )}
      </main>
    </div>
  );
}
