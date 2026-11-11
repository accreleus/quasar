import { describe, expect, it } from "vitest";
import { summarizePresentWindow } from "../webrtc/presentCadence";
import { Playout } from "./playout";

const FRAME_MS = 1000 / 120;

/** A fake frame; `closes` counts the playout giving it up without showing it, `shows` its being shown. */
type FakeFrame = VideoFrame & { closed: boolean; closes: number; shows: number; shownAt: number };
const fakeFrame = (): FakeFrame => {
  const f = { closed: false, closes: 0, shows: 0, shownAt: NaN, close: () => void ((f.closed = true), f.closes++) };
  return f as unknown as FakeFrame;
};
/** Every frame was shown exactly once or closed exactly once, never both and never neither. */
const exactlyOnce = (frames: FakeFrame[]) =>
  expect(frames.filter((f) => f.shows + f.closes !== 1 || f.shows > 1 || f.closes > 1)).toHaveLength(0);

/**
 * Runs a stream through a Playout on a fake clock. `arrivals` are (wall ms, capture ms) pairs in
 * arrival order; returns when each frame was shown. Timers fire in order between arrivals.
 */
function run(arrivals: [number, number][], fps = 120) {
  let clock = 0;
  const timers: { at: number; fn: () => void; id: number }[] = [];
  let nextId = 1;
  const shown: number[] = [];
  const frames: FakeFrame[] = [];
  const p = new Playout({
    show: (f) => {
      (f as FakeFrame).shows++;
      (f as FakeFrame).shownAt = clock;
      shown.push(clock);
    },
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
    const f = fakeFrame();
    frames.push(f);
    p.push(f, captureMs * 1000);
    maxCushion = Math.max(maxCushion, p.cushion());
  }
  fire(Infinity);
  return { shown, frames, p, maxCushion, endClock: () => clock };
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

  it("uses no timer on a clean link with ordinary sub-millisecond noise", () => {
    let clock = 0;
    let timers = 0;
    let shown = 0;
    const p = new Playout({ show: () => shown++, now: () => clock, setTimer: () => ++timers, clearTimer: () => {} });
    p.setFps(120);
    for (let i = 0; i < 600; i++) {
      // The fastest frame first, so every later one is a little after its due time's base.
      clock = 20 + i * FRAME_MS + (i === 0 ? 0 : 0.2 + 0.3 * Math.sin(i));
      p.push(fakeFrame(), i * FRAME_MS * 1000);
    }
    expect(shown).toBe(600);
    expect(timers).toBe(0);
    expect(p.cushion()).toBe(0);
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

  it("shows every frame once or closes it once, on a bumpy link and on a bound-hit one", () => {
    exactlyOnce(run(stream(600, bumpy)).frames);
    exactlyOnce(run(stream(300, (i) => (i % 2 ? 80 : 20)), 30).frames);
  });

  it("gets fewer long frames again within the window after a delay that rose and stayed up", () => {
    // 2 s clean at 20 ms, then the path delay is 120 ms for good, and bumpy.
    const a = stream(1100, (i) => (i < 240 ? 20 + noise(i) * 0.3 : 100 + bumpy(i)));
    const { shown, frames } = run(a);
    exactlyOnce(frames);
    const cut = a[800][0]; // 560 frames (4.7 s) after the step: well past the base window
    const tail = (times: number[]) => times.filter((t) => t >= cut);
    const asArrived = a.map(([at]) => at);
    expect(longFrames(tail(asArrived))).toBeGreaterThan(8);
    expect(longFrames(tail(shown))).toBeLessThan(longFrames(tail(asArrived)) / 4);
  });

  it("follows a delay that fell at once: the old, higher base is not held", () => {
    const a = stream(900, (i) => (i < 300 ? 120 + noise(i) * 0.3 : 20 + noise(i) * 0.3));
    const { frames } = run(a);
    exactlyOnce(frames);
    // Past the frames bunched up by the step, a shown frame is at most the new path delay plus the
    // cushion bound old, never the old 120 ms.
    for (let i = 330; i < 900; i++) {
      if (frames[i].shows) expect(frames[i].shownAt - a[i][1]).toBeLessThanOrEqual(20.3 + 3 * FRAME_MS + 1e-6);
    }
    expect(frames.slice(330).filter((f) => f.shows).length).toBeGreaterThan(300);
  });

  it("drains a burst after a 300 ms outage at once and leaves no standing latency", () => {
    // Frames 300 to 335 are held by the outage and arrive together; the rest are on time.
    const a = stream(1500, (i) => (i >= 300 && i < 336 ? 20 + (336 - i) * FRAME_MS : 20 + noise(i) * 0.3));
    const { frames } = run(a);
    exactlyOnce(frames);
    // The burst is drained, not dropped: each frame is shown. Those already overdue on arrival are
    // shown at that instant (the presenter keeps the last), the newest few when their cushion is up.
    const at = a[335][0];
    expect(frames.slice(300, 336).every((f) => f.shows === 1)).toBe(true);
    expect(frames.slice(300, 320).every((f) => f.shownAt === at)).toBe(true);
    expect(frames.slice(300, 336).every((f) => f.shownAt - at <= 3 * FRAME_MS + 1e-6)).toBe(true);
    // After it, the delay is the path's plus a cushion no larger than its bound, and shrinks to nothing.
    for (let i = 336; i < 1500; i++) expect(frames[i].shownAt - a[i][1]).toBeLessThanOrEqual(20.3 + 3 * FRAME_MS + 1e-6);
    expect(frames[1499].shownAt - a[1499][1]).toBeLessThan(21);
  });

  it("counts a frame superseded by a later due one as closed, not shown", () => {
    let clock = 0;
    const p = new Playout({ show: (f) => void (f as FakeFrame).shows++, now: () => clock, setTimer: () => 1, clearTimer: () => {} });
    p.setFps(120);
    const fs: FakeFrame[] = [];
    // Teach it a bumpy link so a cushion is in force, then push two frames that fall due together.
    for (const [at, c] of stream(120, bumpy)) {
      clock = at;
      const f = fakeFrame();
      fs.push(f);
      p.push(f, c * 1000);
    }
    exactlyOnce(fs.filter((f) => f.shows + f.closes > 0));
    const a = fakeFrame();
    const b = fakeFrame();
    clock += 1;
    p.push(a, (clock - 20) * 1000 + 1_000); // not yet due
    p.push(b, (clock - 20) * 1000 + 2_000); // due a ms later than a
    clock += 100; // both overdue, the timer being the thing that never fired
    const c = fakeFrame();
    p.push(c, (clock - 20) * 1000);
    expect([a.shows, a.closes, b.shows, b.closes]).toEqual([0, 1, 1, 0]);
    expect([c.shows, c.closes]).toEqual([0, 0]); // not due yet
    p.close();
    exactlyOnce([a, b, c]);
  });

  it("closes the oldest when more are waiting than it holds, and every one still waiting on close", () => {
    let clock = 0;
    const p = new Playout({ show: (f) => void (f as FakeFrame).shows++, now: () => clock, setTimer: () => 1, clearTimer: () => {} });
    p.setFps(120);
    for (const [at, c] of stream(120, bumpy)) {
      clock = at;
      p.push(fakeFrame(), c * 1000);
    }
    expect(p.cushion()).toBeGreaterThan(1);
    // Twenty frames all captured "now", so each is due a cushion from now: none is.
    const held = Array.from({ length: 20 }, () => {
      const f = fakeFrame();
      p.push(f, (clock - 20) * 1000);
      return f;
    });
    expect(held.filter((f) => f.closes === 1)).toHaveLength(14);
    expect(held.slice(14).every((f) => f.closes === 0 && f.shows === 0)).toBe(true);
    p.close();
    exactlyOnce(held);
    expect(held.every((f) => f.closes === 1 && f.shows === 0)).toBe(true);
  });

  it("reports no arrival jitter until two frames have been measured, and again after a reset", () => {
    const p = new Playout({ show: () => {}, now: () => 0 });
    expect(p.arrivalJitterMs()).toBeNull();
    p.push(fakeFrame(), 0);
    expect(p.arrivalJitterMs()).toBeNull();
    p.push(fakeFrame(), 1000);
    expect(p.arrivalJitterMs()).not.toBeNull();
    p.reset();
    expect(p.arrivalJitterMs()).toBeNull();
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
