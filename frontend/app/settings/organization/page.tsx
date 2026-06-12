"use client";

import { useState } from "react";
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
import type { OrganizationMember, OrgRole } from "@/types/api";
import { toast } from "sonner";
import { Building2, Loader2, Trash2, UserPlus } from "lucide-react";
import { cn } from "@/lib/utils";

const ROLES: OrgRole[] = ["admin", "engineer", "viewer"];

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
          </>
        )}
      </main>
    </div>
  );
}
