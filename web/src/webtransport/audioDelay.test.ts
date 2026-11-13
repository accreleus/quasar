import { describe, expect, it, vi } from "vitest";
import { AudioRing, MAX_QUEUED_FRAMES, STRETCH_DONE_FRAMES } from "./audioRing";
import { AudioSync } from "./audioSync";

/** Same 60 s packet and video arrivals for the old and new schedulers; no packet loss. */
function jitterStream() {
  const sync = new AudioSync();
  const ring = new AudioRing();
  const buf = [new Float32Array(128)];
  let packet = 0;
  let moves = 0;
  let previous = 0;
  let minRate = 1;
  let maxRate = 1;
  let maxQueued = 0;
  for (let f = 0; f < 60_000 * 48; f += 128) {
    const now = f / 48;
    while (packet < 6_000) {
      const capture = packet * 10;
      const jitter = 20 * (1 + Math.sin(capture / 2_000));
      const arrival = capture + 100 + jitter;
      if (arrival > now) break;
      sync.noteAudio(capture * 1_000, arrival);
      if (packet >= 1_000 && packet <= 1_500 && packet % 100 === 0) sync.noteUnderrun(arrival);
      sync.noteVideo(capture * 1_000, capture + 100 + 70 + 3 * Math.sin(capture / 500));
      const min = 55 + sync.underrunMarginMs(arrival);
      const playAt = sync.playAtMs(capture * 1_000, min)!;
      const target = sync.targetDelayMs(min);
      if (previous && target !== previous) moves++;
      previous = target;
      ring.push(Math.round((playAt - 40) * 48), [new Float32Array(480).fill(1)], f, false, target * 48);
      packet++;
    }
    ring.render(buf, f);
    const rate = Reflect.get(ring, "rate") as number;
    minRate = Math.min(minRate, rate);
    maxRate = Math.max(maxRate, rate);
    maxQueued = Math.max(maxQueued, Reflect.get(ring, "queued") as number);
  }
  return { moves, minRate, maxRate, maxQueued, silenceRuns: ring.stats.silenceRuns, dropped: ring.stats.dropped };
}

describe("audio delay stability", () => {
  it("bounds rate and target moves over 60 s of jitter without extra silence", () => {
    const result = jitterStream();
    console.info("audio delay simulation", JSON.stringify(result));
    expect(result.minRate).toBeGreaterThanOrEqual(0.995);
    expect(result.maxRate).toBeLessThanOrEqual(1.005);
    expect(result.moves).toBeLessThanOrEqual(20);
    // The unmodified scheduler/ring on this exact input also produces no silence runs.
    expect(result.silenceRuns).toBe(0);
    expect(result.dropped).toBe(0);
    expect(result.maxQueued).toBeLessThan(MAX_QUEUED_FRAMES);
  });

  it("holds the scheduled target through noise and decay, but rises promptly and waits before falling", () => {
    const sync = new AudioSync();
    const schedule = (now: number, min: number) => {
      sync.noteAudio(now * 1_000, now);
      sync.playAtMs(now * 1_000, min);
      return sync.targetDelayMs(min);
    };
    expect(schedule(0, 70)).toBe(70);
    expect(schedule(10, 74)).toBe(70);
    expect(schedule(20, 80)).toBe(80);
    expect(schedule(30, 70)).toBe(80);
    for (let t = 40; t < 4_000; t += 10) schedule(t, 70);
    expect(schedule(4_000, 76)).toBe(80); // returns inside the band: cancel the fall
    for (let t = 4_010; t < 5_000; t += 10) schedule(t, 76);
    expect(schedule(5_000, 70)).toBe(80);
    for (let t = 5_010; t < 9_999; t += 10) schedule(t, 70);
    expect(schedule(9_999, 70)).toBe(80);
    expect(schedule(10_000, 70)).toBe(70);
    expect(schedule(10_010, 500)).toBe(150);
    sync.noteUnderrun(10_010);
    expect(schedule(10_020, 70)).toBe(150);
    sync.reset();
    expect(schedule(10_030, 55)).toBe(55);
  });

  it("does not schedule continuous corrections as the underrun margin decays", () => {
    const sync = new AudioSync();
    sync.noteUnderrun(0);
    let moves = 0;
    let previous = 65;
    for (let now = 0; now <= 60_000; now += 10) {
      sync.noteAudio(now * 1_000, now);
      sync.playAtMs(now * 1_000, 55 + sync.underrunMarginMs(now));
      const target = sync.targetDelayMs(55 + sync.underrunMarginMs(now));
      if (target !== previous) moves++;
      previous = target;
      if (now < 55_000) expect(target).toBe(65);
    }
    expect(moves).toBe(1);
    expect(previous).toBeCloseTo(59.499, 3);
  });

  it.each([10, 60, 110, -10, -60, -110])("closes a %s ms step without silence, cuts or reordering", (step) => {
    const ring = new AudioRing();
    const buf = [new Float32Array(128)];
    let next = 0;
    let previous = -1;
    let biggestJump = 0;
    let markerAt = -1;
    let maxRateError = 0;
    const base = step > 0 ? 40 : 150;
    // 110 ms takes about 22 s. Queue each chunk at capture, independently of its target.
    const marker = 2_400;
    for (let f = 0; f < 24_500 * 48; f += 128) {
      while (next * 480 <= f) {
        const delay = base + (next >= 20 ? step : 0);
        const plane = Float32Array.from({ length: 480 }, (_, i) => 1 + next * 480 + i);
        ring.push(next * 480 + delay * 48, [plane], f, false, delay * 48);
        next++;
      }
      ring.render(buf, f);
      maxRateError = Math.max(maxRateError, Math.abs((Reflect.get(ring, "rate") as number) - 1));
      for (let i = 0; i < 128; i++) {
        const value = buf[0][i];
        if (!value) continue;
        if (previous >= 0) {
          if (value < previous) throw new Error(`audio went backwards: ${previous} -> ${value}`);
          biggestJump = Math.max(biggestJump, value - previous);
        }
        if (markerAt < 0 && value >= 1 + marker * 480) markerAt = f + i;
        previous = value;
      }
    }
    expect(ring.stats).toMatchObject({ silenceRuns: 0, dropped: 0 });
    expect(maxRateError).toBeLessThanOrEqual(0.005 + Number.EPSILON);
    expect(biggestJump).toBeLessThanOrEqual(STRETCH_DONE_FRAMES + 12);
    expect(Math.abs(markerAt - (marker * 480 + (base + step) * 48))).toBeLessThanOrEqual(STRETCH_DONE_FRAMES + 10);
  });
});


