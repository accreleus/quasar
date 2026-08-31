// Pure formatting and mapping helpers for the Fleet › Jobs page, kept out of
// JobsTab.tsx so they are unit-testable without rendering.

import type { ChipVariant } from "../../../components/Chip";
import { durationMs } from "../../../lib/format/duration";
import type { JobRunState, JobRunTrigger, JobSchedule } from "../../../api/types";

export const DAY_ABBR = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

/** "HH:MM:SS" -> "HH:MM" (server always sends seconds; the UI never needs them). */
export function trimSeconds(hms: string): string {
  return hms.slice(0, 5);
}

/** `interval_secs` -> a short human duration, always whole units where possible
 *  (mirrors AdminSteamLibrary's `fmtInterval`, generalized to days). */
function fmtDurationUnit(secs: number): string {
  if (secs % 86400 === 0) return `${secs / 86400} d`;
  if (secs % 3600 === 0) return `${secs / 3600} h`;
  if (secs % 60 === 0) return `${secs / 60} min`;
  return `${secs} s`;
}

/**
 * A `JobSchedule` as the Schedule column's one line: "Every 15 min", "Every
 * 6 h, 02:00-06:00", "On event", "Manual only". A locked schedule reads the
 * same; only the lock glyph beside it says an admin cannot edit it.
 */
export function fmtSchedule(schedule: JobSchedule): string {
  if (schedule.kind === "manual") return "Manual only";
  if (schedule.kind === "event") return "On event";
  if (schedule.interval_secs == null) return "—";

  let s = `Every ${fmtDurationUnit(schedule.interval_secs)}`;
  if (schedule.window_start && schedule.window_end) {
    s += `, ${trimSeconds(schedule.window_start)}–${trimSeconds(schedule.window_end)}`;
    if (schedule.window_days.length > 0) {
      s += ` (${schedule.window_days
        .slice()
        .sort((a, b) => a - b)
        .map((d) => DAY_ABBR[d])
        .join(", ")})`;
    }
  }
  return s;
}

/** The run-history table's word for a `JobRunTrigger` (openapi: schedule,
 *  manual, event). Total, so an added trigger renders its own name. */
export function triggerLabel(trigger: JobRunTrigger): string {
  switch (trigger) {
    case "schedule":
      return "Schedule";
    case "manual":
      return "Manual";
    case "event":
      return "Event";
    default:
      return trigger;
  }
}

/** A run's summary blob as one line. The framework never interprets it, so the
 *  runner's own keys are printed as written. */
export function summaryText(summary: Record<string, unknown>): string {
  const entries = Object.entries(summary);
  if (entries.length === 0) return "—";
  return entries
    .map(([k, v]) => `${k} ${typeof v === "object" ? JSON.stringify(v) : String(v)}`)
    .join(" · ");
}

/** Chip variant + label for a job's Result column, keyed off a TERMINAL run
 *  state. `running` is a separate boolean on the job/target and is handled by
 *  the caller before reaching here (design §6.3's Result column table). */
export function resultChip(state: JobRunState): { variant: ChipVariant; label: string } {
  switch (state) {
    case "succeeded":
      return { variant: "success", label: "Succeeded" };
    case "failed":
      return { variant: "danger", label: "Failed" };
    case "deferred":
      return { variant: "warning", label: "Deferred" };
    case "skipped":
      return { variant: "neutral", label: "Skipped" };
    case "aborted":
      return { variant: "danger", label: "Aborted" };
    default:
      // pending/running never appear as a TERMINAL `last_run.state`, but a
      // fallback keeps this total rather than throwing on an unrecognised
      // future value.
      return { variant: "neutral", label: state };
  }
}

/** `duration_ms` -> a short human string; null (not yet finished) -> "—".
 *  Lives in `lib/format/duration` now; re-exported so this page's imports and
 *  its tests keep naming it the way the Jobs table reads. */
export const fmtDurationMs = durationMs;

/**
 * Maps a run-now failure to the honest, specific toast body the spec calls
 * for (job_already_running/job_disabled/job_unmanaged — control-plane
 * internal/httpx/respond.go's wire codes). Falls back to the server's own
 * message for anything else (e.g. a 500) rather than inventing new copy.
 */
export function runNowErrorMessage(code: string, fallback: string): string {
  switch (code) {
    case "job_already_running":
      return "A run for this job is already in progress.";
    case "job_disabled":
      return "This job is disabled. Enable it before running it manually.";
    case "job_unmanaged":
      return "This job is not managed by the job framework, so it cannot be run from here.";
    default:
      return fallback;
  }
}

/**
 * Maps a schedule-PATCH failure to an honest toast body for the two 409 codes
 * the handler can return on top of the generic validation ones (handler.go
 * `handlePatch`).
 */
export function patchErrorMessage(code: string, fallback: string, lockedBy?: string | null): string {
  switch (code) {
    case "schedule_locked":
      return `${lockedBy ?? "An environment variable"} sets this job's interval. Unset it to edit the interval here.`;
    case "job_unmanaged":
      return "This job is not managed by the job framework, so it has no schedule to edit.";
    default:
      return fallback;
  }
}
