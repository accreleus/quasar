// #535 — how QuasarSession handles offers that arrive close together on one pc.
//
// The host can send two offers for a pc a few ms apart: a replaced transport's
// offer and a re-offer. signaling.md §Mid-session reconnection: offers for a pc
// are applied one at a time, and the answer to an offer a later one superseded
// is not sent. The browser objects are stubbed so every step of the
// negotiation settles only when the test says so.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QuasarSession } from "./session";

/** Let the session's promise chains run up to their next pending step. */
const settle = () => new Promise<void>((r) => setTimeout(r));

/** A peer connection whose negotiation steps stay pending until `step()`. */
class StubPeerConnection {
  static all: StubPeerConnection[] = [];
  /** Every step in the order the session called it. */
  calls: string[] = [];
  private pending: Array<{ open: () => void; fail: (e: Error) => void }> = [];
  private remote = "";
  iceConnectionState = "new";
  connectionState = "new";
  ontrack: unknown = null;
  ondatachannel: unknown = null;
  onicecandidate: unknown = null;
  oniceconnectionstatechange: unknown = null;
  onconnectionstatechange: unknown = null;
  constructor() {
    StubPeerConnection.all.push(this);
  }
  private gate<T>(name: string, value: () => T): Promise<T> {
    this.calls.push(name);
    return new Promise<T>((resolve, reject) => {
      this.pending.push({ open: () => resolve(value()), fail: reject });
    });
  }
  setRemoteDescription(d: { sdp: string }) {
    return this.gate(`setRemote ${d.sdp}`, () => {
      this.remote = d.sdp;
    });
  }
  createAnswer() {
    return this.gate("createAnswer", () => ({ type: "answer", sdp: `answer to ${this.remote}` }));
  }
  setLocalDescription(d: { sdp: string }) {
    return this.gate(`setLocal ${d.sdp}`, () => {});
  }
  addIceCandidate(c: { candidate: string }) {
    return this.gate(`candidate ${c.candidate}`, () => {});
  }
  transceivers: Array<{ mid: string; direction: string }> = [];
  getTransceivers() {
    return this.transceivers;
  }
  close() {}
  /** Settle the oldest pending step, as a failure when `error` is given. */
  async step(error?: Error): Promise<void> {
    const next = this.pending.shift();
    if (!next) throw new Error(`nothing pending after ${this.calls.join(", ")}`);
    if (error) next.fail(error);
    else next.open();
    await settle();
  }
  /** Settle steps until none is pending. */
  async drain(): Promise<void> {
    await settle();
    while (this.pending.length > 0) await this.step();
  }
}

