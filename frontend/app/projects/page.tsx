"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { listProjects, deleteProject } from "@/lib/api";
import { YesterdaySummaryCard } from "@/components/summary/yesterday-card";
import { OnboardingChecklist } from "@/components/onboarding/checklist";
import type { Project } from "@/types/api";
import { Plus, GitBranch, Rocket, Trash2, Loader2 } from "lucide-react";
import { toast } from "sonner";

const FRAMEWORK_LABELS: Record<string, string> = {
  fastapi: "FastAPI",
  flask: "Flask",
  django: "Django",
  python: "Python",
  nodejs: "Node.js",
  express: "Express",
  nestjs: "NestJS",
  nextjs: "Next.js",
  remix: "Remix",
  nuxtjs: "Nuxt",
  svelte: "SvelteKit",
  astro: "Astro",
  "react-spa": "React (SPA)",
  vite: "Vite",
  go: "Go",
  rails: "Rails",
  spring: "Spring Boot",
  static: "Static / HTML",
};

export default function ProjectsPage() {
  const { getToken } = useAuth();
  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);

  // delete dialog state
  const [deleteTarget, setDeleteTarget] = useState<Project | null>(null);
  const [confirmName, setConfirmName] = useState("");
  const [deleting, setDeleting] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    getToken().then((t) => {
      if (!t) {
        setLoading(false);
        return;
      }
      listProjects(t)
        .then(setProjects)
        .catch((e: Error) => toast.error(e.message))
        .finally(() => setLoading(false));
    });
  }, [getToken]);

  // focus the input when the dialog opens
  useEffect(() => {
    if (deleteTarget) {
      setTimeout(() => inputRef.current?.focus(), 50);
    }
  }, [deleteTarget]);

  function openDelete(e: React.MouseEvent, p: Project) {
    e.preventDefault();  // don't navigate to project detail
    e.stopPropagation();
    setDeleteTarget(p);
    setConfirmName("");
  }

  function closeDelete() {
    if (deleting) return;
    setDeleteTarget(null);
    setConfirmName("");
  }

  async function handleConfirmDelete() {
    if (!deleteTarget || confirmName !== deleteTarget.name) return;
    const token = await getToken();
    if (!token) return;
    setDeleting(true);
    try {
      await deleteProject(token, deleteTarget.id);
      setProjects((prev) => prev.filter((p) => p.id !== deleteTarget.id));
      toast.success(`"${deleteTarget.name}" deleted — AWS resources are being cleaned up in the background.`);
      setDeleteTarget(null);
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to delete project");
    } finally {
      setDeleting(false);
    }
  }

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />

      {/* Delete confirmation dialog */}
      <Dialog open={!!deleteTarget} onOpenChange={(v) => !v && closeDelete()}>
        <DialogContent className="max-w-sm">
          <DialogHeader>
            <DialogTitle>Delete project</DialogTitle>
            <DialogDescription>
              This permanently removes the project and its deployment history from ConvDeploy.
              All AWS infrastructure provisioned for this project will be torn down in the background.
              <br /><br />
              This action <strong>cannot be undone</strong>.
            </DialogDescription>
          </DialogHeader>
          {deleteTarget && (
            <div className="space-y-3 pt-1">
              <div className="rounded-md border bg-zinc-50 px-3 py-2 text-sm font-mono font-medium">
                {deleteTarget.name}
              </div>
              <div className="space-y-1.5">
                <Label className="text-xs">
                  Type <span className="font-mono font-semibold">{deleteTarget.name}</span> to confirm
                </Label>
                <Input
                  ref={inputRef}
                  value={confirmName}
                  onChange={(e) => setConfirmName(e.target.value)}
                  onKeyDown={(e) => e.key === "Enter" && handleConfirmDelete()}
                  placeholder={deleteTarget.name}
                  autoComplete="off"
                />
              </div>
            </div>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={closeDelete} disabled={deleting}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={confirmName !== deleteTarget?.name || deleting}
              onClick={handleConfirmDelete}
            >
              {deleting
                ? <><Loader2 className="h-4 w-4 mr-2 animate-spin" />Deleting…</>
                : <><Trash2 className="h-4 w-4 mr-2" />Delete project</>
              }
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <main className="max-w-6xl mx-auto px-4 py-10">
        <YesterdaySummaryCard />
        {!loading && <OnboardingChecklist hasProjects={projects.length > 0} />}
        <div className="flex items-center justify-between mb-8">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">Projects</h1>
            <p className="text-muted-foreground text-sm mt-1">
              Deploy and manage your apps through conversation.
            </p>
          </div>
          <Button nativeButton={false} render={<Link href="/projects/new" />}>
            <Plus className="h-4 w-4 mr-2" />
            New Project
          </Button>
        </div>

        {loading ? (
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
            {[1, 2, 3].map((i) => (
              <div key={i} className="h-36 rounded-lg bg-zinc-200 animate-pulse" />
            ))}
          </div>
        ) : projects.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-24 text-center">
            <Rocket className="h-12 w-12 text-zinc-300 mb-4" />
            <h2 className="text-lg font-semibold">No projects yet</h2>
            <p className="text-muted-foreground text-sm mt-1 mb-6">
              Connect a GitHub repo and deploy your first app.
            </p>
            <Button nativeButton={false} render={<Link href="/projects/new" />}>
              <Plus className="h-4 w-4 mr-2" />
              Create your first project
            </Button>
          </div>
        ) : (
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
            {projects.map((p) => (
              <div key={p.id} className="relative group">
                <Link href={`/projects/${p.id}`}>
                  <Card className="hover:shadow-md transition-shadow cursor-pointer h-full">
                    <CardHeader className="pb-2">
                      <div className="flex items-start justify-between">
                        <CardTitle className="text-base pr-6">{p.name}</CardTitle>
                        <Badge variant="secondary" className="text-xs shrink-0">
                          {FRAMEWORK_LABELS[p.framework] ?? p.framework}
                        </Badge>
                      </div>
                      <CardDescription className="flex items-center gap-1 text-xs">
                        <GitBranch className="h-3 w-3" />
                        {p.repo_owner}/{p.repo_name}
                      </CardDescription>
                    </CardHeader>
                    <CardContent>
                      <p className="text-xs text-muted-foreground">{p.branch ?? "default branch"}</p>
                    </CardContent>
                  </Card>
                </Link>
                {/* Delete button — sits on top of the card link, hidden until hover */}
                <button
                  onClick={(e) => openDelete(e, p)}
                  className="absolute top-2.5 right-2.5 p-1 rounded opacity-0 group-hover:opacity-100 transition-opacity text-zinc-400 hover:text-red-500 hover:bg-red-50 z-10"
                  title="Delete project"
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </button>
              </div>
            ))}
          </div>
        )}
      </main>
    </div>
  );
}
