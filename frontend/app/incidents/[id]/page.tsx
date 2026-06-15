"use client";

import { useEffect, useRef, useState, useCallback } from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter,
} from "@/components/ui/dialog";
import {
  getIncident, postIncidentTimeline, acknowledgeIncident, resolveIncident,
  savePostmortem, approveIncidentAction, rejectIncidentAction, incidentWsURL,
} from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import { Markdown } from "@/lib/markdown";
import { ConfidenceBadge, EvidenceSection } from "@/components/ai/explainability";
import { SEVERITY_BADGE, STATUS_BADGE, timeOpen } from "@/lib/incidents";
import type { IncidentDetail, IncidentTimelineEntry, IncidentAction, WsMessage } from "@/types/api";
import { cn } from "@/lib/utils";
import { toast } from "sonner";
import {
  ArrowLeft, Loader2, Send, CheckCircle2, Bot, Clock, Siren, Server, FileText,
} from "lucide-react";

export default function WarRoomPage() {
  const { id } = useParams<{ id: string }>();
  const { getToken } = useAuth();
  const { canAct } = useActiveOrg();

  const [detail, setDetail] = useState<IncidentDetail | null>(null);
  const [entries, setEntries] = useState<IncidentTimelineEntry[]>([]);
  const [actions, setActions] = useState<IncidentAction[]>([]);
  const [input, setInput] = useState("");
  const [posting, setPosting] = useState(false);
  const [busy, setBusy] = useState<string | null>(null); // ack | resolve | action id
  const [, setTick] = useState(0); // drives the live time-open counter

  // postmortem modal
  const [pmOpen, setPmOpen] = useState(false);
  const [pmText, setPmText] = useState("");
  const [publishing, setPublishing] = useState(false);

  const bottomRef = useRef<HTMLDivElement>(null);
  const tokenRef = useRef<string | null>(null);

  const incident = detail?.incident;

  const reload = useCallback(async () => {
    const token = await getToken();
    if (!token) return;
    const d = await getIncident(token, id);
    setDetail(d);
    setEntries(d.timeline ?? []);
    setActions(d.actions ?? []);
  }, [getToken, id]);

  // Initial load.
  useEffect(() => { reload().catch(() => {}); }, [reload]);

  // Live time-open ticker.
  useEffect(() => {
    const t = setInterval(() => setTick((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, []);

  // Keep a fresh token for the WS auth handshake.
  useEffect(() => {
    let active = true;
    const refresh = async () => { const t = await getToken(); if (active) tokenRef.current = t; };
    refresh();
    const iv = setInterval(refresh, 45_000);
    return () => { active = false; clearInterval(iv); };
  }, [getToken]);

  // War-room WebSocket — live timeline + status/action updates.
  useEffect(() => {
    let ws: WebSocket | null = null;
    let closed = false;
    let reconnect: ReturnType<typeof setTimeout> | null = null;

    const connect = () => {
      ws = new WebSocket(incidentWsURL(id));
      ws.onopen = () => ws?.send(JSON.stringify({ type: "auth", token: tokenRef.current }));
      ws.onmessage = (ev) => {
        let msg: WsMessage;
        try { msg = JSON.parse(ev.data); } catch { return; }
        if (msg.type === "incident_timeline") {
          try {
            const entry = JSON.parse(msg.payload) as IncidentTimelineEntry;
            setEntries((prev) => prev.some((e) => e.id === entry.id) ? prev : [...prev, entry]);
          } catch {}
        } else if (msg.type === "incident_update" || msg.type === "incident_action") {
          reload().catch(() => {});
        }
      };
      ws.onclose = () => {
        if (!closed) reconnect = setTimeout(connect, 3000);
      };
    };
    connect();

    return () => {
      closed = true;
      if (reconnect) clearTimeout(reconnect);
      ws?.close();
    };
  }, [id, reload]);

  // Auto-scroll on new entries.
  useEffect(() => { bottomRef.current?.scrollIntoView({ behavior: "smooth" }); }, [entries.length]);

  async function handlePost() {
    const content = input.trim();
    if (!content) return;
    const token = await getToken();
    if (!token) return;
    setPosting(true);
    try {
      await postIncidentTimeline(token, id, content);
      setInput(""); // the entry arrives via WS broadcast
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setPosting(false);
    }
  }

  async function handleAcknowledge() {
    const token = await getToken();
    if (!token) return;
    setBusy("ack");
    try {
      await acknowledgeIncident(token, id);
      await reload();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setBusy(null);
    }
  }

  async function handleResolve() {
    const token = await getToken();
    if (!token) return;
    setBusy("resolve");
    try {
      const { postmortem } = await resolveIncident(token, id);
      setPmText(postmortem ?? "");
      setPmOpen(true);
      await reload();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setBusy(null);
    }
  }

  async function handlePublish() {
    const token = await getToken();
    if (!token) return;
    setPublishing(true);
    try {
      await savePostmortem(token, id, pmText);
      toast.success("Postmortem published");
      setPmOpen(false);
      await reload();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setPublishing(false);
    }
  }

  async function decideAction(actionId: string, approve: boolean) {
    const token = await getToken();
    if (!token) return;
    setBusy(actionId);
    try {
      if (approve) await approveIncidentAction(token, id, actionId);
      else await rejectIncidentAction(token, id, actionId);
      await reload();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setBusy(null);
    }
  }

  if (!incident) {
    return (
      <div className="min-h-screen bg-zinc-50">
        <Navbar />
        <div className="flex items-center justify-center h-64">
          <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
        </div>
      </div>
    );
  }

  const resolved = incident.status === "resolved";
  const pendingActions = actions.filter((a) => a.status === "pending");
  const decidedActions = actions.filter((a) => a.status !== "pending");

  return (
    <div className="min-h-screen bg-zinc-50 flex flex-col">
      <Navbar />

      {/* Postmortem modal */}
      <Dialog open={pmOpen} onOpenChange={setPmOpen}>
        <DialogContent className="max-w-2xl max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Postmortem</DialogTitle>
            <DialogDescription>AI-generated draft — edit as needed, then publish.</DialogDescription>
          </DialogHeader>
          <Textarea
            value={pmText}
            onChange={(e) => setPmText(e.target.value)}
            className="min-h-[320px] font-mono text-xs"
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setPmOpen(false)} disabled={publishing}>Close</Button>
            <Button onClick={handlePublish} disabled={publishing || !pmText.trim()}>
              {publishing ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : <FileText className="h-4 w-4 mr-1" />}
              Publish postmortem
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Top bar */}
      <div className="border-b bg-white px-4 py-2.5 flex items-center justify-between gap-3">
        <div className="flex items-center gap-3 min-w-0">
          <Button variant="ghost" size="sm" className="h-7 px-2" nativeButton={false} render={<Link href="/incidents" />}>
            <ArrowLeft className="h-3.5 w-3.5 mr-1" /> Incidents
          </Button>
          <Siren className={cn("h-4 w-4 shrink-0", resolved ? "text-green-600" : "text-red-600")} />
          <span className="font-medium text-sm truncate">{incident.title || "Incident"}</span>
          <span className={cn("rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase shrink-0", SEVERITY_BADGE[incident.severity])}>
            {incident.severity}
          </span>
          <span className={cn("rounded px-1.5 py-0.5 text-[10px] font-medium capitalize shrink-0", STATUS_BADGE[incident.status])}>
            {incident.status}
          </span>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <span className="flex items-center gap-1 text-xs text-muted-foreground">
            <Clock className="h-3 w-3" /> {timeOpen(incident.created_at, incident.resolved_at)}
          </span>
          {canAct && !resolved && incident.status === "open" && (
            <Button size="sm" variant="outline" onClick={handleAcknowledge} disabled={busy === "ack"}>
              {busy === "ack" ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : null}
              Acknowledge
            </Button>
          )}
          {canAct && !resolved && (
            <Button size="sm" onClick={handleResolve} disabled={busy === "resolve"}>
              {busy === "resolve" ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : <CheckCircle2 className="h-3.5 w-3.5 mr-1" />}
              Mark Resolved
            </Button>
          )}
          {resolved && incident.postmortem && (
            <Button size="sm" variant="outline" onClick={() => { setPmText(incident.postmortem ?? ""); setPmOpen(true); }}>
              <FileText className="h-3.5 w-3.5 mr-1" /> Postmortem
            </Button>
          )}
        </div>
      </div>

      {/* 3-panel layout */}
      <div className="flex-1 flex min-h-0">
        {/* LEFT — metadata */}
        <aside className="hidden md:block w-64 shrink-0 border-r bg-white p-4 space-y-4 overflow-y-auto">
          <Meta label="Status" value={incident.status} />
          <Meta label="Severity" value={incident.severity} />
          <div>
            <p className="text-[10px] uppercase tracking-wide text-muted-foreground">Service</p>
            <p className="text-sm flex items-center gap-1.5 mt-0.5">
              <Server className="h-3.5 w-3.5 text-zinc-400" />
              {incident.project_name}{incident.environment_name ? ` / ${incident.environment_name}` : ""}
            </p>
          </div>
          <Meta label="Time open" value={timeOpen(incident.created_at, incident.resolved_at)} />
          <Meta label="Acknowledged by" value={incident.acknowledged_by_name || "—"} />

          {/* AI explainability — confidence + the evidence behind the diagnosis. */}
          {(incident.confidence_score != null || (incident.evidence && incident.evidence.length > 0)) && (
            <div className="pt-1 border-t">
              <p className="text-[10px] uppercase tracking-wide text-muted-foreground mt-3 mb-1.5">AI diagnosis</p>
              <ConfidenceBadge score={incident.confidence_score} />
              <EvidenceSection items={incident.evidence} className="mt-2" />
            </div>
          )}

          {incident.deployment_id && (
            <Link href={`/projects/${incident.project_id}`} className="text-xs text-indigo-600 hover:underline block">
              View project →
            </Link>
          )}
        </aside>

        {/* CENTER — timeline */}
        <main className="flex-1 flex flex-col min-w-0">
          <div className="flex-1 overflow-y-auto px-4 py-4 space-y-3">
            {entries.map((e) => {
              const isAI = e.author_type === "ai";
              return (
                <div
                  key={e.id}
                  className={cn(
                    "rounded-md border-l-2 bg-white px-3 py-2 shadow-sm animate-in fade-in slide-in-from-bottom-1 duration-300",
                    isAI ? "border-l-indigo-500" : "border-l-zinc-400"
                  )}
                >
                  <div className="flex items-center gap-2 mb-1">
                    <Avatar className="h-5 w-5">
                      <AvatarFallback className={cn("text-[9px]", isAI ? "bg-indigo-600 text-white" : "bg-zinc-200")}>
                        {isAI ? <Bot className="h-3 w-3" /> : (e.author_name?.[0]?.toUpperCase() ?? "U")}
                      </AvatarFallback>
                    </Avatar>
                    <span className="text-xs font-medium">{isAI ? "OpsPilot AI" : (e.author_name || "Engineer")}</span>
                    <span className="text-[10px] text-muted-foreground">· {e.entry_type}</span>
                    <span className="text-[10px] text-muted-foreground ml-auto">
                      {new Date(e.created_at).toLocaleTimeString()}
                    </span>
                  </div>
                  <Markdown content={e.content} className="text-sm" />
                </div>
              );
            })}
            <div ref={bottomRef} />
          </div>

          {/* Update composer */}
          {canAct && !resolved && (
            <div className="border-t bg-white p-3">
              <div className="flex gap-2 items-end max-w-3xl mx-auto">
                <Textarea
                  value={input}
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={(e) => { if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) handlePost(); }}
                  placeholder="Add an update (plain text or markdown)… ⌘/Ctrl+Enter to send"
                  className="min-h-[44px] max-h-40 text-sm"
                />
                <Button onClick={handlePost} disabled={posting || !input.trim()}>
                  {posting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
                </Button>
              </div>
            </div>
          )}
        </main>

        {/* RIGHT — AI actions */}
        <aside className="hidden lg:block w-72 shrink-0 border-l bg-white p-4 overflow-y-auto">
          <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">AI Actions</p>
          {actions.length === 0 && (
            <p className="text-xs text-muted-foreground">No actions proposed.</p>
          )}
          <div className="space-y-3">
            {pendingActions.map((a) => (
              <div key={a.id} className="rounded-md border border-amber-200 bg-amber-50/50 p-2.5">
                <p className="text-xs font-medium capitalize">{a.action_type.replace(/_/g, " ")}</p>
                {typeof a.parameters?.description === "string" && (
                  <p className="text-xs text-muted-foreground mt-0.5">{a.parameters.description as string}</p>
                )}
                {canAct && (
                  <div className="flex gap-2 mt-2">
                    <Button size="sm" className="h-7 text-xs flex-1" onClick={() => decideAction(a.id, true)} disabled={busy === a.id}>
                      Approve
                    </Button>
                    <Button size="sm" variant="outline" className="h-7 text-xs flex-1" onClick={() => decideAction(a.id, false)} disabled={busy === a.id}>
                      Reject
                    </Button>
                  </div>
                )}
              </div>
            ))}
            {decidedActions.map((a) => (
              <div key={a.id} className="rounded-md border bg-zinc-50 p-2.5">
                <div className="flex items-center justify-between">
                  <p className="text-xs font-medium capitalize">{a.action_type.replace(/_/g, " ")}</p>
                  <span className={cn("text-[10px] capitalize", a.status === "rejected" ? "text-red-600" : "text-green-700")}>{a.status}</span>
                </div>
                <p className="text-[10px] text-muted-foreground mt-1">
                  {a.approved_by_name ? `by ${a.approved_by_name}` : ""}
                  {a.executed_at ? ` · ${new Date(a.executed_at).toLocaleString()}` : ""}
                </p>
              </div>
            ))}
          </div>
        </aside>
      </div>
    </div>
  );
}

function Meta({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <p className="text-[10px] uppercase tracking-wide text-muted-foreground">{label}</p>
      <p className="text-sm capitalize mt-0.5">{value}</p>
    </div>
  );
}