class StubWebSocket {
  static OPEN = 1;
  static last: StubWebSocket | null = null;
  readyState = StubWebSocket.OPEN;
  sent: Array<{ type: string; pc?: string; sdp?: string }> = [];
  onopen: (() => void) | null = null;
  onclose: ((e: { code: number }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((e: MessageEvent<string>) => void) | null = null;
  constructor(public url: string) {
    StubWebSocket.last = this;
  }
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {}
  deliver(msg: unknown) {
    this.onmessage!({ data: JSON.stringify(msg) } as MessageEvent<string>);
  }
  answers(): string[] {
    return this.sent.filter((m) => m.type === "answer").map((m) => `${m.pc}: ${m.sdp}`);
  }
}

function startSession() {
  const statuses: string[] = [];
  const phases: string[] = [];
  const session = new QuasarSession(
    "wss://cp.test/v1/signal",
    "tok",
    () => {},
    (s) => statuses.push(s),
    () => {},
    undefined,
    (state) => phases.push(state.phase),
  );
  return { session, statuses, phases, ws: StubWebSocket.last!, video: StubPeerConnection.all[0] };
}

const offer = (sdp: string, pc = "video") => ({ type: "offer", pc, sdp });
const candidate = (name: string, pc = "video") => ({ type: "ice", pc, candidate: { candidate: name } });

/** An audio offer with the host's send m-line and a recvonly one for the microphone. */
const micOffer = (session: string) =>
  [
    "v=0",
    `o=- ${session} 0 IN IP4 0.0.0.0`,
    "m=audio 9 UDP/TLS/RTP/SAVPF 111",
    "a=mid:audio0",
    "a=sendonly",
    "m=audio 9 UDP/TLS/RTP/SAVPF 111",
    "a=mid:audio1",
    "a=recvonly",
    "",
  ].join("\r\n");

const g = globalThis as unknown as Record<string, unknown>;
let realPc: unknown;
let realWs: unknown;

beforeEach(() => {
  realPc = g.RTCPeerConnection;
  realWs = g.WebSocket;
  g.RTCPeerConnection = StubPeerConnection;
  g.WebSocket = StubWebSocket;
  StubPeerConnection.all = [];
  StubWebSocket.last = null;
});

afterEach(() => {
  g.RTCPeerConnection = realPc;
  g.WebSocket = realWs;
  vi.restoreAllMocks();
});

describe("QuasarSession offers for one pc (#535)", () => {
  it("answers an offer", async () => {
    const { ws, video } = startSession();
    ws.deliver(offer("A"));
    await video.drain();

    expect(video.calls).toEqual(["setRemote A", "createAnswer", "setLocal answer to A"]);
    expect(ws.answers()).toEqual(["video: answer to A"]);
  });

  it("applies a second offer only once the first has been dealt with", async () => {
    const { ws, video } = startSession();
    ws.deliver(offer("A"));
    ws.deliver(offer("B"));
    await settle();

    expect(video.calls).toEqual(["setRemote A"]);
    await video.drain();
    expect(video.calls.indexOf("setRemote B")).toBeGreaterThan(video.calls.indexOf("setRemote A"));
    expect(video.calls.filter((c) => c.startsWith("setRemote"))).toHaveLength(2);
  });

  // The later offer arrives while the earlier one is waiting on each of its
  // three steps in turn.
  it.each([
    { during: "setRemoteDescription", settled: 0 },
    { during: "createAnswer", settled: 1 },
    { during: "setLocalDescription", settled: 2 },
  ])("sends one answer, to the later offer, when it arrives during $during", async ({ settled }) => {
    const { ws, video, statuses } = startSession();
    ws.deliver(offer("A"));
    await settle();
    for (let i = 0; i < settled; i++) await video.step();

    ws.deliver(offer("B"));
    await video.drain();

    expect(ws.answers()).toEqual(["video: answer to B"]);
    expect(statuses.filter((s) => s.includes("negotiation failed"))).toEqual([]);
  });

  it("answers only the last of three offers that arrive together", async () => {
    const { ws, video } = startSession();
    ws.deliver(offer("A"));
    ws.deliver(offer("B"));
    ws.deliver(offer("C"));
    await video.drain();

    expect(video.calls.filter((c) => c.startsWith("setRemote"))).toEqual([
      "setRemote A",
      "setRemote B",
      "setRemote C",
    ]);
    expect(ws.answers()).toEqual(["video: answer to C"]);
  });

  it("still answers the next offer when one fails", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const { ws, video, statuses } = startSession();
    ws.deliver(offer("A"));
    await settle();
    await video.step(new Error("bad sdp"));

    ws.deliver(offer("B"));
    await video.drain();

    expect(statuses.filter((s) => s.includes("negotiation failed"))).toHaveLength(1);
    expect(ws.answers()).toEqual(["video: answer to B"]);
  });

  // Applied earlier, the candidate would land on the transport that offer is
  // about to replace.
  it("adds a candidate after the offer it followed", async () => {
    const { ws, video } = startSession();
    ws.deliver(offer("A"));
    ws.deliver({ type: "ice", pc: "video", candidate: { candidate: "c1" } });
    await settle();

    expect(video.calls).toEqual(["setRemote A"]);
    await video.drain();
    expect(video.calls).toEqual(["setRemote A", "createAnswer", "setLocal answer to A", "candidate c1"]);
  });

  it("does not hold the audio pc behind the video pc", async () => {
    const { ws, video } = startSession();
    ws.deliver(offer("A"));
    ws.deliver(offer("M", "audio"));
    await settle();

    const audio = StubPeerConnection.all[1];
    expect(video.calls).toEqual(["setRemote A"]);
    expect(audio.calls).toEqual(["setRemote M"]);
    await audio.drain();
    expect(ws.answers()).toEqual(["audio: answer to M"]);
  });

  it("answers both offers when the second arrives after the first answer went out", async () => {
    const { ws, video } = startSession();
    ws.deliver(offer("A"));
    await video.drain();
    ws.deliver(offer("B"));
    await video.drain();

    expect(ws.answers()).toEqual(["video: answer to A", "video: answer to B"]);
  });

  // Offer A is in flight when its candidate, the offer that supersedes it and that
  // offer's candidate arrive. A re-offer of the same transport shares A's candidates, so
  // A's is still added; each lands after the offer it followed and before the next one.
  it("keeps every candidate behind its own offer, a superseded offer's included", async () => {
    const { ws, video } = startSession();
    ws.deliver(offer("A"));
    ws.deliver(candidate("for A"));
    ws.deliver(offer("B"));
    ws.deliver(candidate("for B"));
    await video.drain();

    expect(video.calls).toEqual([
      "setRemote A",
      "candidate for A",
      "setRemote B",
      "createAnswer",
      "setLocal answer to B",
      "candidate for B",
    ]);
    expect(ws.answers()).toEqual(["video: answer to B"]);
  });

  it("carries on when a candidate is refused", async () => {
    const { ws, video } = startSession();
    ws.deliver(offer("A"));
    await video.drain();
    ws.deliver(candidate("refused"));
    ws.deliver(offer("B"));
    await settle();

    await video.step(new Error("no such m-line"));
    await video.drain();

    expect(ws.answers()).toEqual(["video: answer to A", "video: answer to B"]);
  });

  // The slot claimed on the first audio offer is the one the next offer names: claiming
  // again would be a second write to a transceiver the answer already settled.
  it("leaves the microphone claim in place on a second audio offer", async () => {
    const { ws, session } = startSession();
    let claims = 0;
    const mic = {
      mid: "audio1",
      held: "recvonly",
      get direction() {
        return this.held;
      },
      set direction(to: string) {
        claims += 1;
        this.held = to;
      },
    };
    ws.deliver(offer(micOffer("1"), "audio"));
    await settle();
    const audio = StubPeerConnection.all[1];
    audio.transceivers = [{ mid: "audio0", direction: "recvonly" }, mic];
    await audio.drain();
    expect(mic.direction).toBe("sendonly");

    ws.deliver(offer(micOffer("2"), "audio"));
    await audio.drain();

    expect(claims).toBe(1);
    expect(mic.direction).toBe("sendonly");
    expect(session.hasMicSlot()).toBe(true);
    expect(ws.answers()).toHaveLength(2);
  });

  // Recovery asks for a restart while an offer is still being applied. The request goes
  // out at once; the offer it brings waits its turn and supersedes the one in flight.
  it("sends restart_ice at once while an offer is being applied", async () => {
    const { ws, video, session } = startSession();
    ws.deliver(offer("A"));
    await settle();

    session.recoverMediaPath();
    await settle();
    expect(ws.sent.map((m) => m.type)).toEqual(["restart_ice"]);
    expect(video.calls).toEqual(["setRemote A"]);

    ws.deliver(offer("B"));
    await video.drain();
    session.close(false);

    expect(ws.sent.map((m) => m.type)).toEqual(["restart_ice", "answer"]);
    expect(ws.answers()).toEqual(["video: answer to B"]);
  });

  // A later offer does not excuse a DTLS failure: the pc has one DTLS transport, and
  // the offers of a handoff keep it. (On the old transport's offer a taking page never
  // starts ICE, so DTLS cannot fail there: harness scenario `spa-refused`.)
  it("keeps a DTLS failure terminal after a later offer was applied", async () => {
    const { ws, video, phases } = startSession();
    ws.deliver(offer("A"));
    await video.drain();
    ws.deliver(offer("B"));
    await video.drain();

    video.connectionState = "failed";
    (video.onconnectionstatechange as () => void)();

    expect(phases.at(-1)).toBe("failed");
  });

  it("drops what is still queued when the session closes", async () => {
    const { ws, video, session } = startSession();
    ws.deliver(offer("A"));
    ws.deliver(offer("B"));
    await settle();

    session.close(false);
    await video.drain();

    expect(video.calls.filter((c) => c.startsWith("setRemote"))).toEqual(["setRemote A"]);
  });
});

describe("QuasarSession — the host's bye (#753)", () => {
  it("reports it once, and nothing else does", async () => {
    const { session, ws, video } = startSession();
    const bye = vi.fn();
    session.onHostBye = bye;
    ws.deliver(offer("A"));
    ws.deliver({ type: "error", message: "x" });
    await video.drain();
    expect(bye).not.toHaveBeenCalled();
    ws.deliver({ type: "bye" });
    expect(bye).toHaveBeenCalledTimes(1);
  });
});
