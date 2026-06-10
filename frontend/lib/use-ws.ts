"use client";

import { useEffect, useRef, useState, useCallback } from "react";
import { wsURL } from "@/lib/api";
import type { WsMessage, ConversationMessage } from "@/types/api";

export interface ChatEntry {
  id: string;
  role: "user" | "assistant" | "system";
  content: string;
  type?: WsMessage["type"];
}

const MAX_RECONNECT_DELAY_MS = 15_000;

export function useProjectWS(projectId: string, token: string | null) {
  const [entries, setEntries] = useState<ChatEntry[]>([]);
  const [connected, setConnected] = useState(false);
  const [thinking, setThinking] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const reconnectAttempt = useRef(0);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const closedByUs = useRef(false);

  // The latest token lives in a ref so periodic Clerk token refreshes don't tear
  // down and reconnect the socket every ~45s. The connection effect keys off
  // token *presence*; the current value is read at auth time.
  const tokenRef = useRef(token);
  useEffect(() => {
    tokenRef.current = token;
  }, [token]);
  const hasToken = !!token;

  const push = useCallback((entry: Omit<ChatEntry, "id">) => {
    setEntries((prev) => [...prev, { id: crypto.randomUUID(), ...entry }]);
  }, []);

  // Update the trailing assistant entry in place (streamed progress), or append a
  // new one if the last entry isn't assistant — e.g. the user opened the page while
  // a deploy was already running. Previously such events were silently dropped.
  const upsertAssistant = useCallback((content: string, type: WsMessage["type"]) => {
    setEntries((prev) => {
      const last = prev[prev.length - 1];
      if (last && last.role === "assistant" && last.type !== "response") {
        return [...prev.slice(0, -1), { ...last, content, type }];
      }
      return [...prev, { id: crypto.randomUUID(), role: "assistant", content, type }];
    });
  }, []);

  useEffect(() => {
    if (!hasToken || !projectId) return;
    closedByUs.current = false;

    const connect = () => {
      const ws = new WebSocket(wsURL(projectId));
      wsRef.current = ws;

      ws.onopen = () => {
        // Send auth token as the first message — token never appears in the URL.
        // connected is set to true only after the server ACKs with auth_ok.
        ws.send(JSON.stringify({ type: "auth", token: tokenRef.current }));
      };

      ws.onclose = () => {
        setConnected(false);
        if (closedByUs.current) return;
        // Reconnect with exponential backoff so live deploy progress survives
        // network blips and backend restarts.
        const delay = Math.min(1000 * 2 ** reconnectAttempt.current, MAX_RECONNECT_DELAY_MS);
        reconnectAttempt.current += 1;
        reconnectTimer.current = setTimeout(connect, delay);
      };

      ws.onmessage = (ev) => {
        let msg: WsMessage;
        try {
          msg = JSON.parse(ev.data);
        } catch {
          return;
        }

        switch (msg.type) {
          case "auth_ok":
            setConnected(true);
            reconnectAttempt.current = 0;
            break;

          case "thinking":
            setThinking(true);
            push({ role: "assistant", content: "...", type: "thinking" });
            break;

          case "response":
            setThinking(false);
            upsertAssistant(msg.payload, "response");
            break;

          case "deploy_progress":
          case "provision_progress":
          case "deploy_done":
          case "provision_done":
          case "deploy_failed":
          case "provision_failed":
            setThinking(false);
            upsertAssistant(msg.payload, msg.type);
            break;

          case "error":
            setThinking(false);
            push({ role: "system", content: msg.payload, type: "error" });
            break;
        }
      };
    };

    connect();

    return () => {
      closedByUs.current = true;
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
      wsRef.current?.close();
      wsRef.current = null;
      setConnected(false);
    };
  }, [projectId, hasToken, push, upsertAssistant]);

  const send = useCallback(
    (message: string) => {
      if (!wsRef.current || wsRef.current.readyState !== WebSocket.OPEN) return false;
      push({ role: "user", content: message });
      wsRef.current.send(JSON.stringify({ message }));
      return true;
    },
    [push]
  );

  // Populate entries from persisted conversation history.
  // Call once after the initial API fetch; live messages then stream in via WS.
  const loadHistory = useCallback((messages: ConversationMessage[]) => {
    if (!messages.length) return;
    setEntries(
      messages.map((m) => ({
        id: m.id,
        role: m.role as ChatEntry["role"],
        content: m.message,
        type: "response" as WsMessage["type"],
      }))
    );
  }, []);

  return { entries, connected, thinking, send, loadHistory };
}
