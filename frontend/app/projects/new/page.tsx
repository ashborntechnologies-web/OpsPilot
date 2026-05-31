"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { ConnectAWSModal } from "@/components/project/connect-aws-modal";
import { getGithubAuthURL, listGithubRepos, listGithubBranches, detectFramework, createProject, listAWSAccounts } from "@/lib/api";
import type { GithubRepo, AWSAccount } from "@/types/api";
import { toast } from "sonner";
import { GitBranch, Search, ArrowLeft, Check, Plus, Cloud } from "lucide-react";

type Step = "github" | "repo" | "details";

const FRAMEWORK_GROUPS = [
  {
    group: "Python",
    color: "bg-yellow-400",
    items: [
      { value: "fastapi",  label: "FastAPI",        desc: "Async Python API" },
      { value: "flask",    label: "Flask",           desc: "Lightweight Python" },
      { value: "django",   label: "Django",          desc: "Batteries-included" },
      { value: "python",   label: "Python Script",   desc: "Any Python app" },
    ],
  },
  {
    group: "JavaScript / TypeScript",
    color: "bg-blue-400",
    items: [
      { value: "nextjs",  label: "Next.js",    desc: "React + SSR" },
      { value: "remix",   label: "Remix",      desc: "Full-stack React" },
      { value: "nuxtjs",  label: "Nuxt.js",    desc: "Vue framework" },
      { value: "svelte",  label: "SvelteKit",  desc: "Svelte framework" },
      { value: "astro",   label: "Astro",      desc: "Content-first" },
      { value: "nestjs",  label: "NestJS",     desc: "Enterprise Node.js" },
      { value: "express", label: "Express.js", desc: "Minimal Node.js" },
      { value: "nodejs",  label: "Node.js",    desc: "Any Node.js app" },
    ],
  },
  {
    group: "Go",
    color: "bg-cyan-400",
    items: [
      { value: "go", label: "Go", desc: "Any Go HTTP server" },
    ],
  },
] as const;

const DEFAULT_START_COMMANDS: Record<string, string> = {
  fastapi:  "uvicorn main:app --host 0.0.0.0 --port 8000",
  flask:    "gunicorn main:app -b 0.0.0.0:8000",
  django:   "gunicorn config.wsgi:application -b 0.0.0.0:8000",
  python:   "python main.py",
  nodejs:   "node index.js",
  express:  "node index.js",
  nestjs:   "node dist/main.js",
  nextjs:   "node server.js",
  remix:    "node server.js",
  nuxtjs:   "node .output/server/index.mjs",
  svelte:   "node build/index.js",
  astro:    "node ./dist/server/entry.mjs",
  go:       "./server",
};

