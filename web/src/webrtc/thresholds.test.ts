import { describe, expect, it } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import * as T from "./thresholds";

// The golden threshold file is the SPEC; thresholds.ts is a copy of it. This is
// the test that makes that true — the same job the Go drift test does on the
// control-plane half (control-plane/internal/session/verdict_thresholds_test.go).

// Walk up from the vitest cwd rather than trusting import.meta.url: under the
// jsdom environment that is not a file: URL.
function findGolden(): string {
  let dir = process.cwd();
  for (let i = 0; i < 6; i++) {
    const candidate = resolve(dir, "docs/session-trace/thresholds.json");
    if (existsSync(candidate)) return candidate;
    const parent = dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  throw new Error("docs/session-trace/thresholds.json not found from " + process.cwd());
}

const goldenPath = findGolden();

interface GoldenEntry {
  value: number;
  unit: string;
  why: string;
  consumer: string;
}
interface Golden {
  version: string;
  thresholds: Record<string, GoldenEntry>;
}

const golden = JSON.parse(readFileSync(goldenPath, "utf-8")) as Golden;

const PAIRS: Array<[string, number]> = [
  ["web.client_health.present_sd_degraded_ms", T.PRESENT_SD_DEGRADED_MS],
  ["web.client_health.present_p95_degraded_ms", T.PRESENT_P95_DEGRADED_MS],
  ["web.client_health.freeze_degraded_count", T.FREEZE_DEGRADED_COUNT],
  ["web.client_health.decode_budget_fraction", T.DECODE_BUDGET_FRACTION],
  ["web.client_health.decode_abs_ceiling_ms", T.DECODE_ABS_CEILING_MS],

  ["web.present_cadence.min_samples", T.PRESENT_MIN_SAMPLES],
  ["web.present_cadence.doubled_band", T.PRESENT_DOUBLED_BAND],
  ["web.present_cadence.long_frame_factor", T.PRESENT_LONG_FRAME_FACTOR],
  ["web.present_cadence.beat_doubled_max", T.PRESENT_BEAT_DOUBLED_MAX],

  ["web.playout.default_ms", T.DEFAULT_PLAYOUT_MS],
  ["web.playout.floor_ms", T.PLAYOUT_FLOOR_MS],
  ["web.playout.cap_ms", T.PLAYOUT_CAP_MS],
  ["web.playout.step_down_ms", T.STEP_DOWN_MS],
  ["web.playout.step_up_factor", T.STEP_UP_FACTOR],
  ["web.playout.hold_ms", T.HOLD_MS],
  ["web.playout.hold_fast_ms", T.HOLD_FAST_MS],
  ["web.playout.step_down_fast_ms", T.STEP_DOWN_FAST_MS],
  ["web.playout.healthy_sd_ms", T.HEALTHY_SD_MS],
  ["web.playout.degraded_sd_ms", T.DEGRADED_SD_MS],
  ["web.playout.healthy_streak_for_fast", T.HEALTHY_STREAK_FOR_FAST],
  ["web.playout.eval_interval_ms", T.EVAL_INTERVAL_MS],

  ["web.signal.sd_poor_ms", T.SIGNAL_SD_POOR_MS],
  ["web.signal.sd_fair_ms", T.SIGNAL_SD_FAIR_MS],
  ["web.signal.sd_excellent_ms", T.SIGNAL_SD_EXCELLENT_MS],
  ["web.signal.packets_lost_poor", T.SIGNAL_PACKETS_LOST_POOR],
  ["web.signal.packets_lost_fair", T.SIGNAL_PACKETS_LOST_FAIR],

  ["web.clock.repost_interval_s", T.CLOCK_REPOST_INTERVAL_S],
  ["web.clock.repost_delta_ms", T.CLOCK_REPOST_DELTA_MS],
];

describe("thresholds.ts matches docs/session-trace/thresholds.json", () => {
  it("reports the golden file's version", () => {
    expect(T.THRESHOLDS_VERSION).toBe(golden.version);
  });

  it.each(PAIRS)("%s", (name, code) => {
    const entry = golden.thresholds[name];
    expect(entry, `${name} is missing from thresholds.json`).toBeDefined();
    expect(entry.value).toBe(code);
    // A number without a reason is a number nobody can safely change.
    expect(entry.why.length).toBeGreaterThan(0);
    expect(entry.consumer.length).toBeGreaterThan(0);
  });

  it("keeps the glyph and the playout controller on the same σ bands", () => {
    expect(T.SIGNAL_SD_POOR_MS).toBe(T.DEGRADED_SD_MS);
    expect(T.SIGNAL_SD_FAIR_MS).toBe(T.HEALTHY_SD_MS);
  });
});
