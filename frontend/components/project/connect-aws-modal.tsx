"use client";

import { useState } from "react";
import { useAuth } from "@clerk/nextjs";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { connectAWSAccount, getBootstrapTemplate } from "@/lib/api";
import type { AWSAccount } from "@/types/api";
import { toast } from "sonner";
import {
  Copy, Check, ExternalLink, ArrowRight, Terminal,
  CircleCheck, Circle,
} from "lucide-react";
import { cn } from "@/lib/utils";

const AWS_REGIONS = [
  "us-east-1", "us-east-2", "us-west-1", "us-west-2",
  "eu-west-1", "eu-central-1", "ap-southeast-1", "ap-northeast-1",
];

interface Props {
  open: boolean;
  onClose: () => void;
  onConnected: (account: AWSAccount) => void;
}

type Step = 1 | 2 | 3;

function StepIndicator({ current, step, label }: { current: Step; step: Step; label: string }) {
  const done = current > step;
  const active = current === step;
  return (
    <div className={cn("flex items-center gap-2 text-sm", active ? "text-foreground font-medium" : done ? "text-muted-foreground" : "text-muted-foreground/50")}>
      {done
        ? <CircleCheck className="h-4 w-4 text-green-500 shrink-0" />
        : <Circle className={cn("h-4 w-4 shrink-0", active ? "text-indigo-600" : "text-muted-foreground/40")} />
      }
      {label}
    </div>
  );
}

