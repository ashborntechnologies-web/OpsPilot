"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import useSWR from "swr";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import {
  listOrgMembers, createOrgInvite, updateMemberRole, removeMember,
} from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import { updateSummaryConfig, generateSummaryNow, getOncallSchedule, putOncallSchedule } from "@/lib/api";
import type { OrganizationMember, OrgRole } from "@/types/api";
import { toast } from "sonner";
import { Building2, Loader2, Trash2, UserPlus, MoonStar } from "lucide-react";
import { cn } from "@/lib/utils";

const ROLES: OrgRole[] = ["admin", "engineer", "viewer"];

const WEEKDAYS = ["monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"];

// A curated set of common IANA timezones for the daily-summary picker (any stored value
// not in this list is shown as an extra option so it's never lost).
const TIMEZONES = [
  "UTC", "America/Los_Angeles", "America/Denver", "America/Chicago", "America/New_York",
  "America/Sao_Paulo", "Europe/London", "Europe/Paris", "Europe/Berlin", "Asia/Kolkata",
  "Asia/Singapore", "Asia/Tokyo", "Australia/Sydney",
];

const ROLE_BADGE: Record<OrgRole, string> = {
  admin: "bg-indigo-100 text-indigo-700",
  engineer: "bg-emerald-100 text-emerald-700",
  viewer: "bg-zinc-100 text-zinc-600",
};

const ROLE_HINT: Record<OrgRole, string> = {
  admin: "Full access — manage members, AWS accounts, delete projects.",
  engineer: "Deploy, roll back, scale, env vars, terminal, resolve alerts.",
  viewer: "Read-only — cannot trigger any action.",
};

