import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { BenchController, READBACK_SETTLE_MS, READBACKS_IN_FLIGHT, presentEpochMs } from "./benchController";
import { syntheticFrame, type MarkerFields } from "./syntheticMarker";
import type { MarkerImageData } from "./marker_decode.js";
import { WEB_BUILD } from "./webBuild";
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

  it("reports the web build through the __qBench handle (#1281)", () => {
    expect(harness().ctl.handle().build()).toEqual(WEB_BUILD);
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

  it("reads no pixels between full searches, and counts the frames it did not read", () => {
    const now = 1_000;
    let grabs = 0;
    const blank: MarkerImageData = {
      width: 600, height: 400, data: new Uint8ClampedArray(600 * 400 * 4).fill(90),
    };
    const ctl = new BenchController({
      video: { videoWidth: 1920, videoHeight: 1080 } as HTMLVideoElement,
      sendInput: () => {},
      emitWindow: () => {},
      getClockOffsetMs: () => 0,
      nowEpochMs: () => now,
      grabFrame: () => { grabs += 1; return blank; },
    });
    ctl.start();
    for (let i = 0; i < 120; i++) ctl.onFrame();
    const stats = ctl.handle().stats();
    ctl.stop();
    expect(grabs).toBe(1);
    expect(stats.skipped_reads).toBe(119);
    expect(stats.callback_ms_mean).not.toBeNull();
    // Every presented frame is still a frame of the window, undecoded.
    expect(ctl.handle().windows()[0]!.n).toBe(120);
  });

  it("counts presentations the callback never saw from the presentedFrames gap", () => {
    const { ctl, setFrame } = harness();
    setFrame(markerImage({ frame_index: 1 }));
    ctl.start();
    for (const presentedFrames of [10, 11, 14]) {
      ctl.onFrame({ presentedFrames } as VideoFrameCallbackMetadata);
    }
    ctl.stop();
    expect(ctl.handle().stats().missed_presentations).toBe(2);
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

/** A device whose texture holds `scene.image`: each copy reads the scene as it is at submit, and
 *  each map waits until the test releases it (a GPU that has not finished yet). */
function textureDevice(scene: { image: MarkerImageData }) {
  const copies: { w: number; h: number; bytesPerRow: number }[] = [];
  const maps: (() => void)[] = [];
  let created = 0;
  let mapFails = false;
  let scopeError: GPUError | null = null;
  let scopeDepth = 0;
  let createThrows = false;
  const device = {
    pushErrorScope: () => void scopeDepth++,
    popErrorScope: () => (scopeDepth--, Promise.resolve(scopeError)),
    createBuffer: ({ size }: { size: number }) => {
      if (createThrows) throw new Error("no buffer");
      created++;
      const mem = new Uint8Array(size);
      return {
        size,
        mem,
        mapAsync: () => (mapFails ? Promise.reject(new Error("no readback")) : new Promise<void>((r) => maps.push(r))),
        getMappedRange: (o: number, n: number) => mem.buffer.slice(o, o + n),
        unmap: () => {},
        destroy: () => {},
      };
    },
    createCommandEncoder: () => ({
      copyTextureToBuffer: (_src: unknown, dst: { buffer: { mem: Uint8Array }; bytesPerRow: number }, [w, h]: number[]) => {
        copies.push({ w: w!, h: h!, bytesPerRow: dst.bytesPerRow });
        const { image } = scene;
        for (let y = 0; y < h!; y++) dst.buffer.mem.set(image.data.subarray(y * image.width * 4, (y * image.width + w!) * 4), y * dst.bytesPerRow);
      },
      finish: () => ({}),
    }),
    queue: { submit: () => {} },
  };
  const release = async () => {
    for (const r of maps.splice(0)) r();
    await new Promise((r) => setTimeout(r, 0));
  };
  return { device: device as unknown as GPUDevice, copies, release, created: () => created, failMaps: () => (mapFails = true), failScope: (e: GPUError | null) => (scopeError = e), scopeDepth: () => scopeDepth, throwOnCreate: () => (createThrows = true) };
}

describe("BenchController.onTexture (a PyroWave frame, #1344)", () => {
  beforeEach(() => {
    vi.stubGlobal("GPUBufferUsage", { MAP_READ: 1, COPY_DST: 8 });
    vi.stubGlobal("GPUMapMode", { READ: 1 });
    return () => vi.unstubAllGlobals();
  });

  const W = 1280;
  const H = 720;
  const scene = (frame_index: number) =>
    syntheticFrame({ ...BASE, frame_index, render_w: W, render_h: H }, { width: W, height: H, ox: 8, oy: 8, cell: 16 });

  it("reads back only the marker's crop, never stalls the caller, and measures each frame in order at its submit time", async () => {
    let now = BASE.host_time_ms + 50;
    const windows: BenchWindowPayload[] = [];
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: (p) => windows.push(p), getClockOffsetMs: () => 0, nowEpochMs: () => now });
    const s = { image: scene(1) };
    const gpu = textureDevice(s);
    const texture = { width: W, height: H } as GPUTexture;
    ctl.start();

    ctl.onTexture(gpu.device, texture);
    // Returns before the copy is mapped: nothing measured yet.
    expect(ctl.handle().stats().frames).toBe(0);
    s.image = scene(2);
    now += 16;
    ctl.onTexture(gpu.device, texture);
    now += 100; // measured later, but stamped at its submit
    await gpu.release();

    const { frames } = ctl.handle().dump();
    expect(frames.map((f) => [f.frame_index, f.g2g_ms])).toEqual([[1, 50], [2, 66]]);
    // The first copy is the search crop (45 % of the frame, at least 512 px), rows padded to 256 bytes.
    expect(gpu.copies[0]).toEqual({ w: 576, h: 512, bytesPerRow: 2304 });
    s.image = scene(3);
    ctl.onTexture(gpu.device, texture);
    await gpu.release();
    // Once located, the marker's bounding box and a cell: about 8 + 31 x 16 by 8 + 20 x 16.
    const { w, h } = gpu.copies[2]!;
    expect([w > 480 && w <= 504, h > 304 && h <= 328]).toEqual([true, true]);
    expect(ctl.handle().stats()).toMatchObject({ frames: 3, decoded: 3, located: true, no_image: 0 });
    // The buffers come back for the next frames.
    expect(gpu.created()).toBe(2);
    ctl.stop();
    expect(windows[0]).toMatchObject({ n: 3, decoded: 3 });
  });

  it("measures a texture frame in the window it was submitted, not when its map resolves (#1454)", async () => {
    let now = BASE.host_time_ms + 50;
    const windows: BenchWindowPayload[] = [];
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: (p) => windows.push(p), getClockOffsetMs: () => 0, nowEpochMs: () => now });
    const s = { image: scene(1) };
    const gpu = textureDevice(s);
    const texture = { width: W, height: H } as GPUTexture;
    ctl.start();

    // Frame 1 submitted at T0 (window 0); its map is not released yet.
    ctl.onTexture(gpu.device, texture);
    // The window boundary moves into the next second before the map settles.
    now += 1200;
    // Start the tick while the readback is still in flight, then let the device
    // finish it during the tick's bounded wait.
    const tickP = ctl.tickForTest();
    await gpu.release();
    await tickP;
    ctl.stop();

    // The frame stamped at T0 is measured before window 0 closes, so it lands
    // in window 0 (whose bounds start at T0), not in the next window. Without
    // the fix closeWindow ran before the map resolved and the frame landed in
    // window 1, outside its own [t_start_ms, t_end_ms].
    expect(windows.map((w) => w.t_start_ms)).toEqual([BASE.host_time_ms + 50, BASE.host_time_ms + 1250]);
    expect(windows.map((w) => w.n)).toEqual([1, 0]);
    expect(windows[0]!).toMatchObject({ decoded: 1 });
  });

  it("a device whose map never settles counts the frame no_image and keeps the chain moving (#1454)", async () => {
    let now = BASE.host_time_ms + 50;
    const windows: BenchWindowPayload[] = [];
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: (p) => windows.push(p), getClockOffsetMs: () => 0, nowEpochMs: () => now });
    const s = { image: scene(1) };
    const gpu = textureDevice(s);
    const texture = { width: W, height: H } as GPUTexture;
    ctl.start();

    // Frame 1 submitted; its map never releases — a device whose maps never settle.
    ctl.onTexture(gpu.device, texture);

    // The tick must not hang on such a device: its bounded settle wait returns and
    // closes the window on time.
    const t0 = performance.now();
    await ctl.tickForTest();
    expect(performance.now() - t0).toBeLessThan(1000);

    // Its own bound fires: the stuck frame counts as no_image and its in-flight slot
    // is released. The old code held the slot and the readTail link forever, so the
    // frame vanished — never counted, not even as no_image (silence).
    await new Promise((r) => setTimeout(r, READBACK_SETTLE_MS + 20));
    expect(ctl.handle().stats()).toMatchObject({ no_image: 1, frames: 0 });

    // A later frame whose map does resolve is still measured — the chain moved on.
    now += 16;
    s.image = scene(2);
    ctl.onTexture(gpu.device, texture);
    await gpu.release();
    expect(ctl.handle().stats()).toMatchObject({ no_image: 1, frames: 1 });
    ctl.stop();
  });

  it("a frame past the readbacks in flight, or one the device cannot read back, is no_image, never silence", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: () => {}, getClockOffsetMs: () => 0 });
    const gpu = textureDevice({ image: scene(1) });
    const texture = { width: W, height: H } as GPUTexture;
    ctl.start();
    for (let i = 0; i < READBACKS_IN_FLIGHT + 1; i++) ctl.onTexture(gpu.device, texture);
    expect(gpu.copies).toHaveLength(READBACKS_IN_FLIGHT);
    await gpu.release();
    expect(ctl.handle().stats()).toMatchObject({ frames: READBACKS_IN_FLIGHT, no_image: 1 });

    gpu.failMaps();
    ctl.onTexture(gpu.device, texture);
    ctl.onTexture(gpu.device, texture);
    await gpu.release();
    expect(ctl.handle().stats()).toMatchObject({ frames: READBACKS_IN_FLIGHT, no_image: 3 });
    expect(warn.mock.calls.filter((c) => c[0] === "bench: texture readback")).toHaveLength(1);
    expect(warn.mock.calls.filter((c) => String(c[0]).startsWith("bench: readbacks fell behind"))).toHaveLength(1);
    ctl.stop();
    warn.mockRestore();
  });

  it("a GPU error from the readback stays in its own error scope and counts as no_image", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: () => {}, getClockOffsetMs: () => 0 });
    const gpu = textureDevice({ image: scene(1) });
    gpu.failScope({ message: "bad copy" } as GPUError);
    ctl.start();
    ctl.onTexture(gpu.device, { width: W, height: H } as GPUTexture);
    await gpu.release();
    expect(ctl.handle().stats()).toMatchObject({ frames: 0, no_image: 1 });
    expect(gpu.scopeDepth()).toBe(0);
    // A throw between push and pop must not leave the scopes on the presenter's device.
    gpu.failScope(null);
    gpu.throwOnCreate();
    ctl.onTexture(gpu.device, { width: W, height: H } as GPUTexture);
    await gpu.release();
    expect(ctl.handle().stats()).toMatchObject({ frames: 0, no_image: 2 });
    expect(gpu.scopeDepth()).toBe(0);
    ctl.stop();
    warn.mockRestore();
  });
});

