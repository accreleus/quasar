import { describe, expect, it } from "vitest";
import { summarizePresentWindow } from "../webrtc/presentCadence";
import { Playout } from "./playout";

const FRAME_MS = 1000 / 120;

/** A fake frame; `closed` says whether the playout gave it up without showing it. */
type FakeFrame = VideoFrame & { closed: boolean };
const fakeFrame = (): FakeFrame => {
  const f = { closed: false, close: () => void (f.closed = true) };
  return f as unknown as FakeFrame;
};

/**
 * Runs a stream through a Playout on a fake clock. `arrivals` are (wall ms, capture ms) pairs in
 * arrival order; returns when each frame was shown. Timers fire in order between arrivals.
 */
function run(arrivals: [number, number][], fps = 120) {
  let clock = 0;
  const timers: { at: number; fn: () => void; id: number }[] = [];
  let nextId = 1;
  const shown: number[] = [];
  const p = new Playout({
    show: () => shown.push(clock),
    now: () => clock,
    setTimer: (fn, ms) => {
      const id = nextId++;
      timers.push({ at: clock + ms, fn, id });
      return id;
    },
    clearTimer: (id) => {
      const i = timers.findIndex((t) => t.id === id);
      if (i >= 0) timers.splice(i, 1);
    },
  });
  p.setFps(fps);
  const fire = (until: number) => {
    for (;;) {
      timers.sort((a, b) => a.at - b.at);
      const t = timers[0];
      if (!t || t.at > until) return;
      timers.shift();
      clock = Math.max(clock, t.at);
      t.fn();
    }
  };
  let maxCushion = 0;
  for (const [at, captureMs] of arrivals) {
    fire(at);
    clock = at;
    p.push(fakeFrame(), captureMs * 1000);
    maxCushion = Math.max(maxCushion, p.cushion());
  }
  fire(Infinity);
  return { shown, p, maxCushion, endClock: () => clock };
}

/** Frames captured every 1/120 s, arriving after a transit time from `transit(i)`, in capture order. */
function stream(n: number, transit: (i: number) => number, from = 0): [number, number][] {
  const out: [number, number][] = [];
  let lastArrival = 0;
  for (let i = 0; i < n; i++) {
    const capture = 1_000_000 + (from + i) * FRAME_MS;
    const arrival = Math.max(lastArrival, capture + transit(from + i));
    out.push([arrival, capture]);
    lastArrival = arrival;
  }
  return out;
}

/** Deterministic noise in [0, 1). */
const noise = (i: number) => {
  const x = Math.sin(i * 12.9898) * 43758.5453;
  return x - Math.floor(x);
};

/** One frame in six is held up 10 to 20 ms: a bumpy link. */
const bumpy = (i: number) => 20 + noise(i) * 2 + (noise(i + 7919) < 1 / 6 ? 10 + noise(i + 104729) * 10 : 0);

const intervals = (times: number[]) => times.slice(1).map((t, i) => t - times[i]);
const longFrames = (times: number[]) => summarizePresentWindow(intervals(times), null, 120).longFrames ?? 0;

describe("Playout", () => {
  it("shows every frame the moment it arrives on a clean link", () => {
    const a = stream(600, (i) => 20 + noise(i) * 0.3);
    const { shown, maxCushion } = run(a);
    expect(maxCushion).toBe(0);
    expect(shown).toEqual(a.map(([at]) => at));
  });

  it("gives a bumpy link fewer long frames than showing frames as they arrive, within a bound", () => {
    const a = stream(600, bumpy);
    const asArrived = a.map(([at]) => at);
    const { shown, maxCushion } = run(a);
    expect(longFrames(asArrived)).toBeGreaterThan(10);
    expect(longFrames(shown)).toBeLessThan(longFrames(asArrived) / 4);
    expect(maxCushion).toBeGreaterThan(5);
    expect(maxCushion).toBeLessThanOrEqual(3 * FRAME_MS + 1e-9);
  });

  it("bounds the cushion by 3 frame intervals, and by 50 ms at a low frame rate", () => {
    const hold = (i: number) => (i % 2 ? 80 : 20);
    expect(run(stream(300, hold), 120).maxCushion).toBeLessThanOrEqual(25 + 1e-9);
    expect(run(stream(300, hold), 30).maxCushion).toBeLessThanOrEqual(50 + 1e-9);
  });

  it("shrinks back to nothing once the link is clean again", () => {
    const a = stream(300, bumpy);
    const b = stream(900, (i) => 20 + noise(i) * 0.3, 300);
    const first = run(a);
    expect(first.p.cushion()).toBeGreaterThan(0);
    const both = run([...a, ...b]);
    expect(both.p.cushion()).toBe(0);
    expect(both.p.arrivalJitterMs()).toBeLessThan(0.5);
  });

  it("closes every frame it did not show, on reset and on close", () => {
    let clock = 0;
    const shown = new Set<unknown>();
    // Timers never fire: whatever is not due when it arrives stays queued.
    const p = new Playout({ show: (f) => shown.add(f), now: () => clock, setTimer: () => 1, clearTimer: () => {} });
    p.setFps(120);
    const frames = stream(120, bumpy).map(([at, captureMs]) => {
      clock = at;
      const f = fakeFrame();
      p.push(f, captureMs * 1000);
      return f;
    });
    const held = frames.filter((f) => !shown.has(f) && !f.closed);
    expect(held.length).toBeGreaterThan(0);
    p.reset();
    expect(held.every((f) => f.closed)).toBe(true);
    expect(p.cushion()).toBe(0);
    p.close();
  });
});