export function ConnectAWSModal({ open, onClose, onConnected }: Props) {
  const { getToken } = useAuth();

  const [step, setStep] = useState<Step>(1);
  const [label, setLabel] = useState("");
  const [accountId, setAccountId] = useState("");
  const [region, setRegion] = useState("us-east-1");
  const [script, setScript] = useState("");
  const [externalId, setExternalId] = useState("");
  const [roleArn, setRoleArn] = useState("");
  const [copied, setCopied] = useState(false);
  const [loadingScript, setLoadingScript] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  function handleClose() {
    setStep(1);
    setLabel("");
    setAccountId("");
    setRegion("us-east-1");
    setScript("");
    setExternalId("");
    setRoleArn("");
    onClose();
  }

  async function fetchScript() {
    setLoadingScript(true);
    try {
      const data = await getBootstrapTemplate(region);
      if (data.error) {
        toast.error(data.error);
        return;
      }
      setScript(data.script);
      // Capture the per-connection external ID embedded in this script so we can send it
      // back on connect — it must match the trust policy the user is about to deploy.
      setExternalId(data.external_id ?? "");
      setStep(2);
    } catch {
      toast.error("Failed to load setup script — is the backend running?");
    } finally {
      setLoadingScript(false);
    }
  }

  async function handleCopy() {
    await navigator.clipboard.writeText(script);
    setCopied(true);
    setTimeout(() => setCopied(false), 2500);
  }

  async function handleConnect() {
    if (!roleArn.trim() || !accountId.trim() || !label.trim()) return;
    const token = await getToken();
    if (!token) return;
    setSubmitting(true);
    try {
      const account = await connectAWSAccount(token, {
        label: label.trim(),
        aws_account_id: accountId.trim(),
        iam_role_arn: roleArn.trim(),
        external_id: externalId || undefined,
      });
      toast.success("AWS account connected!");
      onConnected(account);
      handleClose();
    } catch (e: unknown) {
      toast.error((e as Error).message ?? "Failed to connect");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(v) => !v && handleClose()}>
      <DialogContent className="max-w-xl flex flex-col gap-0 p-0 max-h-[90vh]">
        {/* Fixed header — never scrolls away */}
        <div className="px-6 pt-6 pb-4 border-b shrink-0">
          <DialogHeader className="mb-3">
            <DialogTitle>Connect AWS Account</DialogTitle>
          </DialogHeader>
          {/* Step tracker */}
          <div className="flex items-center gap-3">
            <StepIndicator current={step} step={1} label="Your details" />
            <div className="h-px flex-1 bg-border" />
            <StepIndicator current={step} step={2} label="Run setup" />
            <div className="h-px flex-1 bg-border" />
            <StepIndicator current={step} step={3} label="Paste ARN" />
          </div>
        </div>

        {/* Scrollable body */}
        <div className="overflow-y-auto flex-1 px-6 py-4">

        {/* Step 1: AWS details */}
        {step === 1 && (
          <div className="space-y-5 pt-1">
            <div className="space-y-1">
              <h3 className="font-medium text-sm">Enter your AWS details</h3>
              <p className="text-xs text-muted-foreground">
                ConvDeploy will create a secure access role in your account — we never store your credentials.
              </p>
            </div>

            <div className="space-y-3">
              <div className="space-y-1.5">
                <Label className="text-xs">Account label</Label>
                <Input
                  placeholder="Production AWS"
                  value={label}
                  onChange={(e) => setLabel(e.target.value)}
                />
                <p className="text-xs text-muted-foreground">A friendly name to identify this account.</p>
              </div>

              <div className="space-y-1.5">
                <Label className="text-xs">AWS Account ID</Label>
                <Input
                  placeholder="123456789012"
                  value={accountId}
                  onChange={(e) => setAccountId(e.target.value.replace(/\D/g, "").slice(0, 12))}
                  className="font-mono"
                />
                <p className="text-xs text-muted-foreground">
                  12 digits — find it in the top-right corner of your{" "}
                  <a
                    href="https://console.aws.amazon.com"
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-indigo-600 hover:underline"
                  >
                    AWS Console
                  </a>
                </p>
              </div>

              <div className="space-y-1.5">
                <Label className="text-xs">AWS Region (for bootstrap stack)</Label>
                <select
                  value={region}
                  onChange={(e) => setRegion(e.target.value)}
                  className="w-full h-8 rounded-md border border-input bg-background px-3 text-sm outline-none focus:ring-2 focus:ring-ring"
                >
                  {AWS_REGIONS.map((r) => (
                    <option key={r} value={r}>{r}</option>
                  ))}
                </select>
              </div>
            </div>

            <Button
              className="w-full"
              disabled={accountId.length !== 12 || !label.trim() || loadingScript}
              onClick={fetchScript}
            >
              {loadingScript ? "Loading..." : "Continue"}
              <ArrowRight className="h-3.5 w-3.5 ml-1.5" />
            </Button>
          </div>
        )}

        {/* Step 2: CloudShell setup */}
        {step === 2 && (
          <div className="space-y-4 pt-1">
            <div className="space-y-1">
              <h3 className="font-medium text-sm">Run this in AWS CloudShell</h3>
              <p className="text-xs text-muted-foreground">
                CloudShell is a browser-based terminal built into the AWS console. Copy the script, open CloudShell, paste and press Enter.
              </p>
            </div>

            <div className="relative rounded-lg border bg-zinc-950 overflow-hidden">
              <div className="flex items-center justify-between px-3 py-2 border-b border-zinc-800">
                <div className="flex items-center gap-1.5 text-zinc-400 text-xs">
                  <Terminal className="h-3 w-3" />
                  <span>bash</span>
                </div>
                <Button
                  size="sm"
                  variant="ghost"
                  className="h-6 px-2 text-xs text-zinc-300 hover:text-white hover:bg-zinc-800"
                  onClick={handleCopy}
                >
                  {copied ? <Check className="h-3 w-3 mr-1 text-green-400" /> : <Copy className="h-3 w-3 mr-1" />}
                  {copied ? "Copied!" : "Copy"}
                </Button>
              </div>
              <pre className="text-xs text-zinc-300 p-3 max-h-56 overflow-y-auto leading-relaxed font-mono whitespace-pre-wrap">
                {script}
              </pre>
            </div>

            <Button
              variant="outline"
              className="w-full"
              nativeButton={false}
              render={
                <a
                  href={`https://${region}.console.aws.amazon.com/cloudshell/home?region=${region}`}
                  target="_blank"
                  rel="noopener noreferrer"
                />
              }
            >
              <ExternalLink className="h-3.5 w-3.5 mr-2" />
              Open AWS CloudShell
            </Button>

            <div className="rounded-lg bg-amber-50 border border-amber-200 px-3 py-2 text-xs text-amber-800 space-y-0.5">
              <p className="font-medium">After running the script:</p>
              <p>The last line printed is your <strong>Role ARN</strong> — copy it and click Continue.</p>
            </div>

            <Button className="w-full" onClick={() => setStep(3)}>
              I ran the script — Continue
              <ArrowRight className="h-3.5 w-3.5 ml-1.5" />
            </Button>
          </div>
        )}

        {/* Step 3: Paste ARN */}
        {step === 3 && (
          <div className="space-y-4 pt-1">
            <div className="space-y-1">
              <h3 className="font-medium text-sm">Paste your Role ARN</h3>
              <p className="text-xs text-muted-foreground">
                Copy the last line printed by the CloudShell script and paste it below.
              </p>
            </div>

            <div className="space-y-1.5">
              <Label className="text-xs">Role ARN</Label>
              <Input
                placeholder="arn:aws:iam::123456789012:role/ConvDeployPlatformRole"
                value={roleArn}
                onChange={(e) => setRoleArn(e.target.value)}
                className="font-mono text-xs"
              />
            </div>

            <div className="rounded-lg bg-zinc-50 border px-3 py-2 text-xs text-muted-foreground space-y-0.5">
              <p className="font-medium text-foreground">What happens next</p>
              <p>ConvDeploy will verify the role and save this account. You can then link it to projects and environments will provision automatically.</p>
            </div>

            <div className="flex gap-2">
              <Button variant="outline" onClick={() => setStep(2)} className="flex-none">
                Back
              </Button>
              <Button
                className="flex-1"
                disabled={!roleArn.trim() || submitting}
                onClick={handleConnect}
              >
                {submitting ? "Connecting..." : "Save AWS Account"}
              </Button>
            </div>
          </div>
        )}
        </div>{/* end scrollable body */}
      </DialogContent>
    </Dialog>
  );
}