/** A device whose readback maps resolve one at a time, in submission order. A frame whose map is
 *  never released stays in flight ("a GPU that has not finished yet"), so a stuck frame is one we
 *  simply never call release on. */
function orderedReadbackDevice() {
  const resolvers: (() => void)[] = [];
  const device = {
    pushErrorScope: () => {},
    popErrorScope: () => Promise.resolve(null),
    createBuffer: ({ size }: { size: number }) => {
      const mem = new Uint8Array(size);
      return {
        size, mem,
        mapAsync: () => new Promise<void>((r) => resolvers.push(r)),
        getMappedRange: (o: number, n: number) => mem.buffer.slice(o, o + n),
        unmap: () => {}, destroy: () => {},
      };
    },
    createCommandEncoder: () => ({ copyTextureToBuffer: () => {}, finish: () => ({}) }),
    queue: { submit: () => {} },
  } as unknown as GPUDevice;
  /** Settle the oldest in-flight map (one readback completes). */
  const releaseOne = () => { resolvers.shift()?.(); };
  return { device: device as unknown as GPUDevice, releaseOne };
}

describe("BenchController — settle deadline (#1575)", () => {
  const W = 1280;
  const H = 720;
  const scene = (frame_index: number) =>
    syntheticFrame({ ...BASE, frame_index, render_w: W, render_h: H }, { width: W, height: H, ox: 8, oy: 8, cell: 16 });

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(BASE.host_time_ms);
    vi.stubGlobal("GPUBufferUsage", { MAP_READ: 1, COPY_DST: 8 });
    vi.stubGlobal("GPUMapMode", { READ: 1 });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  /**
   * The bug: the loop raced a FRESH `settleDelay(READBACK_SETTLE_MS)` each pass and checked the
   * deadline only afterwards, so a frame that settled just before the deadline forced a second
   * full 250 ms wait past it — ~500 ms total on a clock the deadline never saw. The fix passes the
   * remaining time to one `settleDelay`, so the whole wait is bounded by READBACK_SETTLE_MS.
   */
  it("bounds the settle wait to one READBACK_SETTLE_MS even as a frame races the deadline", async () => {
    const windows: BenchWindowPayload[] = [];
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: (p) => windows.push(p), getClockOffsetMs: () => 0, nowEpochMs: () => Date.now() });
    const s = { image: scene(1) };
    const gpu = orderedReadbackDevice();
    const texture = { width: W, height: H } as GPUTexture;
    ctl.start();

    ctl.onTexture(gpu.device, texture); // frame 1 at T0, map pending -> settle loop starts, deadline T0 + 250
    const start = Date.now();
    let settled = -1;
    ctl.tickForTest().then(() => { settled = Date.now() - start; });
    // Race frame 1's map to just before the deadline, then submit a second frame whose map is stuck.
    await vi.advanceTimersByTimeAsync(READBACK_SETTLE_MS - 1); // T0 + 249
    gpu.releaseOne(); // frame 1 settles at T0 + 249
    s.image = scene(2);
    ctl.onTexture(gpu.device, texture); // frame 2 at T0 + 249, map never released
    await vi.advanceTimersByTimeAsync(READBACK_SETTLE_MS + 50); // let the settle loop run out
    // Bounded to one deadline on one clock (~250), never a second full delay (~500).
    expect(settled).toBeGreaterThanOrEqual(READBACK_SETTLE_MS - 40);
    expect(settled).toBeLessThan(READBACK_SETTLE_MS + 60);
    ctl.stop();
  });

  it("stops at READBACK_SETTLE_MS with a frozen nowEpochMs while frames keep arriving", async () => {
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: () => {}, getClockOffsetMs: () => 0, nowEpochMs: () => BASE.host_time_ms });
    const s = { image: scene(1) };
    const gpu = orderedReadbackDevice();
    const texture = { width: W, height: H } as GPUTexture;
    ctl.start();

    ctl.onTexture(gpu.device, texture);
    const start = Date.now();
    let settled = -1;
    ctl.tickForTest().then(() => { settled = Date.now() - start; });
    // A new frame every 50 ms, each settling the one before: readTail never stops changing.
    for (let i = 2; i <= 16 && settled < 0; i++) {
      await vi.advanceTimersByTimeAsync(50);
      s.image = scene(i);
      ctl.onTexture(gpu.device, texture);
      gpu.releaseOne();
    }
    expect(settled).toBeGreaterThanOrEqual(0);
    expect(settled).toBeLessThan(READBACK_SETTLE_MS + 60);
    ctl.stop();
  });

  /**
   * The loop waits while a new frame keeps swapping `readTail`. A frame submitted during the wait
   * must be waited for and measured before the window closes, so it lands in the current window,
   * not the next. A single wait (no loop) returns as soon as the first readback settles and pushes
   * the later frame into the next window (#1454).
   */
  it("waits for a frame submitted during the settle wait so it lands in the current window", async () => {
    const windows: BenchWindowPayload[] = [];
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: (p) => windows.push(p), getClockOffsetMs: () => 0, nowEpochMs: () => Date.now() });
    const s = { image: scene(1) };
    const gpu = orderedReadbackDevice();
    const texture = { width: W, height: H } as GPUTexture;
    ctl.start();

    ctl.onTexture(gpu.device, texture); // frame 1 at T0, map pending -> loop tail = chain1
    const tickP = ctl.tickForTest();
    await vi.advanceTimersByTimeAsync(100); // T0 + 100, inside the settle wait
    s.image = scene(2);
    ctl.onTexture(gpu.device, texture); // frame 2 at T0 + 100, map pending -> readTail = chain2
    await vi.advanceTimersByTimeAsync(20); // T0 + 120
    gpu.releaseOne(); // frame 1 settles -> loop sees readTail change, advances to chain2, keeps waiting
    await vi.advanceTimersByTimeAsync(20); // T0 + 140, loop now waiting on chain2
    gpu.releaseOne(); // frame 2 settles -> readTail stable -> settle completes
    await tickP;
    ctl.stop(); // stop() closes the (now-empty) window a final time -> [2, 0]
    // Both frames are measured before the first window closes, so they share window 0 (n = 2).
    // A single wait would return after frame 1 and push frame 2 into the next window -> [1, 1, 0].
    expect(windows.map((w) => w.n)).toEqual([2, 0]);
    expect(ctl.handle().stats().frames).toBe(2);
  });

  /**
   * `stop()` sets running=false and closes the window. If it runs during the settle wait, tick()
   * must return on the `if (!this.running)` guard rather than falling through to a second
   * closeWindow(). Removing the guard emits an extra empty window after stop().
   */
  it("returns without closing the window again if stop() ran during the settle wait", async () => {
    const windows: BenchWindowPayload[] = [];
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: (p) => windows.push(p), getClockOffsetMs: () => 0, nowEpochMs: () => Date.now() });
    const gpu = orderedReadbackDevice();
    const texture = { width: W, height: H } as GPUTexture;
    ctl.start();

    ctl.onTexture(gpu.device, texture); // frame 1 at T0, map stuck -> settle loop waits up to the deadline
    const tickP = ctl.tickForTest();
    await vi.advanceTimersByTimeAsync(50); // T0 + 50, inside the settle wait
    ctl.stop(); // running=false, closes the window once
    const afterStop = windows.length;
    await vi.advanceTimersByTimeAsync(READBACK_SETTLE_MS + 50); // let the settle loop finish
    await tickP; // tick() must return on the running guard, not close the window a second time
    expect(windows.length).toBe(afterStop);
    ctl.stop(); // idempotent
  });
});
