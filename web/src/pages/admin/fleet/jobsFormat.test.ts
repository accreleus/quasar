import { describe, expect, it } from "vitest";
import {
  fmtDurationMs,
  fmtSchedule,
  patchErrorMessage,
  resultChip,
  runNowErrorMessage,
  summaryText,
  triggerLabel,
  trimSeconds,
} from "./jobsFormat";
import type { JobSchedule } from "../../../api/types";

function schedule(over: Partial<JobSchedule> = {}): JobSchedule {
  return {
    kind: "interval",
    interval_secs: 900,
    window_start: null,
    window_end: null,
    window_days: [],
    timezone: "UTC",
    locked: false,
    locked_by: null,
    ...over,
  };
}

describe("trimSeconds", () => {
  it("drops the seconds component", () => {
    expect(trimSeconds("02:00:00")).toBe("02:00");
  });
});

describe("fmtSchedule", () => {
  it("renders manual-only", () => {
    expect(fmtSchedule(schedule({ kind: "manual", interval_secs: null }))).toBe("Manual only");
  });

  it("renders event-triggered", () => {
    expect(fmtSchedule(schedule({ kind: "event", interval_secs: null }))).toBe("On event");
  });

  it("reads as no value when an interval schedule carries no interval", () => {
    expect(fmtSchedule(schedule({ interval_secs: null }))).toBe("—");
  });

  it("renders a bare interval in minutes", () => {
    expect(fmtSchedule(schedule({ interval_secs: 900 }))).toBe("Every 15 min");
  });

  it("renders a bare interval in hours", () => {
    expect(fmtSchedule(schedule({ interval_secs: 21600 }))).toBe("Every 6 h");
  });

  it("renders a bare interval in days", () => {
    expect(fmtSchedule(schedule({ interval_secs: 86400 }))).toBe("Every 1 d");
  });

  it("falls back to seconds for a non-round interval", () => {
    expect(fmtSchedule(schedule({ interval_secs: 90 }))).toBe("Every 90 s");
  });

  it("appends a window", () => {
    expect(
      fmtSchedule(
        schedule({ interval_secs: 86400, window_start: "02:00:00", window_end: "06:00:00" }),
      ),
    ).toBe("Every 1 d, 02:00–06:00");
  });

  it("appends window days, sorted, when constrained", () => {
    expect(
      fmtSchedule(
        schedule({
          interval_secs: 86400,
          window_start: "22:00:00",
          window_end: "04:00:00",
          window_days: [6, 5],
        }),
      ),
    ).toBe("Every 1 d, 22:00–04:00 (Fri, Sat)");
  });

  it("omits the day list when window_days is empty (every day)", () => {
    expect(
      fmtSchedule(schedule({ interval_secs: 3600, window_start: "02:00:00", window_end: "06:00:00" })),
    ).toBe("Every 1 h, 02:00–06:00");
  });
});

describe("resultChip", () => {
  it("maps every terminal state to a distinct variant", () => {
    expect(resultChip("succeeded")).toEqual({ variant: "success", label: "Succeeded" });
    expect(resultChip("failed")).toEqual({ variant: "danger", label: "Failed" });
    expect(resultChip("deferred")).toEqual({ variant: "warning", label: "Deferred" });
    expect(resultChip("skipped")).toEqual({ variant: "neutral", label: "Skipped" });
    expect(resultChip("aborted")).toEqual({ variant: "danger", label: "Aborted" });
  });
});

describe("fmtDurationMs", () => {
  it("renders null as an em dash", () => {
    expect(fmtDurationMs(null)).toBe("—");
  });

  it("renders sub-second durations in ms", () => {
    expect(fmtDurationMs(188)).toBe("188 ms");
  });

  it("renders sub-minute durations in seconds", () => {
    expect(fmtDurationMs(1188)).toBe("1.2 s");
  });

  it("renders multi-minute durations as m/s", () => {
    expect(fmtDurationMs(125_000)).toBe("2m 5s");
  });
});

describe("runNowErrorMessage", () => {
  it("maps job_already_running", () => {
    expect(runNowErrorMessage("job_already_running", "fallback")).toMatch(/already in progress/);
  });

  it("maps job_disabled", () => {
    expect(runNowErrorMessage("job_disabled", "fallback")).toMatch(/disabled/);
  });

  it("maps job_unmanaged", () => {
    expect(runNowErrorMessage("job_unmanaged", "fallback")).toMatch(/not managed by the job framework/);
  });

  it("falls back to the server message for anything else", () => {
    expect(runNowErrorMessage("internal", "server exploded")).toBe("server exploded");
  });
});

describe("triggerLabel", () => {
  it("names each trigger the contract enumerates", () => {
    expect(triggerLabel("schedule")).toBe("Schedule");
    expect(triggerLabel("manual")).toBe("Manual");
    expect(triggerLabel("event")).toBe("Event");
  });
});

describe("summaryText", () => {
  it("joins the runner's own keys without connector colons", () => {
    expect(summaryText({ apps_considered: 412, artwork_resolved: 3 })).toBe(
      "apps_considered 412 · artwork_resolved 3",
    );
  });

  it("reads as no value when the run reported nothing", () => {
    expect(summaryText({})).toBe("—");
  });
});

describe("patchErrorMessage", () => {
  it("names the source that pins the interval", () => {
    expect(patchErrorMessage("schedule_locked", "fallback", "QUASAR_LIBRARY_SCAN_INTERVAL")).toBe(
      "QUASAR_LIBRARY_SCAN_INTERVAL sets this job's interval. Unset it to edit the interval here.",
    );
  });

  it("maps schedule_locked without a source", () => {
    expect(patchErrorMessage("schedule_locked", "fallback")).toMatch(/environment variable/);
  });

  it("maps job_unmanaged", () => {
    expect(patchErrorMessage("job_unmanaged", "fallback")).toMatch(/not managed by the job framework/);
  });

  it("falls back to the server message for anything else", () => {
    expect(patchErrorMessage("validation_failed", "bad input")).toBe("bad input");
  });
});
