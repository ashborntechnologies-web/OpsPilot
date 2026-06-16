// Shared incident presentation helpers used by the incident list and war room.

export const SEVERITY_BADGE: Record<string, string> = {
  error: "bg-red-100 text-red-700",
  warn: "bg-amber-100 text-amber-700",
  info: "bg-zinc-100 text-zinc-600",
};

export const STATUS_BADGE: Record<string, string> = {
  open: "bg-red-100 text-red-700",
  investigating: "bg-amber-100 text-amber-700",
  resolved: "bg-green-100 text-green-700",
};

// timeOpen renders the elapsed time from `from` to `to` (or now) compactly.
export function timeOpen(from: string, to?: string | null): string {
  const end = to ? new Date(to).getTime() : Date.now();
  const s = Math.floor((end - new Date(from).getTime()) / 1000);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d`;
}
