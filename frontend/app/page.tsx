"use client";

// Landing page for unauthenticated visitors. Signed-in users are redirected
// straight to /projects.

import { useEffect } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useAuth } from "@clerk/nextjs";
import { Button } from "@/components/ui/button";
import {
  Rocket, Activity, Stethoscope, MessageSquare, GitBranch, Cloud, Eye,
  AlertTriangle, CheckCircle2, ArrowRight,
} from "lucide-react";

const FEATURES = [
  {
    icon: Activity,
    title: "Proactive monitoring",
    body: "OpsPilot watches your ECS services, ALB metrics, and application logs 24/7 — task counts, error rates, latency, crash loops.",
  },
  {
    icon: Stethoscope,
    title: "Automatic diagnosis",
    body: "When something breaks, the AI pulls logs, events, and your project's history, and has the root cause analysis ready instantly.",
  },
  {
    icon: MessageSquare,
    title: "Conversational control",
    body: "Deploy, roll back, scale, and inspect costs in plain English. The AI classifies intent — real code executes the action.",
  },
];

const STEPS = [
  { icon: GitBranch, title: "Connect", body: "Link your GitHub repo and AWS account. OpsPilot assumes a scoped IAM role you control — your credentials never leave your account." },
  { icon: Rocket, title: "Deploy", body: "OpsPilot provisions the VPC, cluster, load balancer, and build pipeline, then ships your first deploy." },
  { icon: Eye, title: "Relax", body: "The AI watches your production around the clock. You get alerts with answers, not surprises." },
];

const TIERS = [
  {
    name: "Free",
    price: "$0",
    features: ["1 project", "1 environment", "10 AI actions/month"],
    cta: "Start free",
    highlight: false,
  },
  {
    name: "Pro",
    price: "$49/mo",
    features: ["5 projects", "Full 24/7 monitoring", "Unlimited AI actions", "Email alerts"],
    cta: "Start with Pro",
    highlight: true,
  },
  {
    name: "Team",
    price: "$149/mo",
    features: ["Unlimited projects", "Team members (coming soon)", "Slack integration (coming soon)"],
    cta: "Start with Team",
    highlight: false,
  },
];

