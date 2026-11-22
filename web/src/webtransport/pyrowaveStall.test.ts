import { afterEach, describe, expect, it, vi } from "vitest";
import { blockLayout, parsePwv1 } from "../pyrowave/bitstream";
import type { PyroWaveFrame } from "../pyrowave/receiver";
import { WebTransportMedia } from "./webTransportMedia";
import { parseVideoDatagram, type VideoFragment } from "./framing";
import { FeedbackRecorder } from "./feedback";
import { COORDS, packPyroWave, pyroWaveDatagrams } from "./testSupport";
import type { MediaSink, VideoConfig } from "./transport";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// #1635: a page stall longer than the frame deadline must not turn datagrams queued behind it into loss.
const { header, frames } = parsePwv1(new Uint8Array(readFileSync(resolve(__dirname, "../../../testdata/pyrowave/detail_320x192_xhi.pwv1"))));
const N = blockLayout(header.width, header.height).totalBlocks;
const FRAME_MS = 1000 / 60;

const frags = (frameSeq: number): VideoFragment[] =>
  pyroWaveDatagrams({ configId: 3, frameSeq }, packPyroWave(frames[0], N, 1000)).map((d) => parseVideoDatagram(d) as VideoFragment);

function setup() {
  vi.useFakeTimers();
  let now = 0;
  const out: PyroWaveFrame[] = [];
  const sink: MediaSink = { config: () => {}, unit: () => {}, pyrowave: (f) => out.push(f), end: () => {} };
  const t = new WebTransportMedia({ sessionId: "s", coords: COORDS, mintToken: async () => COORDS, now: () => now });
  const router = (t as any).makeRouter(sink, { on: true }, new FeedbackRecorder(0), () => 100, () => null);
  router.config({ configId: 3, codec: "pyrowave", description: new Uint8Array(), width: header.width, height: header.height, fps: 60 } as VideoConfig);
  /** Time passes with the page blocked: due timers fire late, before anything queued is read. */
  const stall = (ms: number) => {
    now += ms;
    vi.advanceTimersToNextTimer();
  };
  /** Time passes smoothly, 1 ms at a time. */
  const run = (ms: number) => {
    for (let i = 0; i < ms; i++) {
      now += 1;
      vi.advanceTimersByTime(1);
    }
  };
  const lost = () => (t as any).lostSoFar() as number;
  return { out, router, lost, stall, run, at: (ms: number) => void (now = ms) };
}

afterEach(() => vi.useRealTimers());

describe("PyroWave deadline across a page stall (#1635)", () => {
  it("a tail read after a stall longer than the deadline completes the frame, nothing lost", () => {
    const { out, router, lost, stall } = setup();
    const f = frags(1);
    expect(f.length).toBeGreaterThan(2);
    f.slice(0, 2).forEach((x) => router.datagram(x));
    stall(30);
    f.slice(2).forEach((x) => router.datagram(x));
    expect(out.map((x) => [x.frameSeq, x.complete])).toEqual([[1, true]]);
    expect(lost()).toBe(0);
  });

  it("the deferred fire is not a 0 ms turn: a tail read after other due timers still lands", () => {
    const { out, router, lost, stall } = setup();
    const f = frags(1);
    f.slice(0, 2).forEach((x) => router.datagram(x));
    stall(30);
    // The browser may run every timer already due before the reads the stall queued.
    vi.advanceTimersByTime(1); // a 0 ms timer runs no later than this (browsers clamp it to 1-4 ms)
    f.slice(2).forEach((x) => router.datagram(x));
    expect(out.map((x) => [x.frameSeq, x.complete])).toEqual([[1, true]]);
    expect(lost()).toBe(0);
  });

  it("a tail that is really lost is concealed at the one-frame deadline, no later", () => {
    const { out, router, lost, run } = setup();
    frags(1).slice(0, 2).forEach((x) => router.datagram(x));
    run(Math.floor(FRAME_MS) - 1);
    expect(out).toEqual([]);
    run(3);
    expect(out.map((x) => [x.frameSeq, x.complete])).toEqual([[1, false]]);
    expect(lost()).toBe(frags(1).length - 2);
  });

  it("after a stall, a really lost tail is concealed max(lateness, one frame) later, once", () => {
    const { out, router, stall, run } = setup();
    frags(1).slice(0, 2).forEach((x) => router.datagram(x));
    stall(30);
    expect(out, "the first fire only defers").toEqual([]);
    // 13 ms late, so the fire waits max(13, one frame) = one frame more.
    run(15);
    expect(out, "deferred by at least a frame").toEqual([]);
    run(3);
    expect(out.map((x) => x.complete)).toEqual([false]);
  });

  it("a complete newer frame is presented at once, never held behind an older incomplete one", () => {
    const { out, router, lost, stall, at } = setup();
    frags(1).slice(0, 2).forEach((x) => router.datagram(x));
    stall(30);
    at(31);
    frags(2).forEach((x) => router.datagram(x));
    expect(out.map((x) => [x.frameSeq, x.complete])).toEqual([[2, true]]);
    expect(lost()).toBe(frags(1).length - 2);
  });
});
