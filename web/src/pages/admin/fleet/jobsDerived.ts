/**
 * One row's worth of state for a host-scoped job, rolled up from its targets.
 * openapi.yaml `Job`: a managed host-scoped job carries `targets` instead of
 * the top-level run fields, so the single row it occupies has to derive them.
 */

import type { JobRunState } from "../../../api/types";
import { shortId } from "../../../lib/format/shortId";

export interface DerivedRun {
  state: JobRunState;
  finished_at: string | null;
  duration_ms: number | null;
  error: string | null;
}

export interface DerivedTarget {
  host_id: string;
  node_name: string;
  running: boolean;
  next_run_at: string | null;
  last_run: DerivedRun | null;
}

export interface HostRollup {
  targetCount: number;
  /** The first host with a run in flight, or null when every target is idle. */
  runningOn: string | null;
  /** The newest finished run across the targets, and where it ran. */
  lastRun: { run: DerivedRun; host: string; finishedAt: string } | null;
  /** The soonest queued run across the targets, and where it is queued. */
  nextRun: { at: string; host: string } | null;
}

/** A stale host row sends `node_name: ""` rather than failing the list, so the
 *  id is the fallback label. */
export function hostLabel(target: { host_id: string; node_name: string }): string {
  return target.node_name || shortId(target.host_id);
}

function instant(iso: string | null): number | null {
  if (!iso) return null;
  const ms = Date.parse(iso);
  return Number.isFinite(ms) ? ms : null;
}

export function rollupTargets(targets?: readonly DerivedTarget[] | null): HostRollup {
  const list = targets ?? [];

  let runningOn: string | null = null;
  let lastRun: HostRollup["lastRun"] = null;
  let lastAt = -Infinity;
  let nextRun: HostRollup["nextRun"] = null;
  let nextAt = Infinity;

  for (const target of list) {
    if (target.running && runningOn === null) runningOn = hostLabel(target);

    const finishedAt = target.last_run?.finished_at ?? null;
    const finished = instant(finishedAt);
    if (target.last_run && finishedAt && finished !== null && finished > lastAt) {
      lastAt = finished;
      lastRun = { run: target.last_run, host: hostLabel(target), finishedAt };
    }

    const queued = instant(target.next_run_at);
    if (queued !== null && target.next_run_at && queued < nextAt) {
      nextAt = queued;
      nextRun = { at: target.next_run_at, host: hostLabel(target) };
    }
  }

  return { targetCount: list.length, runningOn, lastRun, nextRun };
}
