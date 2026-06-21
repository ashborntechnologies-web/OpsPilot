"use client";

// Lightweight, dependency-free SVG charts for the leadership dashboard. (recharts is not
// installed; these render crisply, carry no bundle cost, and cover the two shapes we need:
// an uptime line with an SLA reference line, and a weekly incident bar chart.) Hover shows
// an in-SVG crosshair + tooltip — coordinate-consistent with the viewBox, no DOM overlay.

import { useRef, useState } from "react";
import type { UptimePoint, IncidentPoint } from "@/types/api";

const W = 560; // viewBox width; the SVG scales to its container via width="100%"
const H = 200;
const PAD = { top: 12, right: 12, bottom: 24, left: 36 };

function fmtDay(d: string) {
  const [, m, day] = d.split("-").map(Number);
  return `${m}/${day}`;
}

// Tooltip renders a small in-SVG label box near (cx, top), clamped horizontally so it
// doesn't overflow the viewBox. lines[0] is bold-ish (slightly larger).
function Tooltip({ cx, lines }: { cx: number; lines: string[] }) {
  const w = Math.max(58, ...lines.map((l) => l.length * 5.6));
  const x = Math.min(Math.max(cx - w / 2, 2), W - w - 2);
  const h = 12 + lines.length * 12;
  return (
    <g pointerEvents="none">
      <rect x={x} y={2} width={w} height={h} rx="3" fill="#18181b" opacity="0.92" />
      {lines.map((l, i) => (
        <text key={i} x={x + w / 2} y={15 + i * 12} textAnchor="middle" fontSize={i === 0 ? "10" : "9"}
          fill={i === 0 ? "#fff" : "#d4d4d8"}>{l}</text>
      ))}
    </g>
  );
}

// nearestIndex maps a mouse event over the plot to the closest data index across the inner
// plot width.
function nearestIndex(e: React.MouseEvent<SVGSVGElement>, svg: SVGSVGElement | null, n: number): number {
  if (!svg || n === 0) return -1;
  const rect = svg.getBoundingClientRect();
  const innerW = W - PAD.left - PAD.right;
  const xView = ((e.clientX - rect.left) / rect.width) * W;
  const ratio = Math.min(Math.max((xView - PAD.left) / innerW, 0), 1);
  return Math.round(ratio * (n - 1));
}

