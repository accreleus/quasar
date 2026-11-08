// A PyroWave session through the WebTransport runtime, against the fake agent: the hello offers
// PyroWave, its hello_ack configures the PyroWave stage, datagrams that beat it are held, each
// frame decodes with its coverage and is drawn by the presenter, no keyframe is asked for, and a
// decoder that cannot go on falls back once to the standard codec.

import { afterEach, describe, expect, it, vi } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { blockLayout, parsePwv1 } from "./bitstream";
import { PyroWaveError } from "./bitstream";
import type { Presenter } from "../webtransport/presenter";
import { WebTransportMedia } from "../webtransport/webTransportMedia";
import { createWebTransportRuntime } from "../pages/app/webTransportRuntime";
import { avcC, b64, COORDS, FakeAgent, FakeChunk, FakeDecoder, helloAck, packPyroWave, pyroWaveDatagrams } from "../webtransport/testSupport";

afterEach(() => {
  vi.unstubAllGlobals();
});

const raw = new Uint8Array(readFileSync(resolve(__dirname, "../../../testdata/pyrowave/detail_320x192_xhi.pwv1")));
const { frames } = parsePwv1(raw);
const N = blockLayout(320, 192).totalBlocks;

const pyroAck = (configId: number, reason = "launch") => ({
  type: "hello_ack",
  wt_proto: 1,
  config_id: configId,
  video: {
    codec: "pyrowave",
    description_b64: b64(raw.subarray(0, 24)),
    width: 320,
    height: 192,
    fps: 60,
    color_space: { primaries: "bt709", transfer: "bt709", matrix: "bt709", full_range: false },
  },
  audio: null,
  mic: null,
  fec: null,
  codec_switch_reason: reason,
});

function setup(opts: { offer?: boolean; warm?: boolean } = {}) {
  vi.stubGlobal("EncodedVideoChunk", FakeChunk);
  const agent = new FakeAgent();
  agent.replies = [[pyroAck(0)]];
  const decodes: { packets: Uint8Array[]; covered: Uint8Array }[] = [];
  const textures: unknown[] = [];
  let decodeError: PyroWaveError | null = null;
  const texture = { width: 320, height: 192 } as GPUTexture;
  const fakeModule = {
    PyroWaveDecoder: {
      create: vi.fn(async () => ({
        texture,
        decode: (packets: Uint8Array[], covered: Uint8Array) => {
          if (decodeError) throw decodeError;
          decodes.push({ packets, covered });
          return { received: 0, zero: 0, lost: 0 };
        },
        destroy: () => {},
      })),
    },
  };
  const presenter: Presenter = {
    present: () => true,
    device: {} as GPUDevice,
    presentTexture: (t) => (textures.push(t), true),
    destroy: () => {},
  };
  const h264: FakeDecoder[] = [];
  // The receivers' frame deadlines read this clock: a loaded test host cannot cut a frame short.
  const clock = { now: 1_000 };
  const rt = createWebTransportRuntime({
    sessionId: "s1",
    authToken: "auth",
    coords: COORDS,
    canvas: document.createElement("canvas"),
    pyrowave: opts.offer ?? true,
    callbacks: { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn(), onFallback: vi.fn() },
    deps: {
      createTransport: (o) =>
        new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {}, now: () => clock.now }),
      createPresenter: async () => presenter,
      warmPyroWave: async () => opts.warm ?? true,
      loadPyroWave: async () => fakeModule as never,
      decoder: {
        createDecoder: (init) => {
          const d = new FakeDecoder(init);
          h264.push(d);
          return d as unknown as VideoDecoder;
        },
        isConfigSupported: async (c) => ({ supported: true, config: c }),
      },
    },
  });
  const fail = (e: PyroWaveError) => (decodeError = e);
  return { agent, rt, decodes, textures, h264, fail, clock };
}

const datagrams = (configId: number, frameSeq: number) => pyroWaveDatagrams({ configId, frameSeq }, packPyroWave(frames[0], N));

describe("a PyroWave session in the WebTransport runtime", () => {
  it("offers PyroWave, decodes each frame with its coverage and draws it; a lost datagram is concealed, not NACKed", async () => {
    const { agent, rt, decodes, textures, clock } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    expect(agent.sent("hello")[0]).toMatchObject({ pyrowave: true });
    const d = datagrams(0, 1);
    agent.last.datagrams([...d].reverse());
    await vi.waitFor(() => expect(textures).toHaveLength(1));
    expect(decodes[0].covered.every((c) => c === 1)).toBe(true);
    expect(rt.getSnapshot().firstFrame).toBe(true);
    // A frame short a datagram is drawn at its deadline, its missing range left to concealment.
    agent.last.datagrams(datagrams(0, 2).slice(1));
    await new Promise((r) => setTimeout(r, 40));
    expect(textures).toHaveLength(1);
    clock.now += 17;
    await vi.waitFor(() => expect(textures).toHaveLength(2));
    expect(decodes[1].covered.some((c) => c === 0)).toBe(true);
    // codec-pyrowave.md §Loss: PyroWave loss is concealed, never NACKed or answered with a keyframe.
    expect(agent.sent("keyframe_request")).toEqual([]);
    expect(agent.sent("nack")).toEqual([]);
    rt.destroy();
  });

  it("holds datagrams that beat their hello_ack, then decodes them", async () => {
    const { agent, rt, textures } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.datagrams(datagrams(1, 5));
    await new Promise((r) => setTimeout(r, 30));
    expect(textures).toHaveLength(0);
    agent.last.control(pyroAck(1, "reconnect"));
    await vi.waitFor(() => expect(textures).toHaveLength(1));
    rt.destroy();
  });

  it("a decoder that cannot go on sends codec_fallback once, and the standard codec takes over", async () => {
    const { agent, rt, textures, h264, fail } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.datagrams(datagrams(0, 1));
    await vi.waitFor(() => expect(textures).toHaveLength(1));
    fail(new PyroWaveError("device_lost", "lost"));
    agent.last.datagrams(datagrams(0, 2));
    agent.last.datagrams(datagrams(0, 3));
    await vi.waitFor(() => expect(agent.sent("codec_fallback")).toEqual([{ type: "codec_fallback", reason: "decode_error" }]));
    agent.last.control({ ...helloAck(1, avcC()), codec_switch_reason: "client_decode_error" });
    await vi.waitFor(() => expect(h264[0]?.configs).toHaveLength(1));
    expect(agent.sent("codec_fallback")).toHaveLength(1);
    rt.destroy();
  });

  it("a browser whose decoder cannot run does not offer PyroWave", async () => {
    const { agent, rt } = setup({ warm: false });
    agent.replies = [[helloAck(0, avcC())]];
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    expect(agent.sent("hello")[0].pyrowave).toBeUndefined();
    rt.destroy();
  });
});
