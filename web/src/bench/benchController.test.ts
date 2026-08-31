import { beforeEach, describe, expect, it, vi } from "vitest";
import { BenchController, presentEpochMs } from "./benchController";
import { syntheticFrame, type MarkerFields } from "./syntheticMarker";
import type { MarkerImageData } from "./marker_decode.js";
import type { BenchWindowPayload } from "./aggregate";

const BASE: MarkerFields = {
  frame_index: 1,
  host_time_ms: 1_800_000_000_000,
  scene_id: 3,
  load_level: 5,
  event_flags: 0,
  render_w: 1920,
  render_h: 1080,
};

function markerImage(over: Partial<MarkerFields>): MarkerImageData {
  return syntheticFrame({ ...BASE, ...over }, {
    width: 560,
    height: 360,
    ox: 8,
    oy: 8,
    cell: 16,
  });
}

interface Harness {
  ctl: BenchController;
  windows: BenchWindowPayload[];
  sent: Record<string, unknown>[];
  setFrame: (img: MarkerImageData | null) => void;
  setNow: (ms: number) => void;
}

function harness(offset: number | null = 0): Harness {
  let img: MarkerImageData | null = null;
  let now = 1_800_000_000_000;
  const windows: BenchWindowPayload[] = [];
  const sent: Record<string, unknown>[] = [];
  const video = { videoWidth: 1920, videoHeight: 1080 } as HTMLVideoElement;
  const ctl = new BenchController({
    video,
    sendInput: (m) => sent.push(m),
    emitWindow: (p) => windows.push(p),
    getClockOffsetMs: () => offset,
    nowEpochMs: () => now,
    grabFrame: () => img,
  });
  return {
    ctl,
    windows,
    sent,
    setFrame: (v) => { img = v; },
    setNow: (v) => { now = v; },
  };
}

describe("presentEpochMs", () => {
  it("uses RVFC expectedDisplayTime against performance.timeOrigin", () => {
    const metadata = { expectedDisplayTime: 500 } as VideoFrameCallbackMetadata;
    expect(presentEpochMs(metadata, () => 0)).toBe(performance.timeOrigin + 500);
  });

  it("falls back to the wall clock when RVFC gives no display time", () => {
    expect(presentEpochMs(undefined, () => 12345)).toBe(12345);
  });
});

