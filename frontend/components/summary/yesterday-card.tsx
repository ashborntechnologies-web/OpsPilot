"use client";

import useSWR from "swr";
import Link from "next/link";
import { useAuth } from "@clerk/nextjs";
import { Card, CardContent } from "@/components/ui/card";
import { getLatestSummary } from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import type { DailySummaryRecord } from "@/types/api";
import { Sun } from "lucide-react";

// firstParagraph extracts the prose line from the summary markdown (skips headings/lists).
function firstParagraph(md: string): string {
  for (const line of md.split("\n")) {
    const t = line.trim();
    if (!t || t.startsWith("#") || t.startsWith("-") || t.startsWith("*")) continue;
    return t;
  }
  return "";
}

// recentDates returns today's and yesterday's YYYY-MM-DD (local).
function isRecent(date: string): boolean {
  const today = new Date();
  const y = new Date(today);
  y.setDate(today.getDate() - 1);
  const f = (d: Date) => d.toISOString().slice(0, 10);
  return date === f(today) || date === f(y);
}

// YesterdaySummaryCard shows the most recent daily summary (when it's from today or
// yesterday) at the top of the projects dashboard.
export function YesterdaySummaryCard() {
  const { getToken } = useAuth();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id;

  const { data } = useSWR<{ summary: DailySummaryRecord | null }>(
    orgId ? ["latest-summary", orgId] : null,
    async () => {
      const token = await getToken();
      if (!token || !orgId) return { summary: null };
      return getLatestSummary(token, orgId);
    }
  );

  const summary = data?.summary;
  if (!summary || !isRecent(summary.summary_date)) return null;

  const paragraph = firstParagraph(summary.content_markdown);
  const recs = summary.content_json?.recommendations ?? [];

  return (
    <Card className="mb-6 border-amber-200 bg-amber-50/40">
      <CardContent className="py-4">
        <div className="flex items-center justify-between gap-3 mb-2">
          <span className="flex items-center gap-2 text-sm font-medium">
            <Sun className="h-4 w-4 text-amber-500" />
            Daily summary — {summary.summary_date}
          </span>
          {orgId && (
            <Link href={`/orgs/${orgId}/summaries`} className="text-xs text-indigo-600 hover:underline shrink-0">
              View full history →
            </Link>
          )}
        </div>
        {paragraph && <p className="text-sm text-zinc-700 leading-relaxed">{paragraph}</p>}
        {recs.length > 0 && (
          <ul className="mt-2 space-y-0.5 text-sm text-zinc-700 list-disc ml-5">
            {recs.slice(0, 3).map((r, i) => <li key={i}>{r}</li>)}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}
