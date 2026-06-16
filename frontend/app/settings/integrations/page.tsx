"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import useSWR from "swr";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import {
  getSlackStatus, getSlackInstallURL, listSlackChannels, updateSlackChannels, disconnectSlack,
} from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import type { SlackStatus, SlackChannel } from "@/types/api";
import { toast } from "sonner";
import { Loader2, MessageSquare, Plug, Check } from "lucide-react";
import { cn } from "@/lib/utils";

function SettingsNav({ active }: { active: "organization" | "integrations" }) {
  const tab = (id: string, label: string, href: string) => (
    <Link
      href={href}
      className={cn("px-3 py-1.5 text-sm rounded-md", active === id ? "bg-zinc-900 text-white" : "hover:bg-zinc-100")}
    >
      {label}
    </Link>
  );
  return (
    <div className="flex gap-1 mb-6">
      {tab("organization", "Members", "/settings/organization")}
      {tab("integrations", "Integrations", "/settings/integrations")}
    </div>
  );
}

export default function IntegrationsPage() {
  const { getToken } = useAuth();
  const { activeOrg, isAdmin } = useActiveOrg();
  const orgId = activeOrg?.id;
  const search = useSearchParams();

  const [connecting, setConnecting] = useState(false);
  const [saving, setSaving] = useState(false);
  const [alertCh, setAlertCh] = useState("");
  const [deployCh, setDeployCh] = useState("");
  const [summaryCh, setSummaryCh] = useState("");
  const seeded = useState(false);

  // OAuth result toast (?slack=connected|error).
  useEffect(() => {
    const r = search.get("slack");
    if (r === "connected") toast.success("Slack connected");
    else if (r === "error") toast.error("Slack connection failed — please try again");
  }, [search]);

  const { data: status, isLoading, mutate } = useSWR<SlackStatus>(
    orgId ? ["slack-status", orgId] : null,
    async () => {
      const token = await getToken();
      if (!token || !orgId) return { connected: false, configured: false };
      return getSlackStatus(token, orgId);
    }
  );

  const { data: channels } = useSWR<SlackChannel[]>(
    status?.connected && orgId ? ["slack-channels", orgId] : null,
    async () => {
      const token = await getToken();
      if (!token || !orgId) return [];
      return listSlackChannels(token, orgId);
    }
  );

  // Seed dropdowns from the saved integration once.
  useEffect(() => {
    const integ = status?.integration;
    if (!integ || seeded[0]) return;
    setAlertCh(integ.alert_channel_id ?? "");
    setDeployCh(integ.deploy_channel_id ?? "");
    setSummaryCh(integ.summary_channel_id ?? "");
    seeded[1](true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  const channelName = useMemo(() => {
    const m = new Map((channels ?? []).map((c) => [c.id, c.name]));
    return (id: string) => m.get(id) ?? "";
  }, [channels]);

  async function handleConnect() {
    if (!orgId) return;
    const token = await getToken();
    if (!token) return;
    setConnecting(true);
    try {
      const { url } = await getSlackInstallURL(token, orgId);
      window.location.href = url; // off to Slack's OAuth consent
    } catch (e: unknown) {
      toast.error((e as Error).message);
      setConnecting(false);
    }
  }

  async function handleSaveChannels() {
    if (!orgId) return;
    const token = await getToken();
    if (!token) return;
    setSaving(true);
    try {
      await updateSlackChannels(token, orgId, {
        alert_channel_id: alertCh || null, alert_channel_name: channelName(alertCh) || null,
        deploy_channel_id: deployCh || null, deploy_channel_name: channelName(deployCh) || null,
        summary_channel_id: summaryCh || null, summary_channel_name: channelName(summaryCh) || null,
      });
      toast.success("Channels updated");
      mutate();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function handleDisconnect() {
    if (!orgId || !window.confirm("Disconnect Slack from this workspace?")) return;
    const token = await getToken();
    if (!token) return;
    try {
      await disconnectSlack(token, orgId);
      toast.success("Slack disconnected");
      mutate();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    }
  }

  const channelSelect = (value: string, onChange: (v: string) => void) => (
    <select
      value={value}
      disabled={!isAdmin}
      onChange={(e) => onChange(e.target.value)}
      className="w-full h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring disabled:opacity-60"
    >
      <option value="">— None —</option>
      {(channels ?? []).map((c) => <option key={c.id} value={c.id}>#{c.name}</option>)}
    </select>
  );

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-3xl mx-auto px-4 py-10">
        <div className="flex items-center gap-2 mb-1">
          <Plug className="h-5 w-5 text-indigo-600" />
          <h1 className="text-2xl font-bold tracking-tight">{activeOrg?.name ?? "Workspace"}</h1>
        </div>
        <p className="text-muted-foreground text-sm mb-6">Connect OpsPilot to the tools your team already uses.</p>
        <SettingsNav active="integrations" />

        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-base flex items-center gap-2">
              <MessageSquare className="h-4 w-4" /> Slack
            </CardTitle>
            <CardDescription>Alert + deploy notifications, a daily summary, and the <code>/opspilot</code> slash command.</CardDescription>
          </CardHeader>
          <CardContent>
            {isLoading ? (
              <div className="flex items-center justify-center py-8"><Loader2 className="h-5 w-5 animate-spin text-muted-foreground" /></div>
            ) : !status?.configured ? (
              <p className="text-sm text-muted-foreground">Slack is not configured on this server. Set <code>SLACK_CLIENT_ID</code>, <code>SLACK_CLIENT_SECRET</code>, and <code>SLACK_SIGNING_SECRET</code>.</p>
            ) : !status?.connected ? (
              <div>
                <p className="text-sm text-muted-foreground mb-4">
                  {isAdmin ? "Connect your Slack workspace to start receiving notifications." : "Ask an admin to connect Slack for this workspace."}
                </p>
                {isAdmin && (
                  <Button onClick={handleConnect} disabled={connecting}>
                    {connecting ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <MessageSquare className="h-4 w-4 mr-2" />}
                    Connect Slack
                  </Button>
                )}
              </div>
            ) : (
              <div className="space-y-4">
                <div className="flex items-center gap-2 text-sm">
                  <Check className="h-4 w-4 text-green-600" />
                  Connected to <span className="font-medium">{status.integration?.workspace_name || "Slack"}</span>
                </div>

                <div className="grid sm:grid-cols-3 gap-3">
                  <div className="space-y-1.5">
                    <Label className="text-xs">Alerts channel</Label>
                    {channelSelect(alertCh, setAlertCh)}
                  </div>
                  <div className="space-y-1.5">
                    <Label className="text-xs">Deploy notifications</Label>
                    {channelSelect(deployCh, setDeployCh)}
                  </div>
                  <div className="space-y-1.5">
                    <Label className="text-xs">Daily summary</Label>
                    {channelSelect(summaryCh, setSummaryCh)}
                  </div>
                </div>

                {isAdmin && (
                  <div className="flex items-center justify-between pt-1">
                    <Button variant="outline" className="text-red-600 hover:text-red-700" onClick={handleDisconnect}>
                      Disconnect
                    </Button>
                    <Button onClick={handleSaveChannels} disabled={saving}>
                      {saving ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : null}
                      Save channels
                    </Button>
                  </div>
                )}
              </div>
            )}
          </CardContent>
        </Card>
      </main>
    </div>
  );
}
