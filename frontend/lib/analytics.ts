import type { SLAStatus } from "@/types/api";

// SLA_BADGE maps an SLA status to a Tailwind badge class + label.
export const SLA_BADGE: Record<SLAStatus, { cls: string; label: string }> = {
  meeting: { cls: "bg-green-100 text-green-700", label: "Meeting" },
  at_risk: { cls: "bg-amber-100 text-amber-700", label: "At Risk" },
  breached: { cls: "bg-red-100 text-red-700", label: "Breached" },
};

// uptimeColor color-codes an uptime number against its SLA target.
export function uptimeColor(uptimePct: number, target: number): string {
  if (uptimePct >= target) return "text-green-600";
  if (uptimePct >= target - 0.5) return "text-amber-600";
  return "text-red-600";
}

// fmtMinutes renders a minute count compactly (e.g. 5m, 1h 12m).
export function fmtMinutes(min: number): string {
  if (min <= 0) return "—";
  const m = Math.round(min);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  const rem = m % 60;
  return rem ? `${h}h ${rem}m` : `${h}h`;
}
