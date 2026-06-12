"use client";

import { useCallback } from "react";
import useSWR from "swr";
import { useAuth } from "@clerk/nextjs";
import { listMyOrgs, getActiveOrgId, setActiveOrgId } from "@/lib/api";
import type { Organization, OrgRole } from "@/types/api";

// useActiveOrg loads the user's workspaces and resolves the active one (from the
// X-Org-Id selection in localStorage, defaulting to the first/personal org). It
// exposes the current role so UI can gate actions, and a switcher that persists the
// choice and reloads so every SWR cache re-fetches under the new workspace.
export function useActiveOrg() {
  const { getToken } = useAuth();

  const { data: orgs, isLoading, mutate } = useSWR<Organization[]>(
    "orgs-me",
    async () => {
      const token = await getToken();
      if (!token) return [];
      return listMyOrgs(token);
    }
  );

  const list = orgs ?? [];
  const stored = getActiveOrgId();
  const active = list.find((o) => o.id === stored) ?? list[0] ?? null;
  const role: OrgRole | null = active?.role ?? null;

  const switchOrg = useCallback((orgId: string) => {
    setActiveOrgId(orgId);
    // A full reload is the simplest correct way to re-scope every SWR cache key
    // (projects, aws accounts, etc.) to the newly selected workspace.
    window.location.reload();
  }, []);

  return {
    orgs: list,
    activeOrg: active,
    role,
    isViewer: role === "viewer",
    isAdmin: role === "admin",
    canAct: role === "engineer" || role === "admin",
    isLoading,
    switchOrg,
    refreshOrgs: mutate,
  };
}
