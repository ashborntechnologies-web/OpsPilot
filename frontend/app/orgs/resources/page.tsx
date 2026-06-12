"use client";

import { useMemo, useState } from "react";
import useSWR from "swr";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { listOrgResources, assignResource, listProjects } from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import { RESOURCE_TYPES, RESOURCE_ICONS, resourceLabel, resourceStatus } from "@/lib/resources";
import type { DiscoveredResource, Project, ResourceType } from "@/types/api";
import { cn } from "@/lib/utils";
import { toast } from "sonner";
import { Loader2, Boxes, Tag } from "lucide-react";

export default function OrgResourcesPage() {
  const { getToken } = useAuth();
  const { activeOrg, canAct } = useActiveOrg();
  const orgId = activeOrg?.id;

  const [typeFilter, setTypeFilter] = useState<ResourceType | "">("");
  const [regionFilter, setRegionFilter] = useState("");
  const [assignTarget, setAssignTarget] = useState<DiscoveredResource | null>(null);
  const [assignProject, setAssignProject] = useState("");
  const [assigning, setAssigning] = useState(false);

  const { data: resources, isLoading, mutate } = useSWR<DiscoveredResource[]>(
    orgId ? ["org-resources", orgId, typeFilter, regionFilter] : null,
    async () => {
      const token = await getToken();
      if (!token || !orgId) return [];
      return listOrgResources(token, orgId, { resource_type: typeFilter, region: regionFilter });
    }
  );

  const { data: projects } = useSWR<Project[]>(
    orgId ? ["projects-for-assign", orgId] : null,
    async () => {
      const token = await getToken();
      if (!token) return [];
      return listProjects(token);
    }
  );

  // Region options derived from the loaded resources.
  const regions = useMemo(() => {
    const s = new Set<string>();
    (resources ?? []).forEach((r) => r.region && s.add(r.region));
    return Array.from(s).sort();
  }, [resources]);

  const projectName = (id: string | null) =>
    id ? (projects?.find((p) => p.id === id)?.name ?? "assigned") : null;

  async function handleAssign() {
    if (!assignTarget) return;
    const token = await getToken();
    if (!token) return;
    setAssigning(true);
    try {
      await assignResource(token, assignTarget.id, assignProject || null);
      toast.success(assignProject ? "Resource assigned" : "Resource unassigned");
      setAssignTarget(null);
      setAssignProject("");
      mutate();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setAssigning(false);
    }
  }

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />

      {/* Assign-to-project dialog */}
      <Dialog open={!!assignTarget} onOpenChange={(v) => !v && setAssignTarget(null)}>
        <DialogContent className="max-w-sm">
          <DialogHeader>
            <DialogTitle>Assign resource</DialogTitle>
            <DialogDescription>
              Link <span className="font-mono text-xs">{assignTarget?.resource_name}</span> to a project so it appears on that project&apos;s Infrastructure tab.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5 pt-1">
            <Label className="text-xs">Project</Label>
            <select
              value={assignProject}
              onChange={(e) => setAssignProject(e.target.value)}
              className="w-full h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
            >
              <option value="">— Unassigned —</option>
              {(projects ?? []).map((p) => (
                <option key={p.id} value={p.id}>{p.name}</option>
              ))}
            </select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAssignTarget(null)} disabled={assigning}>Cancel</Button>
            <Button onClick={handleAssign} disabled={assigning}>
              {assigning ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
              Save
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <main className="max-w-5xl mx-auto px-4 py-10">
        <div className="flex items-center gap-2 mb-1">
          <Boxes className="h-5 w-5 text-indigo-600" />
          <h1 className="text-2xl font-bold tracking-tight">Infrastructure inventory</h1>
        </div>
        <p className="text-muted-foreground text-sm mb-6">
          Resources discovered across this workspace&apos;s connected AWS accounts.
        </p>

        {/* Filters */}
        <div className="flex flex-wrap gap-2 mb-4">
          <select
            value={typeFilter}
            onChange={(e) => setTypeFilter(e.target.value as ResourceType | "")}
            className="h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
          >
            <option value="">All types</option>
            {RESOURCE_TYPES.map((t) => <option key={t} value={t}>{resourceLabel(t)}</option>)}
          </select>
          <select
            value={regionFilter}
            onChange={(e) => setRegionFilter(e.target.value)}
            className="h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
          >
            <option value="">All regions</option>
            {regions.map((r) => <option key={r} value={r}>{r}</option>)}
          </select>
        </div>

        {isLoading ? (
          <div className="flex items-center justify-center h-40">
            <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
          </div>
        ) : (resources ?? []).length === 0 ? (
          <Card className="border-dashed">
            <CardContent className="py-12 text-center">
              <Boxes className="h-10 w-10 text-zinc-300 mx-auto mb-3" />
              <p className="font-medium text-sm">No resources discovered yet</p>
              <p className="text-muted-foreground text-xs mt-1">
                Connect an AWS account or run a scan to populate this inventory.
              </p>
            </CardContent>
          </Card>
        ) : (
          <div className="rounded-lg border bg-white divide-y">
            {(resources ?? []).map((r) => {
              const Icon = RESOURCE_ICONS[r.resource_type] ?? Tag;
              return (
                <div key={r.id} className="flex items-center gap-3 px-4 py-2.5">
                  <Icon className="h-4 w-4 text-zinc-500 shrink-0" />
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium truncate">{r.resource_name || r.resource_id}</span>
                      {r.is_managed && (
                        <span className="rounded bg-indigo-100 px-1.5 py-0.5 text-[10px] font-medium text-indigo-700">OpsPilot</span>
                      )}
                    </div>
                    <p className="text-xs text-muted-foreground">
                      {resourceLabel(r.resource_type)} · {r.region || "global"} · {resourceStatus(r)}
                    </p>
                  </div>
                  <div className="shrink-0 flex items-center gap-2">
                    {r.project_id ? (
                      <span className="text-xs text-muted-foreground">{projectName(r.project_id)}</span>
                    ) : (
                      <span className="text-xs text-amber-600">Unassigned</span>
                    )}
                    {canAct && (
                      <Button
                        size="sm" variant="outline" className="h-7 text-xs"
                        onClick={() => { setAssignTarget(r); setAssignProject(r.project_id ?? ""); }}
                      >
                        {r.project_id ? "Reassign" : "Assign"}
                      </Button>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </main>
    </div>
  );
}