export default function LandingPage() {
  const { isLoaded, isSignedIn } = useAuth();
  const router = useRouter();

  useEffect(() => {
    if (isLoaded && isSignedIn) router.replace("/projects");
  }, [isLoaded, isSignedIn, router]);

  if (!isLoaded || isSignedIn) return null;

  return (
    <div className="min-h-screen bg-zinc-50">
      {/* Top bar */}
      <header className="border-b bg-white">
        <div className="max-w-6xl mx-auto px-4 h-14 flex items-center justify-between">
          <span className="flex items-center gap-2 font-semibold text-sm">
            <Rocket className="h-4 w-4 text-indigo-600" />
            OpsPilot
          </span>
          <nav className="flex items-center gap-4 text-sm">
            <Link href="/sign-in" className="text-muted-foreground hover:text-foreground">Sign in</Link>
            <Button size="sm" nativeButton={false} render={<Link href="/sign-up" />}>Start free</Button>
          </nav>
        </div>
      </header>

      {/* Hero */}
      <section className="max-w-4xl mx-auto px-4 pt-20 pb-16 text-center">
        <h1 className="text-4xl sm:text-5xl font-bold tracking-tight">
          Your infrastructure has a 24/7 AI engineer watching it
        </h1>
        <p className="mt-5 text-lg text-muted-foreground max-w-2xl mx-auto">
          OpsPilot monitors your AWS deployments, diagnoses failures automatically,
          and alerts you before users notice something is wrong.
        </p>
        <div className="mt-8 flex justify-center gap-3">
          <Button size="lg" nativeButton={false} render={<Link href="/sign-up" />}>
            Start free
            <ArrowRight className="h-4 w-4 ml-2" />
          </Button>
          <Button
            size="lg"
            variant="outline"
            onClick={() => document.getElementById("how-it-works")?.scrollIntoView({ behavior: "smooth" })}
          >
            See how it works
          </Button>
        </div>
      </section>

      {/* Problem / solution */}
      <section className="max-w-5xl mx-auto px-4 pb-20 grid sm:grid-cols-2 gap-4">
        <div className="rounded-xl border border-red-200 bg-red-50/50 p-6">
          <p className="flex items-center gap-2 text-sm font-semibold text-red-800">
            <AlertTriangle className="h-4 w-4" />
            Without OpsPilot
          </p>
          <p className="mt-3 text-sm leading-relaxed text-red-900/80">
            2am PagerDuty alert. You open your laptop, tail logs for an hour,
            cross-reference deploy timestamps, and finally find the connection
            pool exhaustion someone shipped on Friday afternoon.
          </p>
        </div>
        <div className="rounded-xl border border-green-200 bg-green-50/50 p-6">
          <p className="flex items-center gap-2 text-sm font-semibold text-green-800">
            <CheckCircle2 className="h-4 w-4" />
            With OpsPilot
          </p>
          <p className="mt-3 text-sm leading-relaxed text-green-900/80">
            The AI detects the anomaly within a minute, diagnoses the root cause
            from logs and deployment history, and has the answer — and the fix —
            waiting before you even open your laptop.
          </p>
        </div>
      </section>

      {/* Features */}
      <section className="bg-white border-y">
        <div className="max-w-5xl mx-auto px-4 py-20 grid sm:grid-cols-3 gap-8">
          {FEATURES.map((f) => (
            <div key={f.title}>
              <div className="h-10 w-10 rounded-lg bg-indigo-50 flex items-center justify-center">
                <f.icon className="h-5 w-5 text-indigo-600" />
              </div>
              <h3 className="mt-4 font-semibold">{f.title}</h3>
              <p className="mt-2 text-sm text-muted-foreground leading-relaxed">{f.body}</p>
            </div>
          ))}
        </div>
      </section>

      {/* How it works */}
      <section id="how-it-works" className="max-w-5xl mx-auto px-4 py-20">
        <h2 className="text-2xl font-bold text-center">How it works</h2>
        <div className="mt-10 grid sm:grid-cols-3 gap-8">
          {STEPS.map((step, i) => (
            <div key={step.title} className="relative rounded-xl border bg-white p-6">
              <span className="absolute -top-3 left-6 inline-flex h-6 w-6 items-center justify-center rounded-full bg-indigo-600 text-white text-xs font-semibold">
                {i + 1}
              </span>
              <step.icon className="h-5 w-5 text-indigo-600" />
              <h3 className="mt-3 font-semibold">{step.title}</h3>
              <p className="mt-2 text-sm text-muted-foreground leading-relaxed">{step.body}</p>
            </div>
          ))}
        </div>
      </section>

      {/* Pricing */}
      <section className="bg-white border-y">
        <div className="max-w-5xl mx-auto px-4 py-20">
          <h2 className="text-2xl font-bold text-center">Simple pricing</h2>
          <p className="mt-2 text-sm text-muted-foreground text-center flex items-center justify-center gap-1.5">
            <Cloud className="h-4 w-4" />
            Your infrastructure, your AWS account. No vendor lock-in.
          </p>
          <div className="mt-10 grid sm:grid-cols-3 gap-4">
            {TIERS.map((tier) => (
              <div
                key={tier.name}
                className={
                  tier.highlight
                    ? "rounded-xl border-2 border-indigo-600 bg-white p-6 shadow-sm"
                    : "rounded-xl border bg-white p-6"
                }
              >
                <h3 className="font-semibold">{tier.name}</h3>
                <p className="mt-1 text-2xl font-bold">{tier.price}</p>
                <ul className="mt-4 space-y-2 text-sm text-muted-foreground">
                  {tier.features.map((f) => (
                    <li key={f} className="flex items-start gap-2">
                      <CheckCircle2 className="h-4 w-4 text-indigo-600 mt-0.5 shrink-0" />
                      {f}
                    </li>
                  ))}
                </ul>
                <Button
                  className="mt-6 w-full"
                  variant={tier.highlight ? "default" : "outline"}
                  nativeButton={false}
                  render={<Link href="/sign-up" />}
                >
                  {tier.cta}
                </Button>
              </div>
            ))}
          </div>
        </div>
      </section>
    </div>
  );
}
