"use client";

import { useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import useSWR from "swr";
import { UserButton, useAuth } from "@clerk/nextjs";
import { Rocket, CheckCircle2, ChevronDown, Building2, Check, Siren, ShieldCheck } from "lucide-react";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { getMe, listOrgIncidents, listOrgActions } from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import type { Incident, AIAction } from "@/types/api";
import { cn } from "@/lib/utils";

const TIERS = [
  { name: "Free", price: "$0", features: ["1 project", "1 environment", "10 AI actions/month"] },
  { name: "Pro", price: "$49/mo", features: ["5 projects", "Full monitoring", "Unlimited AI", "Email alerts"] },
  { name: "Team", price: "$149/mo", features: ["Unlimited projects", "Team members (soon)", "Slack integration (soon)"] },
];

export function Navbar() {
  const path = usePathname();
  const { getToken } = useAuth();
  const [upgradeOpen, setUpgradeOpen] = useState(false);
  const [orgMenuOpen, setOrgMenuOpen] = useState(false);

  const { orgs, activeOrg, switchOrg } = useActiveOrg();

  const { data: me } = useSWR("users-me", async () => {
    const token = await getToken();
    if (!token) return null;
    return getMe(token);
  }, { refreshInterval: 120_000 });

  // Open-incident count for the active workspace (drives the red navbar badge).
  const { data: openIncidents } = useSWR<Incident[]>(
    activeOrg ? ["navbar-incidents", activeOrg.id] : null,
    async () => {
      const token = await getToken();
      if (!token || !activeOrg) return [];
      return listOrgIncidents(token, activeOrg.id);
    },
    { refreshInterval: 30_000 }
  );
  const openCount = (openIncidents ?? []).filter((i) => i.status !== "resolved").length;

  // Pending AI-action approvals count for the active workspace.
  const { data: pendingActions } = useSWR<AIAction[]>(
    activeOrg ? ["navbar-actions", activeOrg.id] : null,
    async () => {
      const token = await getToken();
      if (!token || !activeOrg) return [];
      return listOrgActions(token, activeOrg.id, "pending");
    },
    { refreshInterval: 30_000 }
  );
  const pendingCount = (pendingActions ?? []).length;

  const usageLabel = me
    ? me.plan === "free"
      ? `${me.ai_actions_this_month}/${me.ai_actions_limit} AI actions`
      : `${me.projects_count}/${me.projects_limit} projects`
    : null;

  return (
    <header className="border-b bg-white">
      <div className="max-w-6xl mx-auto px-4 h-14 flex items-center justify-between">
        <div className="flex items-center gap-3">
          <Link href="/projects" className="flex items-center gap-2 font-semibold text-sm">
            <Rocket className="h-4 w-4 text-indigo-600" />
            OpsPilot
          </Link>

          {/* Workspace switcher — shows the active org; opens a list when the user
              belongs to more than one. Always links to org settings. */}
          {activeOrg && (
            <div className="relative">
              <button
                onClick={() => orgs.length > 1 ? setOrgMenuOpen((v) => !v) : undefined}
                className={cn(
                  "flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs text-foreground",
                  orgs.length > 1 ? "hover:bg-zinc-50" : "cursor-default"
                )}
                title={orgs.length > 1 ? "Switch workspace" : activeOrg.name}
              >
                <Building2 className="h-3.5 w-3.5 text-muted-foreground" />
                <span className="max-w-[12rem] truncate">{activeOrg.name}</span>
                <span className="rounded bg-zinc-100 px-1 text-[10px] capitalize text-muted-foreground">{activeOrg.role}</span>
                {orgs.length > 1 && <ChevronDown className="h-3 w-3 text-muted-foreground" />}
              </button>
              {orgMenuOpen && orgs.length > 1 && (
                <>
                  <div className="fixed inset-0 z-10" onClick={() => setOrgMenuOpen(false)} />
                  <div className="absolute left-0 top-full z-20 mt-1 w-60 rounded-md border bg-white py-1 shadow-md">
                    {orgs.map((o) => (
                      <button
                        key={o.id}
                        onClick={() => { setOrgMenuOpen(false); if (o.id !== activeOrg.id) switchOrg(o.id); }}
                        className="flex w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-xs hover:bg-zinc-50"
                      >
                        <span className="truncate">{o.name}</span>
                        {o.id === activeOrg.id
                          ? <Check className="h-3.5 w-3.5 text-indigo-600 shrink-0" />
                          : <span className="text-[10px] capitalize text-muted-foreground">{o.role}</span>}
                      </button>
                    ))}
                    <div className="my-1 border-t" />
                    <Link
                      href="/settings/organization"
                      onClick={() => setOrgMenuOpen(false)}
                      className="block px-3 py-1.5 text-xs text-indigo-600 hover:bg-zinc-50"
                    >
                      Manage workspace →
                    </Link>
                  </div>
                </>
              )}
            </div>
          )}
        </div>

        <nav className="flex items-center gap-6 text-sm text-muted-foreground">
          <Link
            href="/projects"
            className={cn(path.startsWith("/projects") ? "text-foreground font-medium" : "hover:text-foreground")}
          >
            Projects
          </Link>
          <Link
            href="/aws-accounts"
            className={cn(path.startsWith("/aws-accounts") ? "text-foreground font-medium" : "hover:text-foreground")}
          >
            AWS Accounts
          </Link>
          <Link
            href="/incidents"
            className={cn("relative inline-flex items-center gap-1", path.startsWith("/incidents") ? "text-foreground font-medium" : "hover:text-foreground")}
          >
            <Siren className={cn("h-3.5 w-3.5", openCount > 0 && "text-red-600")} />
            Incidents
            {openCount > 0 && (
              <span className="ml-0.5 inline-flex items-center justify-center h-4 min-w-4 px-1 rounded-full bg-red-600 text-white text-[10px] font-semibold">
                {openCount}
              </span>
            )}
          </Link>
          {pendingCount > 0 && (
            <Link
              href="/settings/organization"
              className="relative inline-flex items-center gap-1 text-amber-700"
              title={`${pendingCount} AI action(s) awaiting approval`}
            >
              <ShieldCheck className="h-3.5 w-3.5" />
              Approvals
              <span className="ml-0.5 inline-flex items-center justify-center h-4 min-w-4 px-1 rounded-full bg-amber-500 text-white text-[10px] font-semibold">
                {pendingCount}
              </span>
            </Link>
          )}
          <Link
            href="/settings/organization"
            className={cn("hidden sm:inline", path.startsWith("/settings") ? "text-foreground font-medium" : "hover:text-foreground")}
          >
            Workspace
          </Link>
          <Link
            href="/terms"
            className={cn("hidden sm:inline", path.startsWith("/terms") ? "text-foreground font-medium" : "hover:text-foreground")}
          >
            Terms
          </Link>
          <Link
            href="/privacy"
            className={cn("hidden sm:inline", path.startsWith("/privacy") ? "text-foreground font-medium" : "hover:text-foreground")}
          >
            Privacy
          </Link>
        </nav>

        <div className="flex items-center gap-3">
          {usageLabel && (
            <button
              onClick={() => setUpgradeOpen(true)}
              className="hidden sm:inline-flex items-center rounded-full border px-2.5 py-1 text-xs text-muted-foreground hover:bg-zinc-50 hover:text-foreground transition-colors"
              title="Plan usage — click to see upgrade options"
            >
              {usageLabel}
            </button>
          )}
          <UserButton />
        </div>
      </div>

      {/* Upgrade modal — pricing overview, contact-based upgrade (no payments yet) */}
      <Dialog open={upgradeOpen} onOpenChange={setUpgradeOpen}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>Plans</DialogTitle>
            <DialogDescription>
              {me && (
                <>You&apos;re on the <span className="font-medium capitalize">{me.plan}</span> plan
                {" — "}{me.ai_actions_this_month}/{me.ai_actions_limit === 999999 ? "∞" : me.ai_actions_limit} AI actions used this month.</>
              )}
            </DialogDescription>
          </DialogHeader>
          <div className="grid sm:grid-cols-3 gap-3 pt-2">
            {TIERS.map((tier) => (
              <div
                key={tier.name}
                className={cn(
                  "rounded-lg border p-4",
                  me?.plan === tier.name.toLowerCase() && "border-indigo-500 bg-indigo-50/40"
                )}
              >
                <p className="font-semibold text-sm">{tier.name}</p>
                <p className="text-lg font-bold mt-0.5">{tier.price}</p>
                <ul className="mt-2 space-y-1 text-xs text-muted-foreground">
                  {tier.features.map((f) => (
                    <li key={f} className="flex items-start gap-1.5">
                      <CheckCircle2 className="h-3 w-3 text-indigo-600 mt-0.5 shrink-0" />
                      {f}
                    </li>
                  ))}
                </ul>
              </div>
            ))}
          </div>
          <Button className="w-full" nativeButton={false} render={
            <a href="mailto:hello@opspilot.dev?subject=OpsPilot%20upgrade" />
          }>
            Contact us to upgrade
          </Button>
        </DialogContent>
      </Dialog>
    </header>
  );
}
