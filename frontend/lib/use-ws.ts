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

export function useProjectWS(projectId: string, token: string | null) {
  const [entries, setEntries] = useState<ChatEntry[]>([]);
  const [connected, setConnected] = useState(false);
  const [thinking, setThinking] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);

  const push = useCallback((entry: Omit<ChatEntry, "id">) => {
    setEntries((prev) => [...prev, { id: crypto.randomUUID(), ...entry }]);
  }, []);

  const updateLast = useCallback((content: string, type: WsMessage["type"]) => {
    setEntries((prev) => {
      if (prev.length === 0) return prev;
      const last = prev[prev.length - 1];
      if (last.role !== "assistant") return prev;
      return [...prev.slice(0, -1), { ...last, content, type }];
    });
  }, []);

  useEffect(() => {
    if (!token || !projectId) return;

    const ws = new WebSocket(wsURL(projectId));
    wsRef.current = ws;

    ws.onopen = () => {
      // Send auth token as the first message — token never appears in the URL.
      // connected is set to true only after the server ACKs with auth_ok.
      ws.send(JSON.stringify({ type: "auth", token }));
    };
    ws.onclose = () => setConnected(false);

    ws.onmessage = (ev) => {
      const msg: WsMessage = JSON.parse(ev.data);

      switch (msg.type) {
        case "auth_ok":
          setConnected(true);
          break;

        case "thinking":
          setThinking(true);
          push({ role: "assistant", content: "...", type: "thinking" });
          break;

        case "response":
          setThinking(false);
          updateLast(msg.payload, "response");
          break;

        case "deploy_progress":
        case "provision_progress":
          setThinking(false);
          updateLast(msg.payload, msg.type);
          break;

        case "deploy_done":
        case "provision_done":
          setThinking(false);
          updateLast(msg.payload, msg.type);
          break;

        case "deploy_failed":
        case "provision_failed":
          setThinking(false);
          updateLast(msg.payload, msg.type);
          break;

        case "error":
          setThinking(false);
          push({ role: "system", content: msg.payload, type: "error" });
          break;
      }
    };

    return () => {
      ws.close();
      wsRef.current = null;
      setConnected(false);
    };
  }, [projectId, token, push, updateLast]);

  const send = useCallback(
    (message: string) => {
      if (!wsRef.current || wsRef.current.readyState !== WebSocket.OPEN) return;
      push({ role: "user", content: message });
      wsRef.current.send(JSON.stringify({ message }));
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
