"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { acceptInvite, setActiveOrgId } from "@/lib/api";
import type { Organization } from "@/types/api";
import { Loader2, CheckCircle2, XCircle, Rocket } from "lucide-react";

export default function AcceptInvitePage() {
  const { token } = useParams<{ token: string }>();
  const { getToken, isLoaded, isSignedIn } = useAuth();
  const router = useRouter();

  const [status, setStatus] = useState<"loading" | "ok" | "error">("loading");
  const [message, setMessage] = useState("");
  const [org, setOrg] = useState<Organization | null>(null);
  const ran = useRef(false);

  useEffect(() => {
    if (!isLoaded || ran.current) return;
    // Not signed in → bounce to sign-in, returning here afterwards.
    if (!isSignedIn) {
      router.push(`/sign-in?redirect_url=/invites/${token}`);
      return;
    }
    ran.current = true;
    (async () => {
      const t = await getToken();
      if (!t) {
        setStatus("error");
        setMessage("Could not authenticate. Please sign in and try again.");
        return;
      }
      try {
        const res = await acceptInvite(t, token);
        setOrg(res.organization);
        // Switch the active workspace to the one just joined.
        if (res.organization?.id) setActiveOrgId(res.organization.id);
        setStatus("ok");
        setMessage(res.message ?? "You've joined the workspace.");
      } catch (e: unknown) {
        setStatus("error");
        setMessage((e as Error).message ?? "This invite could not be accepted.");
      }
    })();
  }, [isLoaded, isSignedIn, getToken, token, router]);

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-md mx-auto px-4 py-20">
        <div className="rounded-lg border bg-white p-8 text-center">
          {status === "loading" && (
            <>
              <Loader2 className="h-8 w-8 mx-auto mb-4 animate-spin text-muted-foreground" />
              <p className="text-sm text-muted-foreground">Accepting your invitation…</p>
            </>
          )}
          {status === "ok" && (
            <>
              <CheckCircle2 className="h-10 w-10 mx-auto mb-4 text-green-500" />
              <h1 className="text-lg font-semibold">Welcome aboard</h1>
              <p className="text-sm text-muted-foreground mt-1 mb-6">
                {org ? <>You&apos;ve joined <span className="font-medium">{org.name}</span>{org.role ? <> as <span className="capitalize">{org.role}</span></> : null}.</> : message}
              </p>
              <Button nativeButton={false} render={<Link href="/projects" />}>
                <Rocket className="h-4 w-4 mr-2" /> Go to projects
              </Button>
            </>
          )}
          {status === "error" && (
            <>
              <XCircle className="h-10 w-10 mx-auto mb-4 text-red-500" />
              <h1 className="text-lg font-semibold">Invitation problem</h1>
              <p className="text-sm text-muted-foreground mt-1 mb-6">{message}</p>
              <Button variant="outline" nativeButton={false} render={<Link href="/projects" />}>
                Go to projects
              </Button>
            </>
          )}
        </div>
      </main>
    </div>
  );
}
