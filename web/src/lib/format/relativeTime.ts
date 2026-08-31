/**
 * An instant, rendered as its age (spec §A.1: every alert row and every
 * "updated N ago" line).
 *
 * Registers, because the console uses each:
 *   · `relativeTime` — the column/label form. Seconds are spelled out because
 *     "3s" beside a fault reads like a measurement rather than a freshness
 *     note; from a minute up it goes terse ("14m", "1h 5m", "3h") because it
 *     sits in a narrow right-aligned column; past a day it goes back to words.
 *   · `elapsedWords` — the same age inside a sentence ("No heartbeat for 14
 *     minutes"), where "14m" would read as an abbreviation mid-prose.
 *   · `relativeTimeFuture` — the same column form for an instant still ahead
 *     ("in 12m"), which the past-only forms would flatten to "just now".
 *
 * `now` is a parameter, never `Date.now()` reached for internally, so callers
 * that already hold the poll's timestamp render one consistent set of ages and
 * tests need no clock control.
 */

export type Instant = string | number | Date;

const SECOND = 1000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

function toMs(instant: Instant): number {
  if (instant instanceof Date) return instant.getTime();
  if (typeof instant === "number") return instant;
  return new Date(instant).getTime();
}

/** Milliseconds between the two, clamped at zero: a timestamp from the future
 *  is clock skew, and negative ages are not a thing this UI shows. */
function ageMs(past: Instant, now: Instant): number | null {
  const then = toMs(past);
  if (!Number.isFinite(then)) return null;
  return Math.max(0, toMs(now) - then);
}

/** "3 seconds ago" / "14m" / "1h 5m" / "3h" / "yesterday" / "3 days ago".
 *  An unparseable instant renders as "" — there is nothing honest to say. */
export function relativeTime(past: Instant, now: Instant = Date.now()): string {
  const age = ageMs(past, now);
  if (age === null) return "";
  if (age < SECOND) return "just now";
  if (age < MINUTE) return plural(Math.floor(age / SECOND), "second") + " ago";
  if (age < HOUR) return `${Math.floor(age / MINUTE)}m`;
  if (age < DAY) {
    const hours = Math.floor(age / HOUR);
    const mins = Math.floor((age % HOUR) / MINUTE);
    return mins ? `${hours}h ${mins}m` : `${hours}h`;
  }
  const days = Math.floor(age / DAY);
  return days === 1 ? "yesterday" : `${days} days ago`;
}

/** One unit, no words: "40s" / "14m" / "3h" / "2d", for a narrow right-aligned
 *  column (mock §A.4's Seen, §A.1's Heartbeat). `relativeTime` spells seconds
 *  out and pairs h with m, both of which widen the column. */
export function relativeTimeCompact(past: Instant, now: Instant = Date.now()): string {
  const age = ageMs(past, now);
  if (age === null) return "";
  if (age < MINUTE) return `${Math.floor(age / SECOND)}s`;
  if (age < HOUR) return `${Math.floor(age / MINUTE)}m`;
  if (age < DAY) return `${Math.floor(age / HOUR)}h`;
  return `${Math.floor(age / DAY)}d`;
}

/** Milliseconds until `future`, clamped at zero: an instant that has already
 *  passed is an overdue schedule, not a negative wait. */
function untilMs(future: Instant, now: Instant): number | null {
  const then = toMs(future);
  if (!Number.isFinite(then)) return null;
  return Math.max(0, then - toMs(now));
}

/**
 * The mirror of `relativeTime` for an instant that has NOT happened yet: "in
 * 12m", "in 1h 5m", "tomorrow". A scheduled run put through the past-only
 * formatter reads "just now" forever, which is why this exists.
 *
 * An instant already gone reads "due now" — the schedule is late, and that is
 * the honest answer for a job whose next run is behind the clock.
 */
export function relativeTimeFuture(future: Instant, now: Instant = Date.now()): string {
  const wait = untilMs(future, now);
  if (wait === null) return "";
  if (wait < MINUTE) return "due now";
  if (wait < HOUR) return `in ${Math.floor(wait / MINUTE)}m`;
  if (wait < DAY) {
    const hours = Math.floor(wait / HOUR);
    const mins = Math.floor((wait % HOUR) / MINUTE);
    return mins ? `in ${hours}h ${mins}m` : `in ${hours}h`;
  }
  const days = Math.floor(wait / DAY);
  return days === 1 ? "tomorrow" : `in ${days} days`;
}

/** The same age as a noun phrase, for prose: "14 minutes", "2 hours". Rounds
 *  each unit before choosing the next, so 59.6 s reads "1 minute". */
export function elapsedWords(past: Instant, now: Instant = Date.now()): string {
  const age = ageMs(past, now);
  if (age === null) return "";
  const secs = Math.round(age / SECOND);
  if (secs < 60) return plural(secs, "second");
  const mins = Math.round(secs / 60);
  if (mins < 60) return plural(mins, "minute");
  const hours = Math.round(mins / 60);
  if (hours < 24) return plural(hours, "hour");
  return plural(Math.round(hours / 24), "day");
}

function plural(n: number, unit: string): string {
  return `${n} ${unit}${n === 1 ? "" : "s"}`;
}
