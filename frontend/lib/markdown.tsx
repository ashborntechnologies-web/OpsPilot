import React from "react";

// Markdown is a tiny, dependency-free renderer for the subset of markdown the AI
// produces in diagnoses and postmortems: ## headings, **bold**, `code`, ``` fences,
// and "- " bullet lists. It is intentionally minimal (no HTML passthrough) so it is
// safe to render model output without sanitization concerns.
export function Markdown({ content, className }: { content: string; className?: string }) {
  return <div className={className}>{renderBlocks(content)}</div>;
}

function renderBlocks(text: string): React.ReactNode[] {
  const lines = text.replace(/\r\n/g, "\n").split("\n");
  const out: React.ReactNode[] = [];
  let i = 0;
  let key = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Fenced code block
    if (line.trim().startsWith("```")) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith("```")) {
        body.push(lines[i]);
        i++;
      }
      i++; // closing fence
      out.push(
        <pre key={key++} className="my-2 rounded bg-zinc-950 text-zinc-100 text-xs p-3 overflow-x-auto whitespace-pre-wrap">
          {body.join("\n")}
        </pre>
      );
      continue;
    }

    // Headings
    const h = /^(#{1,4})\s+(.*)$/.exec(line);
    if (h) {
      const level = h[1].length;
      const cls = level <= 2 ? "text-sm font-semibold mt-3 mb-1" : "text-xs font-semibold mt-2 mb-1 text-muted-foreground";
      out.push(<p key={key++} className={cls}>{renderInline(h[2])}</p>);
      i++;
      continue;
    }

    // Bullet list
    if (/^\s*[-*]\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*[-*]\s+/, ""));
        i++;
      }
      out.push(
        <ul key={key++} className="my-1 ml-4 list-disc space-y-0.5 text-sm">
          {items.map((it, idx) => <li key={idx}>{renderInline(it)}</li>)}
        </ul>
      );
      continue;
    }

    // Blank line
    if (line.trim() === "") {
      i++;
      continue;
    }

    // Paragraph
    out.push(<p key={key++} className="text-sm leading-relaxed my-1">{renderInline(line)}</p>);
    i++;
  }
  return out;
}

// renderInline handles **bold** and `code` spans.
function renderInline(text: string): React.ReactNode[] {
  const nodes: React.ReactNode[] = [];
  const regex = /(\*\*[^*]+\*\*|`[^`]+`)/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let key = 0;
  while ((m = regex.exec(text)) !== null) {
    if (m.index > last) nodes.push(text.slice(last, m.index));
    const tok = m[0];
    if (tok.startsWith("**")) {
      nodes.push(<strong key={key++}>{tok.slice(2, -2)}</strong>);
    } else {
      nodes.push(<code key={key++} className="rounded bg-zinc-100 px-1 py-0.5 text-[0.85em] font-mono">{tok.slice(1, -1)}</code>);
    }
    last = m.index + tok.length;
  }
  if (last < text.length) nodes.push(text.slice(last));
  return nodes;
}
