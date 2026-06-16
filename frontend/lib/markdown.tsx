import React from "react";

// Markdown is a dependency-free renderer for the markdown the AI produces in diagnoses
// and postmortems: ## headings, **bold**, `code`, ``` fences, ordered + unordered lists,
// GFM pipe tables, blockquotes, and horizontal rules. It is intentionally minimal (no raw
// HTML passthrough) so it is safe to render model output without sanitization concerns.
export function Markdown({ content, className }: { content: string; className?: string }) {
  return <div className={className}>{renderBlocks(content)}</div>;
}

// isTableSeparator matches the GFM header-underline row, e.g. | --- | :--: |.
function isTableSeparator(line: string): boolean {
  return line.includes("-") && /^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$/.test(line);
}

// splitRow splits a pipe-table row into trimmed cells (honoring escaped \|).
function splitRow(line: string): string[] {
  return line
    .trim()
    .replace(/^\|/, "")
    .replace(/\|$/, "")
    .split(/(?<!\\)\|/)
    .map((c) => c.replace(/\\\|/g, "|").trim());
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

    // Horizontal rule
    if (/^\s*([-*_])\1{2,}\s*$/.test(line)) {
      out.push(<hr key={key++} className="my-3 border-zinc-200" />);
      i++;
      continue;
    }

    // GFM pipe table — a header row followed by a separator row.
    if (line.includes("|") && i + 1 < lines.length && isTableSeparator(lines[i + 1])) {
      const header = splitRow(line);
      i += 2; // header + separator
      const rows: string[][] = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim() !== "") {
        rows.push(splitRow(lines[i]));
        i++;
      }
      out.push(
        <div key={key++} className="my-2 overflow-x-auto">
          <table className="w-full text-sm border-collapse">
            <thead>
              <tr>
                {header.map((c, idx) => (
                  <th key={idx} className="border border-zinc-200 bg-zinc-50 px-2 py-1 text-left font-medium">{renderInline(c)}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((r, ri) => (
                <tr key={ri}>
                  {header.map((_, ci) => (
                    <td key={ci} className="border border-zinc-200 px-2 py-1 align-top">{renderInline(r[ci] ?? "")}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
      continue;
    }

    // Blockquote (consecutive "> " lines)
    if (/^\s*>\s?/.test(line)) {
      const quoted: string[] = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
        quoted.push(lines[i].replace(/^\s*>\s?/, ""));
        i++;
      }
      out.push(
        <blockquote key={key++} className="my-2 border-l-2 border-zinc-300 pl-3 text-sm text-muted-foreground italic">
          {quoted.map((q, idx) => <p key={idx} className="my-0.5">{renderInline(q)}</p>)}
        </blockquote>
      );
      continue;
    }

    // Ordered list
    if (/^\s*\d+\.\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*\d+\.\s+/, ""));
        i++;
      }
      out.push(
        <ol key={key++} className="my-1 ml-5 list-decimal space-y-0.5 text-sm">
          {items.map((it, idx) => <li key={idx}>{renderInline(it)}</li>)}
        </ol>
      );
      continue;
    }

    // Bullet list (supports one level of nesting via indentation)
    if (/^\s*[-*]\s+/.test(line)) {
      const items: { text: string; nested: boolean }[] = [];
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        const indent = /^(\s*)/.exec(lines[i])?.[1].length ?? 0;
        items.push({ text: lines[i].replace(/^\s*[-*]\s+/, ""), nested: indent >= 2 });
        i++;
      }
      out.push(
        <ul key={key++} className="my-1 ml-4 list-disc space-y-0.5 text-sm">
          {items.map((it, idx) => <li key={idx} className={it.nested ? "ml-4" : undefined}>{renderInline(it.text)}</li>)}
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