// UptimeLineChart plots uptime % per day with a red dashed SLA reference line.
export function UptimeLineChart({ points, slaTarget }: { points: UptimePoint[]; slaTarget: number }) {
  const svgRef = useRef<SVGSVGElement>(null);
  const [hover, setHover] = useState(-1);
  if (points.length === 0) {
    return <EmptyChart label="No uptime data yet" />;
  }
  const innerW = W - PAD.left - PAD.right;
  const innerH = H - PAD.top - PAD.bottom;

  const values = points.map((p) => p.uptime_pct);
  // Y range: a touch below the lower of (min uptime, SLA target), up to 100.
  const lo = Math.min(...values, slaTarget) - 0.2;
  const yMin = Math.max(0, Math.floor(lo * 10) / 10);
  const yMax = 100;
  const x = (i: number) => PAD.left + (points.length === 1 ? innerW / 2 : (i / (points.length - 1)) * innerW);
  const y = (v: number) => PAD.top + innerH - ((v - yMin) / (yMax - yMin || 1)) * innerH;

  const line = points.map((p, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(p.uptime_pct).toFixed(1)}`).join(" ");
  const area = `${line} L${x(points.length - 1).toFixed(1)},${(PAD.top + innerH).toFixed(1)} L${x(0).toFixed(1)},${(PAD.top + innerH).toFixed(1)} Z`;
  const slaY = y(slaTarget);
  const ticks = [yMin, (yMin + yMax) / 2, yMax];
  const labelEvery = Math.ceil(points.length / 8);

  const hp = hover >= 0 && hover < points.length ? points[hover] : null;

  return (
    <svg ref={svgRef} viewBox={`0 0 ${W} ${H}`} className="w-full h-auto" role="img" aria-label="Uptime per day"
      onMouseMove={(e) => setHover(nearestIndex(e, svgRef.current, points.length))}
      onMouseLeave={() => setHover(-1)}>
      {ticks.map((t) => (
        <g key={t}>
          <line x1={PAD.left} y1={y(t)} x2={W - PAD.right} y2={y(t)} stroke="#f4f4f5" />
          <text x={PAD.left - 6} y={y(t) + 3} textAnchor="end" fontSize="9" fill="#a1a1aa">{t.toFixed(1)}</text>
        </g>
      ))}
      {/* SLA reference line */}
      {slaY >= PAD.top && slaY <= PAD.top + innerH && (
        <g>
          <line x1={PAD.left} y1={slaY} x2={W - PAD.right} y2={slaY} stroke="#ef4444" strokeWidth="1" strokeDasharray="4 3" />
          <text x={W - PAD.right} y={slaY - 4} textAnchor="end" fontSize="9" fill="#ef4444">SLA {slaTarget}%</text>
        </g>
      )}
      <path d={area} fill="#6366f1" fillOpacity="0.08" />
      <path d={line} fill="none" stroke="#6366f1" strokeWidth="1.75" strokeLinejoin="round" />
      {points.map((p, i) =>
        i % labelEvery === 0 ? (
          <text key={p.date} x={x(i)} y={H - 8} textAnchor="middle" fontSize="9" fill="#a1a1aa">{fmtDay(p.date)}</text>
        ) : null
      )}
      {/* Hover crosshair + dot + tooltip */}
      {hp && (
        <g pointerEvents="none">
          <line x1={x(hover)} y1={PAD.top} x2={x(hover)} y2={PAD.top + innerH} stroke="#a1a1aa" strokeDasharray="3 3" />
          <circle cx={x(hover)} cy={y(hp.uptime_pct)} r="3" fill="#6366f1" stroke="#fff" strokeWidth="1.5" />
          <Tooltip cx={x(hover)} lines={[`${hp.uptime_pct.toFixed(2)}%`, hp.date]} />
        </g>
      )}
    </svg>
  );
}

// IncidentBarChart plots incident counts per week.
export function IncidentBarChart({ points }: { points: IncidentPoint[] }) {
  const svgRef = useRef<SVGSVGElement>(null);
  const [hover, setHover] = useState(-1);
  if (points.length === 0) {
    return <EmptyChart label="No incident data yet" />;
  }
  const innerW = W - PAD.left - PAD.right;
  const innerH = H - PAD.top - PAD.bottom;
  const maxCount = Math.max(1, ...points.map((p) => p.count));
  const slot = innerW / points.length;
  const barW = Math.min(28, slot * 0.6);
  const y = (v: number) => PAD.top + innerH - (v / maxCount) * innerH;
  const ticks = [0, Math.ceil(maxCount / 2), maxCount];
  const labelEvery = Math.ceil(points.length / 8);

  // Bars occupy slots, so hit-test by slot rather than nearest line point.
  const slotIndex = (e: React.MouseEvent<SVGSVGElement>): number => {
    if (!svgRef.current) return -1;
    const rect = svgRef.current.getBoundingClientRect();
    const xView = ((e.clientX - rect.left) / rect.width) * W;
    const i = Math.floor((xView - PAD.left) / slot);
    return i >= 0 && i < points.length ? i : -1;
  };
  const hp = hover >= 0 && hover < points.length ? points[hover] : null;

  return (
    <svg ref={svgRef} viewBox={`0 0 ${W} ${H}`} className="w-full h-auto" role="img" aria-label="Incidents per week"
      onMouseMove={(e) => setHover(slotIndex(e))} onMouseLeave={() => setHover(-1)}>
      {ticks.map((t) => (
        <g key={t}>
          <line x1={PAD.left} y1={y(t)} x2={W - PAD.right} y2={y(t)} stroke="#f4f4f5" />
          <text x={PAD.left - 6} y={y(t) + 3} textAnchor="end" fontSize="9" fill="#a1a1aa">{t}</text>
        </g>
      ))}
      {points.map((p, i) => {
        const cx = PAD.left + i * slot + slot / 2;
        const barH = PAD.top + innerH - y(p.count);
        return (
          <g key={p.week_start}>
            <rect x={cx - barW / 2} y={y(p.count)} width={barW} height={Math.max(0, barH)} rx="2"
              fill={p.count > 0 ? "#6366f1" : "#e4e4e7"} opacity={hover < 0 || hover === i ? 1 : 0.5} />
            {i % labelEvery === 0 && (
              <text x={cx} y={H - 8} textAnchor="middle" fontSize="9" fill="#a1a1aa">{fmtDay(p.week_start)}</text>
            )}
          </g>
        );
      })}
      {hp && (
        <Tooltip cx={PAD.left + hover * slot + slot / 2}
          lines={[`${hp.count} incident${hp.count === 1 ? "" : "s"}`, `week of ${hp.week_start}`]} />
      )}
    </svg>
  );
}

function EmptyChart({ label }: { label: string }) {
  return (
    <div className="flex items-center justify-center h-[200px] text-xs text-muted-foreground">{label}</div>
  );
}
