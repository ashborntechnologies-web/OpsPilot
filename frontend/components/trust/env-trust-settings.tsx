"use client";

import { useEffect, useState } from "react";
import { useAuth } from "@clerk/nextjs";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { getEnvironmentTrust, updateEnvironmentTrust } from "@/lib/api";
import type { Environment, TrustLevel, AutonomousBoundaries } from "@/types/api";
import { toast } from "sonner";
import { Loader2, ShieldCheck } from "lucide-react";
import { cn } from "@/lib/utils";

const TRUST_DESCRIPTIONS: Record<TrustLevel, string> = {
  suggest: "AI only suggests actions. Every AI-initiated action waits for human approval.",
  supervised: "Every AI action still needs approval, but proposals are surfaced prominently for fast review.",
  autonomous: "AI auto-executes actions that fall within the boundaries below. Anything outside still needs approval (deploys always do).",
};

const DEFAULT_BOUNDARIES: AutonomousBoundaries = {
  can_rollback: true, can_scale: true, min_replicas: 1, max_replicas: 5, can_change_resources: false,
};

function EnvCard({ projectId, env, canEdit }: { projectId: string; env: Environment; canEdit: boolean }) {
  const { getToken } = useAuth();
  const [level, setLevel] = useState<TrustLevel>("suggest");
  const [bounds, setBounds] = useState<AutonomousBoundaries>(DEFAULT_BOUNDARIES);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    (async () => {
      const token = await getToken();
      if (!token) return;
      try {
        const t = await getEnvironmentTrust(token, projectId, env.id);
        setLevel(t.trust_level);
        if (t.autonomous_boundaries) setBounds(t.autonomous_boundaries);
      } catch { /* keep defaults */ } finally {
        setLoading(false);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [env.id]);

  async function save() {
    const token = await getToken();
    if (!token) return;
    setSaving(true);
    try {
      await updateEnvironmentTrust(token, projectId, env.id, {
        trust_level: level,
        autonomous_boundaries: level === "autonomous" ? bounds : undefined,
      });
      toast.success(`Trust level for ${env.name} saved`);
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  const toggle = (k: keyof AutonomousBoundaries) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setBounds((b) => ({ ...b, [k]: e.target.checked }));

  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-base capitalize flex items-center gap-2">
          <ShieldCheck className="h-4 w-4 text-indigo-600" /> {env.name}
        </CardTitle>
        <CardDescription>How much autonomy AI-initiated actions have on this environment.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {loading ? (
          <div className="flex justify-center py-4"><Loader2 className="h-4 w-4 animate-spin text-muted-foreground" /></div>
        ) : (
          <>
            <div className="space-y-2">
              {(["suggest", "supervised", "autonomous"] as TrustLevel[]).map((l) => (
                <label key={l} className={cn("flex gap-2 rounded-md border p-2.5 cursor-pointer", level === l && "border-indigo-400 bg-indigo-50/40", !canEdit && "opacity-60 cursor-not-allowed")}>
                  <input type="radio" name={`trust-${env.id}`} checked={level === l} disabled={!canEdit} onChange={() => setLevel(l)} className="mt-0.5" />
                  <span>
                    <span className="text-sm font-medium capitalize">{l}</span>
                    <span className="block text-xs text-muted-foreground">{TRUST_DESCRIPTIONS[l]}</span>
                  </span>
                </label>
              ))}
            </div>

            {level === "autonomous" && (
              <div className="rounded-md border bg-zinc-50 p-3 space-y-2">
                <p className="text-xs font-medium">Autonomous boundaries</p>
                <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={bounds.can_rollback} disabled={!canEdit} onChange={toggle("can_rollback")} /> Allow auto-rollback</label>
                <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={bounds.can_scale} disabled={!canEdit} onChange={toggle("can_scale")} /> Allow auto-scale</label>
                {bounds.can_scale && (
                  <div className="flex items-center gap-3 ml-6">
                    <label className="text-xs flex items-center gap-1">min
                      <input type="number" min={0} max={50} value={bounds.min_replicas} disabled={!canEdit}
                        onChange={(e) => setBounds((b) => ({ ...b, min_replicas: Number(e.target.value) }))}
                        className="w-16 h-7 rounded border px-2 text-sm" /></label>
                    <label className="text-xs flex items-center gap-1">max
                      <input type="number" min={1} max={50} value={bounds.max_replicas} disabled={!canEdit}
                        onChange={(e) => setBounds((b) => ({ ...b, max_replicas: Number(e.target.value) }))}
                        className="w-16 h-7 rounded border px-2 text-sm" /></label>
                  </div>
                )}
                <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={bounds.can_change_resources} disabled={!canEdit} onChange={toggle("can_change_resources")} /> Allow auto resource changes</label>
              </div>
            )}

            {canEdit ? (
              <Button size="sm" onClick={save} disabled={saving}>
                {saving ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null} Save
              </Button>
            ) : (
              <p className="text-xs text-muted-foreground" title="Only workspace admins can change trust levels">Only admins can change trust levels.</p>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

// EnvTrustSettings renders a trust-config card per non-preview environment.
export function EnvTrustSettings({ projectId, environments, canEdit }: { projectId: string; environments: Environment[]; canEdit: boolean }) {
  const envs = environments.filter((e) => !e.is_preview);
  if (envs.length === 0) {
    return <p className="text-sm text-muted-foreground">Create an environment to configure AI trust levels.</p>;
  }
  return (
    <div className="space-y-4">
      {envs.map((e) => <EnvCard key={e.id} projectId={projectId} env={e} canEdit={canEdit} />)}
    </div>
  );
}