describe("review regressions", () => {
  it.each([2_000, 4_000])("resumes a 55 ms rise after one lost packet %i ms into correction", (lostAt) => {
    const ring = new AudioRing();
    const buf = [new Float32Array(128)];
    let next = 0;
    let markerAt = -1;
    let resumed = false;
    for (let f = 0; f < 13_000 * 48; f += 128) {
      while (next * 480 <= f) {
        const capture = next * 10;
        const delay = capture >= 200 ? 95 : 40;
        if (capture !== 200 + lostAt) {
          ring.push(next * 480 + delay * 48, [new Float32Array(480).fill(next === 1_200 ? 2 : 1)], f, false, delay * 48);
        }
        next++;
      }
      ring.render(buf, f);
      if (f > (300 + lostAt) * 48 && Reflect.get(ring, "rate") === 0.995) resumed = true;
      if (markerAt < 0) {
        const i = buf[0].findIndex((v) => v >= 1.5);
        if (i >= 0) markerAt = f + i;
      }
    }
    console.info("lost packet during correction", { lostAt, silenceMs: ring.stats.underrunFrames / 48 });
    expect(ring.stats).toMatchObject({ underrunFrames: 480, silenceRuns: 1, dropped: 0 });
    expect(resumed).toBe(true);
    expect(Math.abs(markerAt - (12_000 + 95) * 48)).toBeLessThanOrEqual(STRETCH_DONE_FRAMES + 10);
  });

  it("resumes correction after the queue runs dry without paying the remaining correction as silence", () => {
    const ring = new AudioRing();
    const buf = [new Float32Array(128)];
    let next = 0;
    let resumed = false;
    for (let f = 0; f < 6_000 * 48; f += 128) {
      // Decoder pauses for 60 ms, then posts its backlog in stream order.
      if (f < 2_200 * 48 || f >= 2_260 * 48) {
        while (next * 480 <= f) {
          const delay = next >= 20 ? 95 : 40;
          ring.push(next * 480 + delay * 48, [new Float32Array(480).fill(1)], f, false, delay * 48);
          next++;
        }
      }
      ring.render(buf, f);
      if (f > 2_280 * 48 && Reflect.get(ring, "rate") === 0.995) resumed = true;
    }
    expect(ring.stats.underrunFrames).toBeGreaterThan(0);
    expect(ring.stats.underrunFrames).toBeLessThanOrEqual(60 * 48);
    expect(ring.stats.silenceRuns).toBe(1);
    expect(resumed).toBe(true);
  });

  it.each([61, 85, 111])("keeps an unmarked %s ms stall bounded through real worklet messages", async (stall) => {
    let Processor!: new () => { port: { onmessage: (e: { data: unknown }) => void }; process: (i: Float32Array[][], o: Float32Array[][]) => boolean };
    vi.stubGlobal("currentFrame", 0);
    vi.stubGlobal("AudioWorkletProcessor", class { port = { onmessage: null, postMessage: () => {} }; });
    vi.stubGlobal("registerProcessor", (_name: string, ctor: typeof Processor) => { Processor = ctor; });
    try {
      vi.resetModules();
      await import("./audioWorklet");
      const processor = new Processor();
      const gap = stall * 48;
      for (const start of [0, 480 + gap]) {
        processor.port.onmessage({ data: { start, planes: [new Float32Array(480).fill(1)], concealed: false, targetDelayFrames: 40 * 48 } });
      }
      const out = new Float32Array(960 + gap);
      processor.process([], [[out]]);
      expect(out.slice(480, 480 + gap).every((v) => v === 0)).toBe(true);
      expect(out.slice(480 + gap).every((v) => v === 1)).toBe(true);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("restarts the fall hold after a pause in audio arrivals", () => {
    const sync = new AudioSync();
    const schedule = (t: number, target: number) => {
      sync.noteAudio(t * 1_000, t);
      sync.playAtMs(t * 1_000, target);
      return sync.targetDelayMs();
    };
    expect(schedule(0, 140)).toBe(140);
    for (let t = 10; t <= 2_000; t += 10) expect(schedule(t, 80)).toBe(140);
    expect(schedule(10_000, 80)).toBe(140);
    for (let t = 10_010; t < 15_000; t += 10) expect(schedule(t, 80)).toBe(140);
    expect(schedule(15_000, 80)).toBe(80);
  });

  it("retains the peak through sub-hold swings and falls five seconds after they stop", () => {
    const sync = new AudioSync();
    for (let t = 0; t <= 20_000; t += 10) {
      const target = t < 15_000 && t % 3_000 < 1_000 ? 140 : 80;
      sync.noteAudio(t * 1_000, t);
      sync.playAtMs(t * 1_000, target);
      // Last high ends at 13 s; repeated swings retain the peak until 18 s.
      expect(sync.targetDelayMs()).toBe(t < 18_000 ? 140 : 80);
    }
  });
});
