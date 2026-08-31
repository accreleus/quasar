import { describe, expect, it } from "vitest";
import { hostLabel, rollupTargets, type DerivedTarget } from "./jobsDerived";

function target(over: Partial<DerivedTarget> = {}): DerivedTarget {
  return {
    host_id: "b7c1e0f2-0000-0000-0000-000000000000",
    node_name: "tower",
    running: false,
    next_run_at: null,
    last_run: null,
    ...over,
  };
}

function lastRun(finished: string | null, over: Partial<DerivedTarget["last_run"] & object> = {}) {
  return {
    state: "succeeded" as const,
    finished_at: finished,
    duration_ms: 1188,
    error: null,
    ...over,
  };
}

describe("hostLabel", () => {
  it("names the host by its node name", () => {
    expect(hostLabel({ host_id: "b7c1e0f2-dead", node_name: "tower" })).toBe("tower");
  });

  it("falls back to the short id when the name did not resolve", () => {
    expect(hostLabel({ host_id: "b7c1e0f2-dead", node_name: "" })).toBe("b7c1e0f2");
  });
});

describe("rollupTargets", () => {
  it("reports nothing for a job with no targets", () => {
    expect(rollupTargets(undefined)).toEqual({
      targetCount: 0,
      runningOn: null,
      lastRun: null,
      nextRun: null,
    });
    expect(rollupTargets([])).toEqual({
      targetCount: 0,
      runningOn: null,
      lastRun: null,
      nextRun: null,
    });
  });

  it("counts its targets", () => {
    expect(rollupTargets([target(), target({ host_id: "x", node_name: "hermes" })]).targetCount).toBe(2);
  });

  it("takes the most recent finished run and names the host it ran on", () => {
    const roll = rollupTargets([
      target({ node_name: "tower", last_run: lastRun("2026-08-12T02:00:11Z") }),
      target({ node_name: "hermes", last_run: lastRun("2026-08-12T05:30:00Z", { duration_ms: 41 }) }),
    ]);
    expect(roll.lastRun?.host).toBe("hermes");
    expect(roll.lastRun?.run.duration_ms).toBe(41);
  });

  it("ignores a run that has not finished, and a target that has never run", () => {
    const roll = rollupTargets([
      target({ node_name: "tower", last_run: lastRun(null) }),
      target({ node_name: "hermes", last_run: null }),
      target({ node_name: "aux", last_run: lastRun("2026-08-12T05:30:00Z") }),
    ]);
    expect(roll.lastRun?.host).toBe("aux");
  });

  it("ignores an unparseable finished_at rather than ranking it first", () => {
    const roll = rollupTargets([
      target({ node_name: "tower", last_run: lastRun("not a date") }),
      target({ node_name: "hermes", last_run: lastRun("2026-08-12T05:30:00Z") }),
    ]);
    expect(roll.lastRun?.host).toBe("hermes");
  });

  it("has no last run when no target has finished one", () => {
    expect(rollupTargets([target({ last_run: null })]).lastRun).toBeNull();
  });

  it("names the first host that is running", () => {
    const roll = rollupTargets([
      target({ node_name: "tower" }),
      target({ node_name: "hermes", running: true }),
      target({ node_name: "aux", running: true }),
    ]);
    expect(roll.runningOn).toBe("hermes");
  });

  it("reports no running host when every target is idle", () => {
    expect(rollupTargets([target(), target()]).runningOn).toBeNull();
  });

  it("takes the earliest queued next run and names its host", () => {
    const roll = rollupTargets([
      target({ node_name: "tower", next_run_at: "2026-08-12T06:00:00Z" }),
      target({ node_name: "hermes", next_run_at: "2026-08-12T03:00:00Z" }),
      target({ node_name: "aux", next_run_at: null }),
    ]);
    expect(roll.nextRun).toEqual({ at: "2026-08-12T03:00:00Z", host: "hermes" });
  });

  it("has no next run when nothing is queued on any host", () => {
    expect(rollupTargets([target({ next_run_at: null })]).nextRun).toBeNull();
  });
});
