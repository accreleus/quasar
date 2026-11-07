// Input and liveness over WebTransport (#1032): the input.md JSON on the contract's two
// paths, and the control stream's ping/pong. Fake browser boundary, real capture.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { dataChannelSender, type InputMsg } from "../input/inputSender";
import { createWebTransportRuntime } from "../pages/app/webTransportRuntime";
import type { TelemetrySnapshot } from "../webrtc/telemetry";
import { CHANNEL_INPUT } from "./framing";
import { webTransportInputSender, type InputWire } from "./inputSender";
import type { Presenter } from "./presenter";
import type { MediaSink } from "./transport";
import { avcC, COORDS, FakeAgent, FakeDecoder, flush, helloAck } from "./testSupport";
import { WebTransportMedia } from "./webTransportMedia";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Object.defineProperty(document, "pointerLockElement", { value: null, configurable: true });
});

const text = (b: Uint8Array) => new TextDecoder().decode(b);

/** A wire that records which path each message took. */
function recordingWire(datagramRoom = true) {
  const log: { path: "reliable" | "datagram"; json: string }[] = [];
  const wire: InputWire = {
    reliable: (e) => log.push({ path: "reliable", json: JSON.stringify(e) }),
    datagram: (b) => {
      if (!datagramRoom) return false;
      expect(b[0]).toBe(CHANNEL_INPUT);
      log.push({ path: "datagram", json: text(b.subarray(1)) });
      return true;
    },
  };
  return { log, sender: webTransportInputSender(wire) };
}

describe("the WebTransport input sender", () => {
  it("sends presses and releases reliably and continuous input as datagrams", () => {
    const { log, sender } = recordingWire();
    const msgs: InputMsg[] = [
      { t: "mm", dx: 4, dy: -2 },
      { t: "ma", x: 0.5, y: 0.25 },
      { t: "ms", dx: 0, dy: 120 },
      { t: "k", code: 30, pressed: true },
      { t: "k", code: 30, pressed: false },
      { t: "mb", button: 272, pressed: true },
    ];
    msgs.forEach((m) => sender.send(m));
    expect(log.map((l) => l.path)).toEqual(["datagram", "datagram", "datagram", "reliable", "reliable", "reliable"]);
  });

  it("carries the same JSON the DataChannel carries, byte for byte", () => {
    const sent: string[] = [];
    const channel = dataChannelSender({ readyState: "open", send: (s: string) => sent.push(s) } as unknown as RTCDataChannel);
    const { log, sender } = recordingWire();
    const msgs: InputMsg[] = [
      { t: "mm", dx: 4, dy: -2, seq: 17, tc: 123456.7 },
      { t: "ms", dx: 0, dy: -120 },
      { t: "k", code: 1, pressed: true },
    ];
    for (const m of msgs) {
      channel.send(m);
      sender.send(m);
    }
    expect(log.map((l) => l.json)).toEqual(sent);
  });

  it("sends a gamepad state reliably when a button changes and as a datagram when only an axis does", () => {
    const { log, sender } = recordingWire();
    const pad = (buttons: number[], axes: number[]): InputMsg => ({ t: "gp", i: 0, buttons, axes });
    sender.send(pad([0, 0, 0], [0, 0])); // first state of a pad
    sender.send(pad([0, 0, 0], [0.4, 0])); // stick
    sender.send(pad([0, 0.3, 0], [0.4, 0.1])); // analog value below the press threshold
    sender.send(pad([0, 1, 0], [0.4, 0.1])); // press
    sender.send(pad([0, 1, 0], [0.9, 0.1])); // stick, button still held
    sender.send(pad([0, 0, 0], [0.9, 0.1])); // release
    expect(log.map((l) => l.path)).toEqual(["reliable", "datagram", "datagram", "reliable", "datagram", "reliable"]);
  });

  it("falls back to the control stream when the datagram does not fit", () => {
    const { log, sender } = recordingWire(false);
    sender.send({ t: "mm", dx: 1, dy: 1 });
    expect(log.map((l) => l.path)).toEqual(["reliable"]);
    const big = recordingWire();
    big.sender.send({ t: "gp", i: 0, buttons: [], axes: [] });
    big.sender.send({ t: "gp", i: 0, buttons: [], axes: new Array(400).fill(0.123456789) });
    expect(big.log.map((l) => l.path)).toEqual(["reliable", "reliable"]);
  });
});

