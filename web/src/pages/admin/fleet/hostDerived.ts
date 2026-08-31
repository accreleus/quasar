/**
 * What a host's row and detail page say, as pure functions off the two
 * payloads the console already holds. Whether a host needs attention stays in
 * `lib/fleet/deriveAlerts`, so every surface reads one predicate.
 */

import { needsAttention, type AttentionHost } from "../../../lib/fleet/deriveAlerts";

// ── Inputs ───────────────────────────────────────────────────────────────────

// Inputs are structural rather than the generated API types, like
// `lib/fleet/deriveAlerts`: testable from a three-line fixture, and `status`
// is an open vocabulary a consumer passes through rather than rejects. `now`
// is always a parameter, so one poll renders one consistent set of ages.

export interface UtilisationStorage {
  total_mb: number;
  available_mb: number;
}

export interface UtilisationGpu {
  vram_mb_total: number;
  vram_mb_used: number | null;
  slots_total: number;
  slots_reserved: number;
}

export interface UtilisationHost {
  /** The v3 read-time roll-up. Null means "no schedulable GPUs reported",
   *  never "zero capacity" — so the GPU list is consulted instead. */
  capacity?: {
    slots_total: number;
    slots_used: number;
    vram_mb_total: number;
    vram_mb_used: number;
  } | null;
  storage?: readonly UtilisationStorage[] | null;
}

export interface HostLike extends AttentionHost {
  node_name?: string;
}

// ── Output ───────────────────────────────────────────────────────────────────

export interface HostUtilisation {
  /** Encode slots held vs reported. Null when nothing has reported any. */
  gpu: { used: number; total: number } | null;
  /** Live VRAM, in MiB. Null when no GPU has been sampled. */
  vram: { usedMb: number; totalMb: number } | null;
  /**
   * Always null. Host memory use is not on the wire (no agent-api field), so
   * the column says "n/a" rather than drawing a 0 % bar (spec §9). Typed as
   * `null` on purpose: a future agent field changes this type and every caller
   * is then forced to handle the number.
   */
  ramPct: null;
  /** Percent of reported storage used, summed over volumes. Null when the
   *  agent has reported no volumes. */
  diskPct: number | null;
}

/** The mock's `tone(p)`: danger at 90 %, warning at 75 %. */
export type Tone = "success" | "warning" | "danger";

export function tone(percent: number): Tone {
  if (percent >= 90) return "danger";
  if (percent >= 75) return "warning";
  return "success";
}

/** The mock's `toneC(p)` — the same three steps as a colour, for a gauge arc. */
export function toneColor(percent: number): string {
  if (percent >= 90) return "var(--danger)";
  if (percent >= 75) return "var(--warning)";
  return "var(--accent)";
}

export function percentOf(used: number, total: number): number {
  if (!total) return 0;
  return Math.round((used / total) * 100);
}

/**
 * The 2×2 utilisation block on a host row, and the gauges on host detail.
 *
 * `capacity` wins when the server sent one: it is derived at read time from
 * the same GPUs, so it cannot drift, and it is there for every host in one
 * request. The GPU list is the fallback for a host detail page that has the
 * per-GPU payload anyway, and for any host whose roll-up is absent.
 */
export function utilisation(
  host: UtilisationHost,
  gpus?: readonly UtilisationGpu[] | null,
): HostUtilisation {
  const capacity = host.capacity ?? null;
  const list = gpus ?? [];

  let gpu: HostUtilisation["gpu"] = null;
  let vram: HostUtilisation["vram"] = null;

  if (capacity) {
    gpu = { used: capacity.slots_used, total: capacity.slots_total };
    vram = { usedMb: capacity.vram_mb_used, totalMb: capacity.vram_mb_total };
  } else if (list.length > 0) {
    gpu = {
      used: list.reduce((sum, g) => sum + g.slots_reserved, 0),
      total: list.reduce((sum, g) => sum + g.slots_total, 0),
    };
    vram = {
      // A GPU that has never been sampled contributes nothing to either side,
      // so an unknown GPU is never counted as an idle one.
      usedMb: list.reduce((sum, g) => sum + (g.vram_mb_used ?? 0), 0),
      totalMb: list.reduce((sum, g) => (g.vram_mb_used == null ? sum : sum + g.vram_mb_total), 0),
    };
    if (vram.totalMb === 0) vram = null;
  }

  return { gpu, vram, ramPct: null, diskPct: storageUsedPercent(host.storage) };
}

/** Used and total across every volume the agent reported. Null when it has
 *  reported none — an empty disk bar would read as "empty", not "unknown". */
export function storageTotals(
  storage?: readonly UtilisationStorage[] | null,
): { usedMb: number; totalMb: number } | null {
  if (!storage || storage.length === 0) return null;
  const totalMb = storage.reduce((sum, v) => sum + v.total_mb, 0);
  if (totalMb === 0) return null;
  const availableMb = storage.reduce((sum, v) => sum + v.available_mb, 0);
  return { usedMb: totalMb - availableMb, totalMb };
}

export function storageUsedPercent(storage?: readonly UtilisationStorage[] | null): number | null {
  const totals = storageTotals(storage);
  return totals ? percentOf(totals.usedMb, totals.totalMb) : null;
}

/**
 * The colour of the "Seen" column, off the state the row shows — so a host
 * that is online but degraded reads warning rather than a healthy green. Only
 * a host that has stopped talking reads danger; a drain is deliberate, and so
 * is warning.
 */
export function heartbeatTone(host: HostLike): Tone {
  const state = hostStateLabel(host);
  if (state === "online") return "success";
  if (state === "offline") return "danger";
  return "warning";
}

/**
 * How long the agent process has been connected ("18d 4h", "1h 30m", "47m").
 *
 * Host uptime is not on the wire and this is not it: `agent_connected_since`
 * is derived control-plane side from WebSocket connect timing (#429) and does
 * not reset on a network blip, only on a genuine agent restart. "n/a" when the
 * control plane has never seen this agent connect — never a fabricated zero.
 */
export function uptimeSince(iso: string | null | undefined, now: number = Date.now()): string {
  if (!iso) return "n/a";
  const then = Date.parse(iso);
  if (!Number.isFinite(then)) return "n/a";
  const seconds = Math.max(0, Math.round((now - then) / 1000));
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m`;
  return `${seconds}s`;
}

/** What the host is doing with new sessions, in the fact table's words. */
export function schedulingLabel(host: { status: string }): string {
  return host.status === "online" ? "accepting sessions" : "paused";
}

/**
 * The word the row and the detail head use for this host's state.
 *
 * `status` alone would call a host with a failed capacity report or a failed
 * readiness check "online" — the row an operator most needs to spot. Same
 * predicate as the rail badge (`needsAttention`), so they cannot disagree.
 */
export function hostStateLabel(host: HostLike): string {
  return host.status === "online" && needsAttention(host) ? "degraded" : host.status;
}

/** `.sdot` modifier for that state (mock: ok / warn / bad / off). */
export function hostStateDot(host: HostLike): string {
  const state = hostStateLabel(host);
  if (state === "online") return "ok";
  if (state === "draining") return "warn";
  if (state === "offline" || state === "degraded") return "bad";
  return "off";
}

/** `.chip-*` modifier for that state. */
export function hostStateChip(host: HostLike): "success" | "warning" | "danger" | "neutral" {
  const state = hostStateLabel(host);
  if (state === "online") return "success";
  if (state === "draining") return "warning";
  if (state === "offline" || state === "degraded") return "danger";
  return "neutral";
}
