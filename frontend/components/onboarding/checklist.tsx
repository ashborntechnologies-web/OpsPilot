"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@clerk/nextjs";
import { Card, CardContent } from "@/components/ui/card";
import { listGithubRepos, listAWSAccounts } from "@/lib/api";
import { CheckCircle2, Circle, ArrowRight, GitBranch, Cloud, Rocket } from "lucide-react";
import { cn } from "@/lib/utils";

// OnboardingChecklist shows a live 3-step getting-started checklist (connect GitHub →
// connect AWS → create a project) so new users always have a clear next action and don't
// dead-end in the project wizard. It self-hides once all three steps are complete.
export function OnboardingChecklist({ hasProjects }: { hasProjects: boolean }) {
  const { getToken } = useAuth();
  const [githubConnected, setGithubConnected] = useState<boolean | null>(null);
  const [awsConnected, setAwsConnected] = useState<boolean | null>(null);

  useEffect(() => {
    (async () => {
      const token = await getToken();
      if (!token) return;
      // GitHub: listing repos succeeds only when a token is connected.
      try {
        await listGithubRepos(token);
        setGithubConnected(true);
      } catch {
        setGithubConnected(false);
      }
      try {
        const accts = await listAWSAccounts(token);
        setAwsConnected((accts ?? []).length > 0);
      } catch {
        setAwsConnected(false);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Wait for both probes before deciding whether to render (avoids a flash).
  if (githubConnected === null || awsConnected === null) return null;

  const steps = [
    { done: githubConnected, icon: GitBranch, title: "Connect GitHub", desc: "Authorize OpsPilot to read the repo you want to deploy.", href: "/projects/new", cta: "Connect" },
    { done: awsConnected, icon: Cloud, title: "Connect AWS", desc: "Run the bootstrap CloudFormation and paste the scoped role ARN.", href: "/aws-accounts", cta: "Connect" },
    { done: hasProjects, icon: Rocket, title: "Create your first project", desc: "Pick a repo and environment — OpsPilot provisions and deploys it.", href: "/projects/new", cta: "Create" },
  ];

  const completed = steps.filter((s) => s.done).length;
  if (completed === steps.length) return null; // fully onboarded — hide

  return (
    <Card className="mb-8 border-indigo-100 bg-indigo-50/30">
      <CardContent className="py-5">
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-sm font-semibold">Get started with OpsPilot</h2>
          <span className="text-xs text-muted-foreground">{completed} of {steps.length} complete</span>
        </div>
        <div className="space-y-2">
          {steps.map((s) => {
            const Icon = s.icon;
            // The first incomplete step is the "next" action — highlight it.
            const isNext = !s.done && steps.findIndex((x) => !x.done) === steps.indexOf(s);
            return (
              <div
                key={s.title}
                className={cn(
                  "flex items-center gap-3 rounded-md border px-3 py-2.5 bg-white",
                  s.done && "opacity-60",
                  isNext && "border-indigo-300 ring-1 ring-indigo-200"
                )}
              >
                {s.done
                  ? <CheckCircle2 className="h-5 w-5 text-green-600 shrink-0" />
                  : <Circle className="h-5 w-5 text-zinc-300 shrink-0" />}
                <Icon className="h-4 w-4 text-muted-foreground shrink-0" />
                <div className="min-w-0 flex-1">
                  <p className={cn("text-sm font-medium", s.done && "line-through")}>{s.title}</p>
                  {!s.done && <p className="text-xs text-muted-foreground">{s.desc}</p>}
                </div>
                {!s.done && (
                  <Link
                    href={s.href}
                    className={cn(
                      "inline-flex items-center gap-1 rounded-md px-2.5 py-1 text-xs font-medium shrink-0",
                      isNext ? "bg-indigo-600 text-white hover:bg-indigo-700" : "border hover:bg-zinc-50"
                    )}
                  >
                    {s.cta} <ArrowRight className="h-3 w-3" />
                  </Link>
                )}
              </div>
            );
          })}
        </div>
      </CardContent>
    </Card>
  );
}