describe("WebTransportMedia input and ping", () => {
  const NO_SINK: MediaSink = { config: () => {}, unit: () => {}, end: () => {} };
  function connected(sink: MediaSink = NO_SINK, now?: () => number) {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const t = new WebTransportMedia({
      sessionId: "s1",
      coords: COORDS,
      mintToken: async () => COORDS,
      createWebTransport: agent.create,
      sleep: async () => {},
      now,
    });
    return { agent, t, up: t.connect(sink) };
  }

  it("writes input on the control stream as {type:input,event} and on the datagram channel as 0x03 + JSON", async () => {
    const { agent, t, up } = connected();
    await up;
    const key = { t: "k", code: 30, pressed: false };
    t.input.reliable(key);
    expect(t.input.datagram(Uint8Array.from([CHANNEL_INPUT, 0x7b, 0x7d]))).toBe(true);
    await flush();
    expect(agent.sent("input")).toEqual([{ type: "input", event: key }]);
    expect(agent.datagramsSent.map((d) => Array.from(d))).toEqual([[3, 0x7b, 0x7d]]);
    t.close();
  });

  it("refuses a datagram over the 1148-byte floor and any input before the connection is up", async () => {
    const { t, up } = connected();
    expect(t.input.datagram(new Uint8Array(10))).toBe(false);
    await up;
    expect(t.input.datagram(new Uint8Array(1148))).toBe(true);
    expect(t.input.datagram(new Uint8Array(1149))).toBe(false);
    t.close();
  });

  it("pings once a second and reports each pong's round trip", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    let clock = 1000;
    const rtts: number[] = [];
    const { agent, t, up } = connected({ ...NO_SINK, rtt: (ms) => rtts.push(ms) }, () => clock);
    await up;
    await flush();
    expect(agent.sent("ping")).toEqual([{ type: "ping", id: 1, tc: 1000 }]);

    clock = 1042;
    agent.last.control({ type: "pong", id: 1, tc: 1000, ts: 5, tw: 1762400000000 });
    await flush();
    expect(rtts).toEqual([42]);

    clock = 2000;
    vi.advanceTimersByTime(1000);
    await flush();
    expect(agent.sent("ping").map((p) => [p.id, p.tc])).toEqual([[1, 1000], [2, 2000]]);
    t.close();
    vi.advanceTimersByTime(5000);
    await flush();
    expect(agent.sent("ping")).toHaveLength(2);
  });

  it("answers the agent's ping with its fields echoed and its own clock", async () => {
    const { agent, t, up } = connected(NO_SINK, () => 777);
    await up;
    agent.last.control({ type: "ping", id: 4, ts: 998880.0, tw: 1762400003001.2 });
    await vi.waitFor(() => expect(agent.sent("pong")).toEqual([{ type: "pong", id: 4, ts: 998880.0, tw: 1762400003001.2, tc: 777 }]));
    t.close();
  });
});

describe("input through the WebTransport runtime", () => {
  beforeEach(() => {
    vi.stubGlobal("requestAnimationFrame", vi.fn(() => 1));
    vi.stubGlobal("cancelAnimationFrame", vi.fn());
    vi.stubGlobal(
      "PointerEvent",
      class extends Event {
        movementX: number;
        movementY: number;
        constructor(type: string, init: EventInit & { movementX?: number; movementY?: number } = {}) {
          super(type, init);
          this.movementX = init.movementX ?? 0;
          this.movementY = init.movementY ?? 0;
        }
        getCoalescedEvents() {
          return [];
        }
      },
    );
  });

  function setup() {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const canvas = document.createElement("canvas");
    const presenter: Presenter = { present: () => true, destroy: () => {} };
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn() };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas,
      callbacks,
      deps: {
        createTransport: (o) => new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {} }),
        createPresenter: async () => presenter,
        decoder: {
          createDecoder: (init) => new FakeDecoder(init) as unknown as VideoDecoder,
          isConfigSupported: async (c) => ({ supported: true, config: c }),
        },
      },
    });
    return { agent, canvas, rt };
  }

  /** Pointer Lock granted to `el`, as the browser would. */
  function lock(el: Element) {
    Object.defineProperty(document, "pointerLockElement", { value: el, configurable: true });
    document.dispatchEvent(new Event("pointerlockchange"));
  }

  it("sends a locked mouse move as a datagram, a key as control, and lifts the held key before the bye", async () => {
    const { agent, canvas, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    expect(canvas.dataset.streamW).toBe("1920");

    lock(canvas);
    expect(rt.getSnapshot()).toMatchObject({ inputCaptured: true, pointerLocked: true });
    expect(rt.isCaptured()).toBe(true);

    document.dispatchEvent(new PointerEvent("pointermove", { bubbles: true, movementX: 3, movementY: -5 }));
    document.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, code: "KeyA" }));
    await flush();
    expect(agent.datagramsSent.map((d) => [d[0], text(d.subarray(1))])).toEqual([[3, '{"t":"mm","dx":3,"dy":-5}']]);
    expect(agent.sent("input").map((m) => m.event)).toEqual([{ t: "k", code: 30, pressed: true }]);

    rt.destroy();
    await flush();
    // The held key is released through the transport, and the bye follows it.
    const order = agent.received.filter((m) => m.type === "input" || m.type === "bye").map((m) => m.type === "bye" ? "bye" : JSON.stringify(m.event));
    expect(order).toEqual([JSON.stringify({ t: "k", code: 30, pressed: true }), JSON.stringify({ t: "k", code: 30, pressed: false }), "bye"]);
  });

  it("shows the round trip in the overlay's telemetry", async () => {
    const { agent, rt } = setup();
    const snaps: TelemetrySnapshot[] = [];
    rt.registerTelemetry((s) => snaps.push(s));
    rt.start();
    await vi.waitFor(() => expect(agent.sent("ping")).toHaveLength(1));
    const { tc } = agent.sent("ping")[0];
    agent.last.control({ type: "pong", id: 1, tc, ts: 1, tw: 2 });
    await vi.waitFor(() => expect(snaps.at(-1)?.rttMs).toEqual(expect.any(Number)), { timeout: 3000 });
    expect(snaps.at(-1)!.rttMs).toBeGreaterThanOrEqual(0);
    rt.destroy();
  });
});