export default function OrganizationSettingsPage() {
  const { getToken } = useAuth();
  const { activeOrg, isAdmin, isLoading: orgLoading } = useActiveOrg();
  const orgId = activeOrg?.id;

  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState<OrgRole>("engineer");
  const [inviting, setInviting] = useState(false);
  const [busyUser, setBusyUser] = useState<string | null>(null);

  // Daily-summary config (seeded from the active org once loaded).
  const [sumEnabled, setSumEnabled] = useState(true);
  const [sumHour, setSumHour] = useState("08");
  const [sumTz, setSumTz] = useState("UTC");
  const [savingSummary, setSavingSummary] = useState(false);
  const [testingSummary, setTestingSummary] = useState(false);

  // On-call quiet-hours config.
  const [ocTz, setOcTz] = useState("UTC");
  const [ocStart, setOcStart] = useState("22");
  const [ocEnd, setOcEnd] = useState("08");
  const [ocDays, setOcDays] = useState<string[]>([]);
  const [savingOncall, setSavingOncall] = useState(false);

  useEffect(() => {
    if (!activeOrg) return;
    setSumEnabled(activeOrg.summary_enabled ?? true);
    setSumHour((activeOrg.summary_time ?? "08:00").slice(0, 2));
    setSumTz(activeOrg.summary_timezone ?? "UTC");
  }, [activeOrg]);

  useEffect(() => {
    if (!orgId) return;
    (async () => {
      const token = await getToken();
      if (!token) return;
      try {
        const oc = await getOncallSchedule(token, orgId);
        setOcTz(oc.timezone || "UTC");
        setOcStart((oc.quiet_hours_start || "22:00").slice(0, 2));
        setOcEnd((oc.quiet_hours_end || "08:00").slice(0, 2));
        setOcDays(oc.quiet_days ?? []);
      } catch { /* defaults */ }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [orgId]);

  async function handleSaveOncall() {
    if (!orgId) return;
    const token = await getToken();
    if (!token) return;
    setSavingOncall(true);
    try {
      await putOncallSchedule(token, orgId, {
        timezone: ocTz,
        quiet_hours_start: `${ocStart}:00`,
        quiet_hours_end: `${ocEnd}:00`,
        quiet_days: ocDays,
      });
      toast.success("On-call schedule saved");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setSavingOncall(false);
    }
  }

  async function handleSaveSummary() {
    if (!orgId) return;
    const token = await getToken();
    if (!token) return;
    setSavingSummary(true);
    try {
      await updateSummaryConfig(token, orgId, {
        summary_enabled: sumEnabled,
        summary_time: `${sumHour}:00:00`,
        summary_timezone: sumTz,
      });
      toast.success("Daily summary settings saved");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setSavingSummary(false);
    }
  }

  async function handleTestSummary() {
    if (!orgId) return;
    const token = await getToken();
    if (!token) return;
    setTestingSummary(true);
    try {
      await generateSummaryNow(token, orgId);
      toast.success("Test summary generated and delivered");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setTestingSummary(false);
    }
  }

  const { data: members, mutate } = useSWR<OrganizationMember[]>(
    orgId ? ["org-members", orgId] : null,
    async () => {
      const token = await getToken();
      if (!token || !orgId) return [];
      return listOrgMembers(token, orgId);
    }
  );

  async function handleInvite() {
    if (!orgId || !inviteEmail.trim()) return;
    const token = await getToken();
    if (!token) return;
    setInviting(true);
    try {
      const res = await createOrgInvite(token, orgId, { email: inviteEmail.trim(), role: inviteRole });
      toast.success(
        res.email_sent
          ? `Invite sent to ${inviteEmail.trim()}`
          : `Invite created — share this link: ${res.accept_url}`
      );
      setInviteEmail("");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setInviting(false);
    }
  }

  async function handleRoleChange(m: OrganizationMember, role: OrgRole) {
    if (!orgId || role === m.role) return;
    const token = await getToken();
    if (!token) return;
    setBusyUser(m.user_id);
    try {
      await updateMemberRole(token, orgId, m.user_id, role);
      toast.success(`${m.email} is now ${role}`);
      mutate();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setBusyUser(null);
    }
  }

  async function handleRemove(m: OrganizationMember) {
    if (!orgId) return;
    if (!window.confirm(`Remove ${m.email} from ${activeOrg?.name}?`)) return;
    const token = await getToken();
    if (!token) return;
    setBusyUser(m.user_id);
    try {
      await removeMember(token, orgId, m.user_id);
      toast.success(`${m.email} removed`);
      mutate();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setBusyUser(null);
    }
  }

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-3xl mx-auto px-4 py-10 space-y-6">
        <div className="flex items-center gap-2">
          <Building2 className="h-5 w-5 text-indigo-600" />
          <h1 className="text-2xl font-bold tracking-tight">{activeOrg?.name ?? "Workspace"}</h1>
        </div>

        {/* Settings sub-navigation */}
        <div className="flex gap-1">
          <span className="px-3 py-1.5 text-sm rounded-md bg-zinc-900 text-white">Members</span>
          <Link href="/settings/integrations" className="px-3 py-1.5 text-sm rounded-md hover:bg-zinc-100">
            Integrations
          </Link>
        </div>

        {orgLoading ? (
          <div className="flex items-center justify-center h-40">
            <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
          </div>
        ) : (
          <>
            {/* Invite (admin only) */}
            {isAdmin && (
              <Card>
                <CardHeader className="pb-3">
                  <CardTitle className="text-base flex items-center gap-2">
                    <UserPlus className="h-4 w-4" /> Invite a member
                  </CardTitle>
                  <CardDescription>They&apos;ll get an email with a link to join. Invites expire in 7 days.</CardDescription>
                </CardHeader>
                <CardContent>
                  <div className="flex flex-col sm:flex-row gap-2 sm:items-end">
                    <div className="flex-1 space-y-1.5">
                      <Label className="text-xs">Email</Label>
                      <Input
                        type="email"
                        placeholder="teammate@company.com"
                        value={inviteEmail}
                        onChange={(e) => setInviteEmail(e.target.value)}
                      />
                    </div>
                    <div className="space-y-1.5">
                      <Label className="text-xs">Role</Label>
                      <select
                        value={inviteRole}
                        onChange={(e) => setInviteRole(e.target.value as OrgRole)}
                        className="w-full sm:w-36 h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
                      >
                        {ROLES.map((r) => <option key={r} value={r} className="capitalize">{r}</option>)}
                      </select>
                    </div>
                    <Button onClick={handleInvite} disabled={inviting || !inviteEmail.trim()}>
                      {inviting ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
                      Invite
                    </Button>
                  </div>
                  <p className="text-xs text-muted-foreground mt-2">{ROLE_HINT[inviteRole]}</p>
                </CardContent>
              </Card>
            )}

            {/* Members */}
            <Card>
              <CardHeader className="pb-3">
                <CardTitle className="text-base">Members</CardTitle>
                <CardDescription>
                  {isAdmin ? "Change a role or remove a member." : "You have read-only access to workspace settings."}
                </CardDescription>
              </CardHeader>
              <CardContent className="divide-y">
                {(members ?? []).map((m) => (
                  <div key={m.id} className="flex items-center justify-between gap-3 py-2.5">
                    <div className="min-w-0">
                      <p className="text-sm font-medium truncate">{m.email}</p>
                      <p className="text-xs text-muted-foreground">
                        Joined {new Date(m.joined_at).toLocaleDateString()}
                      </p>
                    </div>
                    <div className="flex items-center gap-2 shrink-0">
                      {isAdmin ? (
                        <select
                          value={m.role}
                          disabled={busyUser === m.user_id}
                          onChange={(e) => handleRoleChange(m, e.target.value as OrgRole)}
                          className="h-8 rounded-md border border-input bg-background px-2 text-xs capitalize outline-none focus:ring-2 focus:ring-ring"
                        >
                          {ROLES.map((r) => <option key={r} value={r} className="capitalize">{r}</option>)}
                        </select>
                      ) : (
                        <span className={cn("rounded px-2 py-0.5 text-xs font-medium capitalize", ROLE_BADGE[m.role])}>
                          {m.role}
                        </span>
                      )}
                      {isAdmin && (
                        <button
                          onClick={() => handleRemove(m)}
                          disabled={busyUser === m.user_id}
                          className="p-1 rounded text-zinc-400 hover:text-red-500 hover:bg-red-50 disabled:opacity-50"
                          title="Remove member"
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                        </button>
                      )}
                    </div>
                  </div>
                ))}
                {members && members.length === 0 && (
                  <p className="py-4 text-sm text-muted-foreground">No members yet.</p>
                )}
              </CardContent>
            </Card>

            {/* Daily summary */}
            <Card>
              <CardHeader className="pb-3">
                <CardTitle className="text-base">Daily summary</CardTitle>
                <CardDescription>An AI morning briefing of the last 24 hours, posted to Slack and emailed to admins &amp; engineers.</CardDescription>
              </CardHeader>
              <CardContent className="space-y-4">
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={sumEnabled}
                    disabled={!isAdmin}
                    onChange={(e) => setSumEnabled(e.target.checked)}
                    className="h-3.5 w-3.5"
                  />
                  Enable daily summary
                </label>

                <div className="flex flex-col sm:flex-row gap-3 sm:items-end">
                  <div className="space-y-1.5">
                    <Label className="text-xs">Delivery time (on the hour)</Label>
                    <select
                      value={sumHour}
                      disabled={!isAdmin}
                      onChange={(e) => setSumHour(e.target.value)}
                      className="h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring disabled:opacity-60"
                    >
                      {Array.from({ length: 24 }, (_, h) => String(h).padStart(2, "0")).map((h) => (
                        <option key={h} value={h}>{h}:00</option>
                      ))}
                    </select>
                  </div>
                  <div className="flex-1 space-y-1.5">
                    <Label className="text-xs">Timezone</Label>
                    <select
                      value={sumTz}
                      disabled={!isAdmin}
                      onChange={(e) => setSumTz(e.target.value)}
                      className="w-full h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring disabled:opacity-60"
                    >
                      {TIMEZONES.includes(sumTz) ? null : <option value={sumTz}>{sumTz}</option>}
                      {TIMEZONES.map((tz) => <option key={tz} value={tz}>{tz}</option>)}
                    </select>
                  </div>
                </div>

                {isAdmin && (
                  <div className="flex items-center justify-between pt-1">
                    <Button variant="outline" onClick={handleTestSummary} disabled={testingSummary}>
                      {testingSummary ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : null}
                      Send test summary now
                    </Button>
                    <Button onClick={handleSaveSummary} disabled={savingSummary}>
                      {savingSummary ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : null}
                      Save
                    </Button>
                  </div>
                )}
              </CardContent>
            </Card>

            {/* On-call schedule — quiet hours */}
            <Card>
              <CardHeader className="pb-3">
                <CardTitle className="text-base flex items-center gap-2">
                  <MoonStar className="h-4 w-4 text-indigo-600" /> On-Call Schedule
                </CardTitle>
                <CardDescription>
                  Warn-level alerts are suppressed during quiet hours. Error-level alerts always notify.
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-4">
                <div className="flex flex-col sm:flex-row gap-3 sm:items-end">
                  <div className="flex-1 space-y-1.5">
                    <Label className="text-xs">Timezone</Label>
                    <select
                      value={ocTz}
                      disabled={!isAdmin}
                      onChange={(e) => setOcTz(e.target.value)}
                      className="w-full h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring disabled:opacity-60"
                    >
                      {TIMEZONES.includes(ocTz) ? null : <option value={ocTz}>{ocTz}</option>}
                      {TIMEZONES.map((tz) => <option key={tz} value={tz}>{tz}</option>)}
                    </select>
                  </div>
                  <div className="space-y-1.5">
                    <Label className="text-xs">Quiet hours start</Label>
                    <select
                      value={ocStart}
                      disabled={!isAdmin}
                      onChange={(e) => setOcStart(e.target.value)}
                      className="h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring disabled:opacity-60"
                    >
                      {Array.from({ length: 24 }, (_, h) => String(h).padStart(2, "0")).map((h) => (
                        <option key={h} value={h}>{h}:00</option>
                      ))}
                    </select>
                  </div>
                  <div className="space-y-1.5">
                    <Label className="text-xs">Quiet hours end</Label>
                    <select
                      value={ocEnd}
                      disabled={!isAdmin}
                      onChange={(e) => setOcEnd(e.target.value)}
                      className="h-9 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring disabled:opacity-60"
                    >
                      {Array.from({ length: 24 }, (_, h) => String(h).padStart(2, "0")).map((h) => (
                        <option key={h} value={h}>{h}:00</option>
                      ))}
                    </select>
                  </div>
                </div>

                <div className="space-y-1.5">
                  <Label className="text-xs">Quiet days (all-day quiet)</Label>
                  <div className="flex flex-wrap gap-1.5">
                    {WEEKDAYS.map((d) => {
                      const on = ocDays.includes(d);
                      return (
                        <button
                          key={d}
                          type="button"
                          disabled={!isAdmin}
                          onClick={() => setOcDays((prev) => on ? prev.filter((x) => x !== d) : [...prev, d])}
                          className={cn(
                            "rounded-md border px-2.5 py-1 text-xs capitalize disabled:opacity-60",
                            on ? "bg-indigo-600 text-white border-indigo-600" : "hover:bg-zinc-50"
                          )}
                        >
                          {d.slice(0, 3)}
                        </button>
                      );
                    })}
                  </div>
                </div>

                {isAdmin && (
                  <div className="flex justify-end pt-1">
                    <Button onClick={handleSaveOncall} disabled={savingOncall}>
                      {savingOncall ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : null}
                      Save
                    </Button>
                  </div>
                )}
              </CardContent>
            </Card>
          </>
        )}
      </main>
    </div>
  );
}
