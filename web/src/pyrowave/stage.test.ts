// #1416 — the stage's decode back-pressure: a GPU slower than the frame rate must drop frames, not
// queue them. The stage is fed a fake decoder and a device whose submitted work is held open, so
// the test decides when a decode is "finished".

import { describe, expect, it, vi } from "vitest";
import type { VideoConfig } from "../webtransport/transport";
import { MAX_IN_FLIGHT_DECODES, PyroWaveStage } from "./stage";

function header(width: number, height: number): Uint8Array {
  const b = new Uint8Array(24);
  const dv = new DataView(b.buffer);
  dv.setUint32(0, 0x3156_5750, true); // "PWV1"
  dv.setUint32(4, ((width - 1) & 0x3fff) | (((height - 1) & 0x3fff) << 14), true);
  dv.setUint32(8, 0, true); // 4:2:0, sRGB/BT.709, full range
  b[21] = 1; // header_version
  return b;
}

/** A submitted-work promise the test settles by hand, so "the GPU is still busy" is observable. */
function gate() {
  let resolve!: () => void;
  let reject!: (e: unknown) => void;
  const p = new Promise<void>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { p, resolve, reject };
}

function setup() {
  const config: VideoConfig = {
    configId: 0,
    codec: "pyrowave",
    description: header(640, 360),
    width: 640,
    height: 360,
    fps: 60,
  };
  /** Every decode, by its frame's sequence number. */
  const decoded: number[] = [];
  const presented: number[] = [];
  /** One per decode, in order: settle it to say that decode's GPU work finished. */
  const gates: ReturnType<typeof gate>[] = [];
  const failWith: { error: Error | null } = { error: null };
  const device = {
    queue: {
      onSubmittedWorkDone: () => {
        const g = gate();
        gates.push(g);
        return g.p;
      },
    },
  } as unknown as GPUDevice;
  const texture = { width: 640, height: 360 } as GPUTexture;
  const droppedCallbacks: number[] = [];
  const stage = new PyroWaveStage({
    device,
    present: () => {
      presented.push(decoded.length);
      return true;
    },
    onPresented: () => {},
    onInFlightDrop: (total) => droppedCallbacks.push(total),
    fallback: vi.fn(),
    load: async () =>
      ({
        PyroWaveDecoder: {
          create: async () => ({
            texture,
            decode: (packets: readonly Uint8Array[]) => {
              if (failWith.error) throw failWith.error;
              decoded.push(Number(new TextDecoder().decode(packets[0])));
              return { received: 0, zero: 0, lost: 0 };
            },
            destroy: () => {},
          }),
        },
      }) as never,
  });
  /** Hands the stage one frame of the open configuration. */
  const send = (frameSeq: number): void =>
    stage.frame({
      configId: 0,
      frameSeq,
      ts90k: frameSeq,
      captureUs: frameSeq,
      packets: [new TextEncoder().encode(String(frameSeq))],
      covered: new Uint8Array([1]),
      complete: true,
    });
  /** Opens the configuration and lets its decoder build, so frames decode from here on. */
  const open = async () => {
    stage.config(config);
    // `config` builds the decoder in a promise chain (the module seam is a plain async function).
    await new Promise((r) => setTimeout(r, 0));
  };
  return { stage, open, send, decoded, presented, gates, failWith, droppedCallbacks };
}

describe("PyroWave decode back-pressure (#1416)", () => {
  it("stops submitting once the bound is full and keeps only the newest frame, so a slow GPU cannot queue work", async () => {
    const { open, send, decoded, presented, gates } = setup();
    await open();
    for (let seq = 1; seq <= 6; seq++) send(seq);
    expect(decoded).toEqual([1, 2]); // the bound, and no more: the GPU has finished neither
    expect(presented).toHaveLength(MAX_IN_FLIGHT_DECODES);
    expect(gates).toHaveLength(MAX_IN_FLIGHT_DECODES);

    // The first submission completes: the newest frame that arrived meanwhile is decoded next, not
    // the third one, so the picture is never six frames old.
    gates[0].resolve();
    await vi.waitFor(() => expect(decoded).toEqual([1, 2, 6]));
    expect(presented).toHaveLength(3);
  });

  it("decodes every frame when the GPU keeps up", async () => {
    const { open, send, decoded, presented, gates } = setup();
    await open();
    for (let seq = 1; seq <= 6; seq++) {
      send(seq);
      // Each submission completes before the next frame arrives: one frame in flight at a time.
      gates.at(-1)?.resolve();
      await new Promise((r) => setTimeout(r, 0));
    }
    expect(decoded).toEqual([1, 2, 3, 4, 5, 6]);
    expect(presented).toHaveLength(6);
  });

  it("gives a dropped bad frame's slot back, so one malformed frame does not stop the stream", async () => {
    const { open, send, decoded, presented, gates, failWith } = setup();
    await open();
    failWith.error = Object.assign(new Error("bad"), { reason: "bad_frame" });
    send(1);
    expect(decoded).toEqual([]);
    failWith.error = null;
    send(2);
    expect(decoded).toEqual([2]); // the bound was not spent by the dropped frame
    expect(presented).toHaveLength(1);
    expect(gates).toHaveLength(1);
  });

  it("frees the gate when the device is lost: work it abandoned must not hold the last picture", async () => {
    const { open, send, decoded, gates } = setup();
    await open();
    for (let seq = 1; seq <= 4; seq++) send(seq);
    expect(decoded).toEqual([1, 2]);
    gates[0].reject(new Error("device lost"));
    gates[1].reject(new Error("device lost"));
    await new Promise((r) => setTimeout(r, 0));
    // Both slots came back, so the newest waiting frame is decoded (and its own slot then waits).
    expect(decoded).toEqual([1, 2, 4]);
  });

  it("counts frames skipped by the in-flight decode gate, and none while frames are only deferred (#1635)", async () => {
    const { stage, open, send, decoded, gates, droppedCallbacks } = setup();
    await open();

    // Frames 1 and 2 fill the in-flight bound; frame 3 is held pending (not dropped yet).
    send(1);
    send(2);
    send(3);
    expect(stage.inFlightDrops).toBe(0);
    expect(droppedCallbacks).toEqual([]);

    // Frames 4, 5, 6 overwrite pending frames 3, 4, 5 while the bound is full: 3 skipped.
    send(4);
    send(5);
    send(6);
    expect(stage.inFlightDrops).toBe(3);
    expect(droppedCallbacks).toEqual([1, 2, 3]);

    // When a decode finishes, frame 6 decodes and the drop total stays at 3.
    gates[0].resolve();
    await vi.waitFor(() => expect(decoded).toEqual([1, 2, 6]));
    expect(stage.inFlightDrops).toBe(3);

    // A configuration change preserves the cumulative count across the stage's lifetime.
    await open();
    expect(stage.inFlightDrops).toBe(3);
  });
});