export default function NewProjectPage() {
  const { getToken } = useAuth();
  const router = useRouter();

  const [step, setStep] = useState<Step>("github");
  const [repos, setRepos] = useState<GithubRepo[]>([]);
  const [filtered, setFiltered] = useState<GithubRepo[]>([]);
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<GithubRepo | null>(null);
  const [name, setName] = useState("");
  const [framework, setFramework] = useState("");
  const [branches, setBranches] = useState<string[]>([]);
  const [branch, setBranch] = useState("");
  const [startCommand, setStartCommand] = useState("");
  const [accountId, setAccountId] = useState<string>("");
  const [accounts, setAccounts] = useState<AWSAccount[]>([]);
  const [connectOpen, setConnectOpen] = useState(false);
  const [loadingRepos, setLoadingRepos] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    setFiltered(
      repos.filter((r) => r.full_name.toLowerCase().includes(search.toLowerCase()))
    );
  }, [repos, search]);

  // Auto-detect if GitHub is already connected — skip the connect step
  useEffect(() => {
    if (step !== "github") return;
    (async () => {
      const token = await getToken();
      if (!token) return;
      try {
        const data = await listGithubRepos(token);
        setRepos(data);
        setStep("repo");
      } catch {
        // Not connected yet — show the connect buttons
      }
    })();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function loadAccounts() {
    const token = await getToken();
    if (!token) return;
    try {
      const data = await listAWSAccounts(token);
      setAccounts(data ?? []);
      if (data?.length === 1) setAccountId(data[0].id);
    } catch {
      // non-fatal
    }
  }

  useEffect(() => {
    loadAccounts();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function handleConnectGithub() {
    const token = await getToken();
    if (!token) return;
    try {
      const { url } = await getGithubAuthURL(token);
      window.location.href = url;
    } catch {
      loadRepos(token);
    }
  }

  async function loadRepos(token: string) {
    setLoadingRepos(true);
    try {
      const data = await listGithubRepos(token);
      setRepos(data);
      setStep("repo");
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to load repos");
    } finally {
      setLoadingRepos(false);
    }
  }

  async function handleSelectRepo(repo: GithubRepo) {
    setSelected(repo);
    setName(repo.name);
    const token = await getToken();
    if (!token) return;
    const owner = repo.full_name.split("/")[0];

    // Fetch branches and detect framework in parallel
    const [branchList, frameworkResult] = await Promise.allSettled([
      listGithubBranches(token, owner, repo.name),
      detectFramework(token, owner, repo.name),
    ]);

    if (branchList.status === "fulfilled") {
      setBranches(branchList.value);
      // Pre-select main/master if present, otherwise first branch
      const preferred = ["main", "master"].find((b) => branchList.value.includes(b)) ?? branchList.value[0] ?? "";
      setBranch(preferred);
    }

    if (frameworkResult.status === "fulfilled") {
      const detected = frameworkResult.value.framework;
      setFramework(detected);
      setStartCommand(DEFAULT_START_COMMANDS[detected] ?? "");
    }

    setStep("details");
  }

  async function handleCreate() {
    if (!selected || !name || !framework) return;
    const token = await getToken();
    if (!token) return;
    setSubmitting(true);
    try {
      const project = await createProject(token, {
        name,
        repo_url: selected.html_url,
        repo_owner: selected.full_name.split("/")[0],
        repo_name: selected.name,
        framework,
        branch: branch || null,
        start_command: startCommand || null,
        account_id: accountId || null,
      });
      toast.success("Project created!");
      router.push(`/projects/${project.id}`);
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to create project");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />

      <ConnectAWSModal
        open={connectOpen}
        onClose={() => setConnectOpen(false)}
        onConnected={(account) => {
          setAccounts((prev) => [account, ...prev]);
          setAccountId(account.id);
          setConnectOpen(false);
        }}
      />

      <main className="max-w-2xl mx-auto px-4 py-10">
        <button
          onClick={() => step === "github" ? router.push("/projects") : setStep(step === "details" ? "repo" : "github")}
          className="flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground mb-6"
        >
          <ArrowLeft className="h-3 w-3" /> Back
        </button>

        <h1 className="text-2xl font-bold tracking-tight mb-2">New Project</h1>

        {/* Step indicator */}
        <div className="flex items-center gap-2 mb-8">
          {(["github", "repo", "details"] as Step[]).map((s, i) => (
            <div key={s} className="flex items-center gap-2">
              <div className={`w-6 h-6 rounded-full flex items-center justify-center text-xs font-medium ${
                s === step ? "bg-indigo-600 text-white" :
                (["github", "repo", "details"].indexOf(step) > i) ? "bg-green-500 text-white" :
                "bg-zinc-200 text-zinc-500"
              }`}>
                {(["github", "repo", "details"].indexOf(step) > i) ? <Check className="h-3 w-3" /> : i + 1}
              </div>
              {i < 2 && <div className="h-px w-8 bg-zinc-200" />}
            </div>
          ))}
          <span className="ml-2 text-sm text-muted-foreground capitalize">{step}</span>
        </div>

        {/* Step: GitHub */}
        {step === "github" && (
          <Card>
            <CardHeader>
              <CardTitle>Connect GitHub</CardTitle>
              <CardDescription>
                Authorize ConvDeploy to read your repositories.
              </CardDescription>
            </CardHeader>
            <CardContent className="flex flex-col gap-3">
              <Button onClick={handleConnectGithub} disabled={loadingRepos}>
                <GitBranch className="h-4 w-4 mr-2" />
                {loadingRepos ? "Loading repos..." : "Connect GitHub"}
              </Button>
              <Button variant="outline" disabled={loadingRepos} onClick={async () => {
                const token = await getToken();
                if (token) loadRepos(token);
              }}>
                Already connected — load repos
              </Button>
            </CardContent>
          </Card>
        )}

        {/* Step: Repo picker */}
        {step === "repo" && (
          <Card>
            <CardHeader>
              <CardTitle>Choose a Repository</CardTitle>
              <CardDescription>{repos.length} repos found</CardDescription>
            </CardHeader>
            <CardContent>
              <div className="relative mb-3">
                <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
                <Input
                  className="pl-8"
                  placeholder="Search repos..."
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                />
              </div>
              <div className="max-h-72 overflow-y-auto space-y-1">
                {filtered.map((r) => (
                  <button
                    key={r.full_name}
                    className="w-full text-left px-3 py-2 rounded-md hover:bg-zinc-100 flex items-center justify-between text-sm"
                    onClick={() => handleSelectRepo(r)}
                  >
                    <span className="font-medium">{r.full_name}</span>
                    {r.private && <Badge variant="outline" className="text-xs">Private</Badge>}
                  </button>
                ))}
                {filtered.length === 0 && (
                  <p className="text-center text-muted-foreground text-sm py-8">No repos found</p>
                )}
              </div>
            </CardContent>
          </Card>
        )}

        {/* Step: Details */}
        {step === "details" && selected && (
          <Card>
            <CardHeader>
              <CardTitle>Project Details</CardTitle>
              <CardDescription>{selected.full_name}</CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="space-y-1.5">
                <Label>Project name</Label>
                <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="my-app" />
              </div>
              <div className="space-y-1.5">
                <Label>Branch</Label>
                <Select value={branch} onValueChange={(v) => { if (v) setBranch(v); }}>
                  <SelectTrigger>
                    <SelectValue placeholder="Select branch" />
                  </SelectTrigger>
                  <SelectContent>
                    {branches.map((b) => (
                      <SelectItem key={b} value={b}>{b}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label>Framework</Label>
                <div className="space-y-3">
                  {FRAMEWORK_GROUPS.map((group) => (
                    <div key={group.group}>
                      <p className="text-xs text-muted-foreground font-medium mb-1.5 flex items-center gap-1.5">
                        <span className={`inline-block h-2 w-2 rounded-full ${group.color}`} />
                        {group.group}
                      </p>
                      <div className="grid grid-cols-2 gap-1.5 sm:grid-cols-4">
                        {group.items.map((f) => (
                          <button
                            key={f.value}
                            type="button"
                            onClick={() => {
                              setFramework(f.value);
                              setStartCommand((prev) =>
                                !prev || Object.values(DEFAULT_START_COMMANDS).includes(prev)
                                  ? (DEFAULT_START_COMMANDS[f.value] ?? "")
                                  : prev
                              );
                            }}
                            className={`text-left rounded-lg border px-3 py-2 transition-colors ${
                              framework === f.value
                                ? "border-indigo-500 bg-indigo-50 ring-1 ring-indigo-500"
                                : "border-input hover:bg-zinc-50"
                            }`}
                          >
                            <p className="text-xs font-medium leading-none">{f.label}</p>
                            <p className="text-xs text-muted-foreground mt-0.5">{f.desc}</p>
                          </button>
                        ))}
                      </div>
                    </div>
                  ))}
                </div>
              </div>
              <div className="space-y-1.5">
                <Label>Start command</Label>
                <Input
                  value={startCommand}
                  onChange={(e) => setStartCommand(e.target.value)}
                  placeholder={framework ? (DEFAULT_START_COMMANDS[framework] ?? "./start.sh") : "Select a framework first"}
                  className="font-mono text-sm"
                />
                <p className="text-xs text-muted-foreground">
                  The command used to start your app. Must bind to <span className="font-mono">0.0.0.0</span> on port <span className="font-mono">8000</span> (or <span className="font-mono">3000</span> for Node.js).
                  Example: <span className="font-mono">uvicorn app.main:app --host 0.0.0.0 --port 8000</span>
                </p>
              </div>
              <div className="space-y-1.5">
                <Label>AWS Account</Label>
                {accounts.length === 0 ? (
                  <div className="rounded-lg border border-dashed p-4 flex flex-col items-center gap-2 text-center">
                    <Cloud className="h-6 w-6 text-zinc-300" />
                    <p className="text-xs text-muted-foreground">No AWS accounts connected yet.</p>
                    <Button size="sm" variant="outline" onClick={() => setConnectOpen(true)}>
                      <Plus className="h-3 w-3 mr-1" /> Connect Account
                    </Button>
                  </div>
                ) : (
                  <div className="flex gap-2">
                    <Select value={accountId} onValueChange={(v) => { if (v) setAccountId(v); }}>
                      <SelectTrigger className="flex-1">
                        <SelectValue placeholder="Select AWS account" />
                      </SelectTrigger>
                      <SelectContent>
                        {accounts.map((a) => (
                          <SelectItem key={a.id} value={a.id}>
                            <span className="font-medium">{a.label}</span>
                            <span className="ml-2 text-muted-foreground font-mono text-xs">{a.aws_account_id}</span>
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <Button size="sm" variant="outline" onClick={() => setConnectOpen(true)}>
                      <Plus className="h-3 w-3" />
                    </Button>
                  </div>
                )}
                <p className="text-xs text-muted-foreground">
                  AWS region is chosen per environment when you deploy.
                </p>
              </div>
              <Button
                className="w-full"
                onClick={handleCreate}
                disabled={submitting || !name || !framework}
              >
                {submitting ? "Creating..." : "Create Project"}
              </Button>
            </CardContent>
          </Card>
        )}
      </main>
    </div>
  );
}