describe("BenchController", () => {
  beforeEach(() => vi.useRealTimers());

  it("decodes a frame and computes glass-to-glass from the marker timestamp", () => {
    const h = harness(0);
    h.ctl.start();
    h.setNow(BASE.host_time_ms + 90);
    h.setFrame(markerImage({ frame_index: 7 }));
    h.ctl.onFrame();
    h.ctl.stop();

    expect(h.windows).toHaveLength(1);
    const w = h.windows[0]!;
    expect(w.n).toBe(1);
    expect(w.decoded).toBe(1);
    expect(w.g2g_p50_ms).toBe(90);
    expect(w.render_w).toBe(1920);
    expect(w.offset_unknown).toBe(false);
  });

  it("applies the ST-05 offset in the documented direction (host = client + offset)", () => {
    const h = harness(25);
    h.ctl.start();
    h.setNow(BASE.host_time_ms + 90);
    h.setFrame(markerImage({ frame_index: 7 }));
    h.ctl.onFrame();
    h.ctl.stop();
    expect(h.windows[0]!.g2g_p50_ms).toBe(115);
    expect(h.windows[0]!.offset_ms).toBe(25);
  });

  it("flags offset_unknown when the clock estimate is unavailable", () => {
    const h = harness(null);
    h.ctl.start();
    h.setFrame(markerImage({ frame_index: 1 }));
    h.ctl.onFrame();
    h.ctl.stop();
    expect(h.windows[0]!.offset_unknown).toBe(true);
    expect(h.windows[0]!.offset_ms).toBeNull();
  });

  it("classifies missing indices, duplicates and reorders across frames", () => {
    const h = harness();
    h.ctl.start();
    for (const idx of [10, 11, 15, 15, 12]) {
      h.setFrame(markerImage({ frame_index: idx }));
      h.ctl.onFrame();
    }
    h.ctl.stop();
    const w = h.windows[0]!;
    expect(w.decoded).toBe(5);
    expect(w.missing_indices).toBe(3); // 11 → 15
    expect(w.duplicated).toBe(1); // 15 → 15
    expect(w.reordered).toBe(1); // 15 → 12
  });

  it("counts an undecodable frame as undecoded without breaking the loop", () => {
    const h = harness();
    h.ctl.start();
    h.setNow(1_000);
    h.setFrame({ width: 200, height: 200, data: new Uint8ClampedArray(200 * 200 * 4).fill(90) });
    h.ctl.onFrame();
    // Past the full-search throttle, so the next frame gets a real search.
    h.setNow(1_400);
    h.setFrame(markerImage({ frame_index: 4 }));
    h.ctl.onFrame();
    h.ctl.stop();
    const w = h.windows[0]!;
    expect(w.n).toBe(2);
    expect(w.decoded).toBe(1);
    expect(w.undecoded).toBe(1);
  });

  it("measures input-to-photon from the key send to the echo flag's rising edge", () => {
    const h = harness();
    h.ctl.start();
    // A pulse already lit before we send must NOT be matched.
    h.setNow(1000);
    h.setFrame(markerImage({ frame_index: 1, event_flags: 0x01 }));
    h.ctl.onFrame();
    h.setNow(1010);
    h.setFrame(markerImage({ frame_index: 2, event_flags: 0x00 }));
    h.ctl.onFrame();

    h.setNow(1020);
    expect(h.ctl.pressKey("Space")).toBe(true);
    expect(h.sent).toEqual([
      { t: "k", code: 57, pressed: true },
      { t: "k", code: 57, pressed: false },
    ]);

    h.setNow(1030);
    h.setFrame(markerImage({ frame_index: 3, event_flags: 0x00 }));
    h.ctl.onFrame();
    h.setNow(1065);
    h.setFrame(markerImage({ frame_index: 4, event_flags: 0x01 }));
    h.ctl.onFrame();
    h.ctl.stop();

    expect(h.windows[0]!.i2p_ms).toEqual([45]);
  });

  it("refuses an unknown key but QUEUES overlapping pulses", () => {
    const h = harness();
    h.ctl.start();
    expect(h.ctl.pressKey("NoSuchKey")).toBe(false);
    expect(h.ctl.pressKey("Space")).toBe(true);
    // A second pulse while the first is outstanding must not be refused —
    // refusing it is how one missed echo used to kill i2p for a whole run.
    expect(h.ctl.pressKey("Space")).toBe(true);
    h.ctl.stop();
  });

  it("exposes frames and windows through the __qBench handle", () => {
    const h = harness();
    h.ctl.start();
    h.setFrame(markerImage({ frame_index: 9, scene_id: 2 }));
    h.ctl.onFrame();
    h.ctl.stop();
    const handle = h.ctl.handle();
    expect(handle.enabled).toBe(true);
    const dumped = handle.dump();
    expect(dumped.frames).toHaveLength(1);
    expect(dumped.frames[0]!.frame_index).toBe(9);
    expect(dumped.frames[0]!.scene_id).toBe(2);
    expect(dumped.windows).toHaveLength(1);
    expect(handle.stats()).toMatchObject({ frames: 1, decoded: 1, located: true });
  });

  it("does nothing once stopped", () => {
    const h = harness();
    h.ctl.start();
    h.setFrame(markerImage({ frame_index: 1 }));
    h.ctl.onFrame();
    h.ctl.stop();
    const before = h.ctl.handle().dump().frames.length;
    h.ctl.onFrame();
    expect(h.ctl.handle().dump().frames).toHaveLength(before);
  });

  it("never requests a video frame callback before start()", () => {
    const rvfc = vi.fn();
    const ctl = new BenchController({
      video: { videoWidth: 0, videoHeight: 0, requestVideoFrameCallback: rvfc } as unknown as HTMLVideoElement,
      sendInput: () => {},
      emitWindow: () => {},
      getClockOffsetMs: () => null,
    });
    expect(rvfc).not.toHaveBeenCalled();
    ctl.start();
    expect(rvfc).toHaveBeenCalledTimes(1);
    ctl.stop();
  });
});

