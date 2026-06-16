"use client";

import { useEffect, useState, useCallback } from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import { useAuth } from "@clerk/nextjs";
import { Navbar } from "@/components/layout/navbar";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Markdown } from "@/lib/markdown";
import {
  getPostmortem, updatePostmortem, publishPostmortem, exportPostmortem,
} from "@/lib/api";
import { useActiveOrg } from "@/lib/use-org";
import type { Postmortem, ActionItem } from "@/types/api";
import { cn } from "@/lib/utils";
import { toast } from "sonner";
import {
  ArrowLeft, Loader2, Save, FileText, Eye, Pencil, Download, FileDown, Plus, Trash2,
} from "lucide-react";

export default function PostmortemEditorPage() {
  const { id } = useParams<{ id: string }>();
  const { getToken } = useAuth();
  const { canAct } = useActiveOrg();

  const [pm, setPm] = useState<Postmortem | null>(null);
  const [title, setTitle] = useState("");
  const [content, setContent] = useState("");
  const [items, setItems] = useState<ActionItem[]>([]);
  const [tab, setTab] = useState<"edit" | "preview">("edit");
  const [saving, setSaving] = useState(false);
  const [publishing, setPublishing] = useState(false);
  const [dirty, setDirty] = useState(false);

  const load = useCallback(async () => {
    const token = await getToken();
    if (!token) return;
    const p = await getPostmortem(token, id);
    setPm(p);
    setTitle(p.title);
    setContent(p.content_markdown);
    setItems(p.action_items ?? []);
    setDirty(false);
  }, [getToken, id]);

  useEffect(() => { load().catch((e) => toast.error((e as Error).message)); }, [load]);

  const markDirty = () => setDirty(true);

  async function handleSave() {
    const token = await getToken();
    if (!token) return;
    setSaving(true);
    try {
      await updatePostmortem(token, id, { title, content_markdown: content, action_items: items });
      toast.success("Saved");
      setDirty(false);
      await load();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function handlePublish() {
    const token = await getToken();
    if (!token) return;
    setPublishing(true);
    try {
      if (dirty) await updatePostmortem(token, id, { title, content_markdown: content, action_items: items });
      await publishPostmortem(token, id);
      toast.success("Published to the org library");
      await load();
    } catch (e: unknown) {
      toast.error((e as Error).message);
    } finally {
      setPublishing(false);
    }
  }

  async function handleExport(format: "md" | "pdf") {
    const token = await getToken();
    if (!token) return;
    try {
      await exportPostmortem(token, id, format, title || "postmortem");
    } catch (e: unknown) {
      toast.error((e as Error).message);
    }
  }

  function updateItem(idx: number, patch: Partial<ActionItem>) {
    setItems((prev) => prev.map((it, i) => (i === idx ? { ...it, ...patch } : it)));
    markDirty();
  }
  function addItem() {
    setItems((prev) => [...prev, { item: "", owner: "", priority: "medium", status: "open" }]);
    markDirty();
  }
  function removeItem(idx: number) {
    setItems((prev) => prev.filter((_, i) => i !== idx));
    markDirty();
  }

  if (!pm) {
    return (
      <div className="min-h-screen bg-zinc-50">
        <Navbar />
        <div className="flex items-center justify-center h-64">
          <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
        </div>
      </div>
    );
  }

  const published = pm.status === "published";
  const readOnly = !canAct;

  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-3xl mx-auto px-4 py-8">
        {/* Header */}
        <div className="flex items-center justify-between gap-3 mb-4">
          <Button variant="ghost" size="sm" className="h-8 px-2" nativeButton={false}
            render={<Link href={`/incidents/${pm.incident_id}`} />}>
            <ArrowLeft className="h-3.5 w-3.5 mr-1" /> War room
          </Button>
          <div className="flex items-center gap-2">
            <span className={cn("rounded px-2 py-0.5 text-[11px] font-medium capitalize",
              published ? "bg-green-100 text-green-700" : "bg-amber-100 text-amber-700")}>
              {pm.status}
            </span>
            <Button variant="outline" size="sm" onClick={() => handleExport("md")}>
              <Download className="h-3.5 w-3.5 mr-1" /> .md
            </Button>
            <Button variant="outline" size="sm" onClick={() => handleExport("pdf")}>
              <FileDown className="h-3.5 w-3.5 mr-1" /> PDF
            </Button>
            {!readOnly && (
              <>
                <Button variant="outline" size="sm" onClick={handleSave} disabled={saving || !dirty}>
                  {saving ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : <Save className="h-3.5 w-3.5 mr-1" />}
                  Save
                </Button>
                <Button size="sm" onClick={handlePublish} disabled={publishing}>
                  {publishing ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : <FileText className="h-3.5 w-3.5 mr-1" />}
                  {published ? "Re-publish" : "Publish"}
                </Button>
              </>
            )}
          </div>
        </div>

        {/* Title */}
        <Input
          value={title}
          onChange={(e) => { setTitle(e.target.value); markDirty(); }}
          disabled={readOnly}
          className="text-lg font-semibold mb-4"
          placeholder="Postmortem title"
        />

        {/* Edit / Preview toggle */}
        <div className="flex items-center gap-1 mb-2">
          <Button variant={tab === "edit" ? "secondary" : "ghost"} size="sm" className="h-7" onClick={() => setTab("edit")}>
            <Pencil className="h-3.5 w-3.5 mr-1" /> Edit
          </Button>
          <Button variant={tab === "preview" ? "secondary" : "ghost"} size="sm" className="h-7" onClick={() => setTab("preview")}>
            <Eye className="h-3.5 w-3.5 mr-1" /> Preview
          </Button>
        </div>

        {tab === "edit" ? (
          <Textarea
            value={content}
            onChange={(e) => { setContent(e.target.value); markDirty(); }}
            disabled={readOnly}
            className="min-h-[480px] font-mono text-xs"
          />
        ) : (
          <div className="rounded-md border bg-white p-5 min-h-[480px]">
            <Markdown content={content} className="text-sm prose-sm max-w-none" />
          </div>
        )}

        {/* Action items */}
        <div className="mt-6">
          <div className="flex items-center justify-between mb-2">
            <h2 className="text-sm font-semibold">Action items</h2>
            {!readOnly && (
              <Button variant="outline" size="sm" className="h-7" onClick={addItem}>
                <Plus className="h-3.5 w-3.5 mr-1" /> Add
              </Button>
            )}
          </div>
          {items.length === 0 ? (
            <p className="text-xs text-muted-foreground">No action items.</p>
          ) : (
            <div className="space-y-2">
              {items.map((it, idx) => (
                <div key={idx} className="rounded-md border bg-white p-2.5 flex items-start gap-2">
                  <div className="flex-1 space-y-1.5">
                    <Input
                      value={it.item}
                      onChange={(e) => updateItem(idx, { item: e.target.value })}
                      disabled={readOnly}
                      placeholder="What needs to happen"
                      className="text-sm"
                    />
                    <div className="flex gap-1.5">
                      <Input
                        value={it.owner ?? ""}
                        onChange={(e) => updateItem(idx, { owner: e.target.value })}
                        disabled={readOnly}
                        placeholder="Owner"
                        className="h-8 text-xs"
                      />
                      <Input
                        value={it.priority ?? ""}
                        onChange={(e) => updateItem(idx, { priority: e.target.value })}
                        disabled={readOnly}
                        placeholder="Priority"
                        className="h-8 text-xs w-28"
                      />
                      <Input
                        value={it.due_date ?? ""}
                        onChange={(e) => updateItem(idx, { due_date: e.target.value })}
                        disabled={readOnly}
                        placeholder="Due"
                        className="h-8 text-xs w-32"
                      />
                    </div>
                  </div>
                  {!readOnly && (
                    <Button variant="ghost" size="sm" className="h-8 px-2 text-muted-foreground" onClick={() => removeItem(idx)}>
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </main>
    </div>
  );
}
