"use client";

import { useEffect, useRef, useState, FormEvent } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Badge } from "@/components/ui/badge";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { useProjectWS, type ChatEntry } from "@/lib/use-ws";
import { getConversationHistory, getProject } from "@/lib/api";
import type { Project, ConversationMessage } from "@/types/api";
import { ArrowLeft, Send, Wifi, WifiOff, Loader2, Rocket } from "lucide-react";
import { cn } from "@/lib/utils";

const SUGGESTED = [
  "Deploy to production",
  "Show me the latest deployment status",
  "Check application health",
  "Show me recent logs",
  "Roll back the last deployment",
];

function msgClass(type: ChatEntry["type"]) {
  switch (type) {
    case "deploy_done":
    case "provision_done":
      return "bg-green-50 border border-green-200 text-green-900";
    case "deploy_failed":
    case "provision_failed":
    case "error":
      return "bg-red-50 border border-red-200 text-red-900";
    case "deploy_progress":
    case "provision_progress":
      return "bg-blue-50 border border-blue-200 text-blue-900";
    default:
      return "bg-muted";
  }
}

function MessageBubble({ entry }: { entry: ChatEntry }) {
  const isUser = entry.role === "user";
  const isSystem = entry.role === "system";

  if (isSystem) {
    return (
      <div className="flex justify-center">
        <span className={cn("text-xs px-3 py-1.5 rounded-full", msgClass(entry.type))}>
          {entry.content}
        </span>
      </div>
    );
  }

  return (
    <div className={cn("flex gap-3 max-w-3xl", isUser ? "ml-auto flex-row-reverse" : "mr-auto")}>
      <Avatar className="h-7 w-7 shrink-0 mt-0.5">
        <AvatarFallback className={cn("text-xs", isUser ? "bg-indigo-600 text-white" : "bg-zinc-200")}>
          {isUser ? "U" : "AI"}
        </AvatarFallback>
      </Avatar>

      <div
        className={cn(
          "rounded-2xl px-4 py-2.5 text-sm max-w-[75%]",
          isUser
            ? "bg-indigo-600 text-white rounded-tr-sm"
            : cn(msgClass(entry.type), "rounded-tl-sm")
        )}
      >
        {entry.type === "thinking" ? (
          <span className="flex items-center gap-1.5 text-muted-foreground">
            <Loader2 className="h-3 w-3 animate-spin" />
            Thinking...
          </span>
        ) : (
          <p className="whitespace-pre-wrap leading-relaxed">{entry.content}</p>
        )}
      </div>
    </div>
  );
}

export default function ChatPage() {
  const { id } = useParams<{ id: string }>();
  const { getToken } = useAuth();

  const [token, setToken] = useState<string | null>(null);
  const [project, setProject] = useState<Project | null>(null);
  const [input, setInput] = useState("");
  const [historyLoaded, setHistoryLoaded] = useState(false);
  const bottomRef = useRef<HTMLDivElement>(null);

  const { entries, connected, thinking, send } = useProjectWS(id, token);

  // Refresh token periodically (Clerk tokens expire after ~60s)
  useEffect(() => {
    let mounted = true;
    async function refresh() {
      const t = await getToken();
      if (mounted) setToken(t);
    }
    refresh();
    const interval = setInterval(refresh, 45_000);
    return () => {
      mounted = false;
      clearInterval(interval);
    };
  }, [getToken]);

  useEffect(() => {
    if (!token) return;
    Promise.all([
      getProject(token, id).then(setProject),
      getConversationHistory(token, id),
    ]).then(([, history]) => {
      // History is loaded once at mount; live messages come through WS
      void history;
      setHistoryLoaded(true);
    }).catch(() => setHistoryLoaded(true));
  }, [token, id]);

  // Auto-scroll on new messages
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [entries]);

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const msg = input.trim();
    if (!msg || thinking) return;
    send(msg);
    setInput("");
  }

  return (
    <div className="flex flex-col h-screen bg-zinc-50">
      <Navbar />

      {/* Sub-header */}
      <div className="border-b bg-white px-4 py-2.5 flex items-center justify-between">
        <div className="flex items-center gap-3">
          <Button variant="ghost" size="sm" className="h-7 px-2" nativeButton={false} render={<Link href={`/projects/${id}`} />}>
            <ArrowLeft className="h-3.5 w-3.5 mr-1" />
            Back
          </Button>
          <div className="flex items-center gap-2">
            <Rocket className="h-4 w-4 text-indigo-600" />
            <span className="font-medium text-sm">{project?.name ?? "..."}</span>
          </div>
        </div>

        <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
          {connected ? (
            <>
              <Wifi className="h-3.5 w-3.5 text-green-500" />
              <span>Connected</span>
            </>
          ) : (
            <>
              <WifiOff className="h-3.5 w-3.5 text-zinc-400" />
              <span>Connecting...</span>
            </>
          )}
        </div>
      </div>

      {/* Messages */}
      <ScrollArea className="flex-1 px-4">
        <div className="max-w-3xl mx-auto py-6 space-y-4">
          {/* Welcome state */}
          {historyLoaded && entries.length === 0 && (
            <div className="text-center py-12">
              <Rocket className="h-10 w-10 text-zinc-300 mx-auto mb-3" />
              <h2 className="font-semibold text-sm">Chat with ConvDeploy</h2>
              <p className="text-muted-foreground text-xs mt-1 mb-6">
                Deploy, rollback, check logs, and diagnose issues — all in conversation.
              </p>
              <div className="flex flex-wrap justify-center gap-2">
                {SUGGESTED.map((s) => (
                  <button
                    key={s}
                    onClick={() => { send(s); }}
                    className="text-xs px-3 py-1.5 rounded-full border hover:bg-zinc-100 transition-colors"
                  >
                    {s}
                  </button>
                ))}
              </div>
            </div>
          )}

          {entries.map((entry) => (
            <MessageBubble key={entry.id} entry={entry} />
          ))}

          <div ref={bottomRef} />
        </div>
      </ScrollArea>

      {/* Suggested pills (shown while conversation is active) */}
      {entries.length > 0 && (
        <div className="border-t bg-white px-4 py-2 flex gap-2 overflow-x-auto">
          {SUGGESTED.slice(0, 3).map((s) => (
            <button
              key={s}
              onClick={() => { setInput(s); }}
              className="shrink-0 text-xs px-3 py-1 rounded-full border hover:bg-zinc-100 transition-colors whitespace-nowrap"
            >
              {s}
            </button>
          ))}
        </div>
      )}

      {/* Input bar */}
      <div className="border-t bg-white px-4 py-3">
        <form onSubmit={handleSubmit} className="max-w-3xl mx-auto flex gap-2">
          <Input
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder={connected ? "Ask anything about your deployment..." : "Connecting to server..."}
            disabled={!connected || thinking}
            className="flex-1"
            autoFocus
          />
          <Button type="submit" size="icon" disabled={!input.trim() || !connected || thinking}>
            {thinking ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <Send className="h-4 w-4" />
            )}
          </Button>
        </form>

        {thinking && (
          <p className="text-center text-xs text-muted-foreground mt-2">
            ConvDeploy is working on your request...
          </p>
        )}
      </div>
    </div>
  );
}