describe("BenchController — recovery paths the review found (I2/I3/I4)", () => {
  it("abandons a send whose echo never arrives, and keeps measuring after (I2)", () => {
    const h = harness();
    h.ctl.start();
    h.setNow(1_000);
    expect(h.ctl.pressKey("Space")).toBe(true);

    // The echo frame is lost. Frames keep arriving past the timeout.
    for (const [t, idx] of [[1_500, 1], [3_500, 2]] as const) {
      h.setNow(t);
      h.setFrame(markerImage({ frame_index: idx, event_flags: 0 }));
      h.ctl.onFrame();
    }
    // A later pulse is accepted and DOES produce a sample — the old behaviour
    // wedged here and reported an empty i2p series for the rest of the run.
    h.setNow(4_000);
    expect(h.ctl.pressKey("Space")).toBe(true);
    h.setNow(4_060);
    h.setFrame(markerImage({ frame_index: 3, event_flags: 0x01 }));
    h.ctl.onFrame();
    h.ctl.stop();

    const all = h.windows.flatMap((w) => w.i2p_ms);
    expect(all).toEqual([60]);
    expect(h.windows.reduce((a, w) => a + w.i2p_missed, 0)).toBe(1);
    expect(h.ctl.handle().stats().i2p_missed).toBe(1);
  });

  it("pairs queued pulses FIFO with successive rising edges (I2)", () => {
    const h = harness();
    h.ctl.start();
    h.setNow(1_000);
    h.ctl.pressKey("Space");
    h.setNow(1_010);
    h.ctl.pressKey("Space");

    h.setNow(1_050);
    h.setFrame(markerImage({ frame_index: 1, event_flags: 0x01 }));
    h.ctl.onFrame();
    h.setNow(1_060);
    h.setFrame(markerImage({ frame_index: 2, event_flags: 0x00 }));
    h.ctl.onFrame();
    h.setNow(1_080);
    h.setFrame(markerImage({ frame_index: 3, event_flags: 0x01 }));
    h.ctl.onFrame();
    h.ctl.stop();

    expect(h.windows.flatMap((w) => w.i2p_ms)).toEqual([50, 70]);
  });

  it("does not let a frame with no readable image inflate n or undecoded", () => {
    const h = harness();
    h.ctl.start();
    h.setFrame(null);
    h.ctl.onFrame();
    h.ctl.onFrame();
    h.setFrame(markerImage({ frame_index: 1 }));
    h.ctl.onFrame();
    h.ctl.stop();
    const w = h.windows[0]!;
    expect(w.n).toBe(1);
    expect(w.decoded).toBe(1);
    expect(w.undecoded).toBe(0);
    expect(w.no_image).toBe(2);
  });

  it("emits a window for a frameless second so a freeze is not compressed away", () => {
    const h = harness();
    h.ctl.start();
    h.setFrame(markerImage({ frame_index: 1 }));
    h.ctl.onFrame();
    // Two frameless windows, then one with a frame.
    h.ctl.closeWindowForTest();
    h.ctl.closeWindowForTest();
    h.ctl.closeWindowForTest();
    h.setFrame(markerImage({ frame_index: 2 }));
    h.ctl.onFrame();
    h.ctl.stop();
    expect(h.windows.map((w) => w.n)).toEqual([1, 0, 0, 1]);
  });

  it("re-acquires the marker after a resolution change (I3)", () => {
    let vw = 1280;
    let vh = 720;
    let img: MarkerImageData | null = null;
    const windows: BenchWindowPayload[] = [];
    const video = {
      get videoWidth() { return vw; },
      get videoHeight() { return vh; },
    } as unknown as HTMLVideoElement;
    const ctl = new BenchController({
      video,
      sendInput: () => {},
      emitWindow: (p) => windows.push(p),
      getClockOffsetMs: () => 0,
      nowEpochMs: () => 1_800_000_000_000,
      grabFrame: () => img,
    });
    ctl.start();
    img = markerImage({ frame_index: 1 });
    ctl.onFrame();
    expect(ctl.handle().stats().located).toBe(true);

    // The rung changes upward. The old code only reset on a DOWNscale, so the
    // cached geometry survived and decode was lost for the rest of the session.
    vw = 1920;
    vh = 1080;
    img = markerImage({ frame_index: 2 });
    ctl.onFrame();
    ctl.stop();
    expect(ctl.handle().stats().decoded).toBe(2);
  });

  it("throttles the expensive full search while the marker is absent (I4)", () => {
    let now = 1_000;
    const blank: MarkerImageData = {
      width: 600, height: 400, data: new Uint8ClampedArray(600 * 400 * 4).fill(90),
    };
    const ctl = new BenchController({
      video: { videoWidth: 1920, videoHeight: 1080 } as HTMLVideoElement,
      sendInput: () => {},
      emitWindow: () => {},
      getClockOffsetMs: () => 0,
      nowEpochMs: () => now,
      grabFrame: () => blank,
    });
    ctl.start();
    const t0 = performance.now();
    // 120 frames inside one throttle interval: at most ONE may pay for a full
    // search. Before the throttle this was 120 integral images at ~3.4 MB each.
    for (let i = 0; i < 120; i++) ctl.onFrame();
    const elapsed = performance.now() - t0;
    ctl.stop();
    // A single full search over this image is ~10-40 ms; 120 of them would be
    // seconds. The bound is deliberately loose — it is catching an order of
    // magnitude, not benchmarking the decoder.
    expect(elapsed).toBeLessThan(1_500);

    // And the throttle opens again once the interval elapses.
    now += 1_000;
    ctl.start();
    ctl.onFrame();
    ctl.stop();
  });
});
