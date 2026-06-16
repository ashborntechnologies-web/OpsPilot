"use client";

import { useEffect, useState } from "react";
import { useAuth } from "@clerk/nextjs";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { getEnvironmentSLA, setEnvironmentSLA } from "@/lib/api";
import type { Environment } from "@/types/api";
import { toast } from "sonner";
import { Loader2, Gauge } from "lucide-react";

function EnvSLACard({ projectId, env, canEdit }: { projectId: string; env: Environment; canEdit: boolean }) {
  const { getToken } = useAuth();
  const [target, setTarget] = useState("99.9");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    (async () => {
      const token = await getToken();
      if (!token) return;
      try {
        const sla = await getEnvironmentSLA(token, projectId, env.id);
        setTarget(String(sla.target_uptime_pct));
      } catch { /* keep default */ } finally {
        setLoading(false);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [env.id]);

  async function save() {
    const token = await getToken();
    if (!token) return;
    const pct = Number(target);
    if (!(pct > 0 && pct <= 100)) {
      toast.error("SLA target must be between 0 and 100");
      return;
    }
    setSaving(true);
    try {
      await setEnvironmentSLA(token, projectId, env.id, { target_uptime_pct: pct });
      toast.success(`SLA target for ${env.name} saved`);
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-base capitalize flex items-center gap-2">
          <Gauge className="h-4 w-4 text-indigo-600" /> {env.name}
        </CardTitle>
        <CardDescription>Target uptime % used by the analytics dashboard to judge SLA status.</CardDescription>
      </CardHeader>
      <CardContent>
        {loading ? (
          <div className="flex justify-center py-2"><Loader2 className="h-4 w-4 animate-spin text-muted-foreground" /></div>
        ) : (
          <div className="flex items-end gap-2">
            <label className="text-xs flex flex-col gap-1">
              SLA target (%)
              <input
                type="number" min={0} max={100} step={0.01} value={target} disabled={!canEdit}
                onChange={(e) => setTarget(e.target.value)}
                className="w-28 h-8 rounded border px-2 text-sm disabled:opacity-60"
              />
            </label>
            {canEdit ? (
              <Button size="sm" onClick={save} disabled={saving}>
                {saving ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null} Save
              </Button>
            ) : (
              <p className="text-xs text-muted-foreground pb-1.5">Only engineers/admins can change SLA targets.</p>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// EnvSLASettings renders an SLA-target card per non-preview environment.
export function EnvSLASettings({ projectId, environments, canEdit }: { projectId: string; environments: Environment[]; canEdit: boolean }) {
  const envs = environments.filter((e) => !e.is_preview);
  if (envs.length === 0) {
    return <p className="text-sm text-muted-foreground">Create an environment to configure SLA targets.</p>;
  }
  return (
    <div className="space-y-4">
      {envs.map((e) => <EnvSLACard key={e.id} projectId={projectId} env={e} canEdit={canEdit} />)}
    </div>
  );
}
