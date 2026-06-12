"use client";

import { useState } from "react";
import useSWR from "swr";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { ConnectAWSModal } from "@/components/project/connect-aws-modal";
import { listAWSAccounts, deleteAWSAccount, scanAccount } from "@/lib/api";
import Link from "next/link";
import { toast } from "sonner";
import { Plus, Trash2, Cloud, Loader2, RefreshCw, Boxes } from "lucide-react";

function timeAgo(iso: string | null): string {
  if (!iso) return "never scanned";
  const s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return "scanned just now";
  if (s < 3600) return `scanned ${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `scanned ${Math.floor(s / 3600)}h ago`;
  return `scanned ${Math.floor(s / 86400)}d ago`;
}

export default function AWSAccountsPage() {
  const { getToken } = useAuth();
  const [connectOpen, setConnectOpen] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [scanning, setScanning] = useState<string | null>(null);

  const {
    data: accounts = [],
    isLoading: loading,
    mutate,
  } = useSWR("aws-accounts", async () => {
    const token = await getToken();
    if (!token) return [];
    return (await listAWSAccounts(token)) ?? [];
  }, {
    onError: () => toast.error("Failed to load AWS accounts"),
  });

  async function handleScan(id: string) {
    const token = await getToken();
    if (!token) return;
    setScanning(id);
    try {
      await scanAccount(token, id);
      toast.success("Scan started — discovered resources will appear shortly.");
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to start scan");
    } finally {
      setScanning(null);
    }
  }

  async function handleDelete(id: string) {
    const account = accounts.find((a) => a.id === id);
    if (!window.confirm(`Remove AWS account "${account?.label ?? id}"? Projects using it will no longer be able to deploy.`)) {
      return;
    }
    const token = await getToken();
    if (!token) return;
    setDeleting(id);
    try {
      await deleteAWSAccount(token, id);
      void mutate((prev) => (prev ?? []).filter((a) => a.id !== id), { revalidate: false });
      toast.success("AWS account removed");
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to remove account");
    } finally {
      setDeleting(null);
    }
  }

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />

      <ConnectAWSModal
        open={connectOpen}
        onClose={() => setConnectOpen(false)}
        onConnected={(account) => {
          void mutate((prev) => [account, ...(prev ?? [])], { revalidate: false });
          setConnectOpen(false);
        }}
      />

      <main className="max-w-4xl mx-auto px-4 py-10">
        <div className="flex items-center justify-between mb-8">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">AWS Accounts</h1>
            <p className="text-muted-foreground text-sm mt-1">
              Connect your AWS accounts once — use them across all projects.
            </p>
          </div>
          <Button onClick={() => setConnectOpen(true)}>
            <Plus className="h-4 w-4 mr-2" />
            Connect Account
          </Button>
        </div>

        {loading ? (
          <div className="flex items-center justify-center py-24">
            <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
          </div>
        ) : accounts.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-24 text-center">
            <Cloud className="h-12 w-12 text-zinc-300 mb-4" />
            <h2 className="text-lg font-semibold">No AWS accounts connected</h2>
            <p className="text-muted-foreground text-sm mt-1 mb-6">
              Connect an AWS account to start deploying your projects.
            </p>
            <Button onClick={() => setConnectOpen(true)}>
              <Plus className="h-4 w-4 mr-2" />
              Connect your first account
            </Button>
          </div>
        ) : (
          <div className="space-y-3">
            {accounts.map((account) => (
              <Card key={account.id}>
                <CardHeader className="pb-3">
                  <div className="flex items-start justify-between">
                    <div className="flex items-center gap-3">
                      <Cloud className="h-5 w-5 text-indigo-500 shrink-0" />
                      <div>
                        <CardTitle className="text-base">{account.label}</CardTitle>
                        <CardDescription className="text-xs mt-0.5">
                          Account ID: <span className="font-mono">{account.aws_account_id}</span>
                        </CardDescription>
                      </div>
                    </div>
                    <div className="flex items-center gap-2">
                      <Badge variant="secondary" className="text-xs">Connected</Badge>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-7 w-7 p-0 text-muted-foreground hover:text-red-600"
                        disabled={deleting === account.id}
                        onClick={() => handleDelete(account.id)}
                      >
                        {deleting === account.id
                          ? <Loader2 className="h-3.5 w-3.5 animate-spin" />
                          : <Trash2 className="h-3.5 w-3.5" />
                        }
                      </Button>
                    </div>
                  </div>
                </CardHeader>
                <CardContent className="pt-0">
                  <p className="text-xs text-muted-foreground font-mono truncate">
                    {account.iam_role_arn}
                  </p>
                  <div className="mt-3 flex items-center justify-between gap-2 flex-wrap">
                    <div className="flex items-center gap-3 text-xs text-muted-foreground">
                      <Link href="/orgs/resources" className="inline-flex items-center gap-1 hover:text-foreground">
                        <Boxes className="h-3.5 w-3.5" />
                        {account.resource_count ?? 0} resource{(account.resource_count ?? 0) === 1 ? "" : "s"}
                      </Link>
                      <span>·</span>
                      <span>{timeAgo(account.last_scanned_at)}</span>
                    </div>
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 text-xs"
                      disabled={scanning === account.id}
                      onClick={() => handleScan(account.id)}
                    >
                      {scanning === account.id
                        ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" />
                        : <RefreshCw className="h-3.5 w-3.5 mr-1" />}
                      Scan now
                    </Button>
                  </div>
                </CardContent>
              </Card>
            ))}
          </div>
        )}
      </main>
    </div>
  );
}
