"use client";

import { useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import useSWR from "swr";
import { UserButton, useAuth } from "@clerk/nextjs";
import { Rocket, CheckCircle2 } from "lucide-react";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { getMe } from "@/lib/api";
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

  const { data: me } = useSWR("users-me", async () => {
    const token = await getToken();
    if (!token) return null;
    return getMe(token);
  }, { refreshInterval: 120_000 });

  const usageLabel = me
    ? me.plan === "free"
      ? `${me.ai_actions_this_month}/${me.ai_actions_limit} AI actions`
      : `${me.projects_count}/${me.projects_limit} projects`
    : null;

  return (
    <header className="border-b bg-white">
      <div className="max-w-6xl mx-auto px-4 h-14 flex items-center justify-between">
        <Link href="/projects" className="flex items-center gap-2 font-semibold text-sm">
          <Rocket className="h-4 w-4 text-indigo-600" />
          OpsPilot
        </Link>

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
