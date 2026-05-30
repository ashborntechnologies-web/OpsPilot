"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { listProjects } from "@/lib/api";
import type { Project } from "@/types/api";
import { Plus, GitBranch, Rocket } from "lucide-react";

const FRAMEWORK_LABELS: Record<string, string> = {
  fastapi: "FastAPI",
  flask: "Flask",
  nodejs: "Node.js",
  nextjs: "Next.js",
};

export default function ProjectsPage() {
  const { getToken } = useAuth();
  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    getToken().then((t) => {
      if (!t) return;
      listProjects(t)
        .then(setProjects)
        .finally(() => setLoading(false));
    });
  }, [getToken]);

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-6xl mx-auto px-4 py-10">
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
              <Link key={p.id} href={`/projects/${p.id}`}>
                <Card className="hover:shadow-md transition-shadow cursor-pointer h-full">
                  <CardHeader className="pb-2">
                    <div className="flex items-start justify-between">
                      <CardTitle className="text-base">{p.name}</CardTitle>
                      <Badge variant="secondary" className="text-xs">
                        {FRAMEWORK_LABELS[p.framework] ?? p.framework}
                      </Badge>
                    </div>
                    <CardDescription className="flex items-center gap-1 text-xs">
                      <GitBranch className="h-3 w-3" />
                      {p.repo_owner}/{p.repo_name}
                    </CardDescription>
                  </CardHeader>
                  <CardContent>
                    <p className="text-xs text-muted-foreground">{p.aws_region}</p>
                  </CardContent>
                </Card>
              </Link>
            ))}
          </div>
        )}
      </main>
    </div>
  );
}
