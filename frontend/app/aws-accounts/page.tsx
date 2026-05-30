"use client";

import { useEffect, useState } from "react";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { ConnectAWSModal } from "@/components/project/connect-aws-modal";
import { listAWSAccounts, deleteAWSAccount } from "@/lib/api";
import type { AWSAccount } from "@/types/api";
import { toast } from "sonner";
import { Plus, Trash2, Cloud, Loader2 } from "lucide-react";

export default function AWSAccountsPage() {
  const { getToken } = useAuth();
  const [accounts, setAccounts] = useState<AWSAccount[]>([]);
  const [loading, setLoading] = useState(true);
  const [connectOpen, setConnectOpen] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);

  async function load() {
    const token = await getToken();
    if (!token) return;
    try {
      const data = await listAWSAccounts(token);
      setAccounts(data ?? []);
    } catch {
      toast.error("Failed to load AWS accounts");
    }
  }

  useEffect(() => {
    load().finally(() => setLoading(false));
  }, []);

  async function handleDelete(id: string) {
    const token = await getToken();
    if (!token) return;
    setDeleting(id);
    try {
      await deleteAWSAccount(token, id);
      setAccounts((prev) => prev.filter((a) => a.id !== id));
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
          setAccounts((prev) => [account, ...prev]);
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
                </CardContent>
              </Card>
            ))}
          </div>
        )}
      </main>
    </div>
  );
}
