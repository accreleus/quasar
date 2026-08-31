import { describe, expect, it } from "vitest";
import {
  elapsedWords,
  relativeTime,
  relativeTimeCompact,
  relativeTimeFuture,
} from "./relativeTime";

const now = new Date("2026-08-28T10:14:00Z");
const ago = (ms: number) => new Date(now.getTime() - ms);
const ahead = (ms: number) => new Date(now.getTime() + ms);

const SEC = 1000;
const MIN = 60 * SEC;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

describe("relativeTime", () => {
  it("counts seconds out loud", () => {
    expect(relativeTime(ago(3 * SEC), now)).toBe("3 seconds ago");
    expect(relativeTime(ago(SEC), now)).toBe("1 second ago");
    expect(relativeTime(now, now)).toBe("just now");
  });

  it("goes terse from a minute up", () => {
    expect(relativeTime(ago(14 * MIN), now)).toBe("14m");
    expect(relativeTime(ago(HOUR + 5 * MIN), now)).toBe("1h 5m");
    expect(relativeTime(ago(3 * HOUR), now)).toBe("3h");
  });

  it("names the days", () => {
    expect(relativeTime(ago(30 * HOUR), now)).toBe("yesterday");
    expect(relativeTime(ago(3 * DAY), now)).toBe("3 days ago");
  });

  it("accepts ISO strings and epoch millis, and refuses a bad instant", () => {
    expect(relativeTime("2026-08-28T10:00:00Z", now)).toBe("14m");
    expect(relativeTime(ago(14 * MIN).getTime(), now)).toBe("14m");
    expect(relativeTime("not a date", now)).toBe("");
  });

  it("does not run the clock backwards for a future instant", () => {
    expect(relativeTime(new Date(now.getTime() + 5 * MIN), now)).toBe("just now");
  });
});

describe("elapsedWords", () => {
  it("spells the unit out for prose", () => {
    expect(elapsedWords(ago(14 * MIN), now)).toBe("14 minutes");
    expect(elapsedWords(ago(MIN), now)).toBe("1 minute");
    expect(elapsedWords(ago(2 * HOUR), now)).toBe("2 hours");
    expect(elapsedWords(ago(3 * DAY), now)).toBe("3 days");
    expect(elapsedWords(ago(9 * SEC), now)).toBe("9 seconds");
    expect(elapsedWords(ago(59.6 * SEC), now)).toBe("1 minute");
  });
});

describe("relativeTimeCompact", () => {
  it("shows one unit, for a narrow column", () => {
    expect(relativeTimeCompact(ago(4 * SEC), now)).toBe("4s");
    expect(relativeTimeCompact(now, now)).toBe("0s");
    expect(relativeTimeCompact(ago(40 * SEC), now)).toBe("40s");
    expect(relativeTimeCompact(ago(14 * MIN), now)).toBe("14m");
    expect(relativeTimeCompact(ago(HOUR + 5 * MIN), now)).toBe("1h");
    expect(relativeTimeCompact(ago(3 * DAY), now)).toBe("3d");
  });

  it("refuses a bad instant and never runs backwards", () => {
    expect(relativeTimeCompact("not a date", now)).toBe("");
    expect(relativeTimeCompact(new Date(now.getTime() + 5 * MIN), now)).toBe("0s");
  });
});

describe("relativeTimeFuture", () => {
  it("counts forward, in the same column shapes", () => {
    expect(relativeTimeFuture(ahead(12 * MIN), now)).toBe("in 12m");
    expect(relativeTimeFuture(ahead(HOUR + 5 * MIN), now)).toBe("in 1h 5m");
    expect(relativeTimeFuture(ahead(3 * HOUR), now)).toBe("in 3h");
    expect(relativeTimeFuture(ahead(DAY), now)).toBe("tomorrow");
    expect(relativeTimeFuture(ahead(3 * DAY), now)).toBe("in 3 days");
  });

  it("calls an instant already gone overdue, not 'just now'", () => {
    expect(relativeTimeFuture(ago(2 * HOUR), now)).toBe("due now");
    expect(relativeTimeFuture(ahead(30 * SEC), now)).toBe("due now");
  });

  it("refuses a bad instant", () => {
    expect(relativeTimeFuture("not a date", now)).toBe("");
  });
});
