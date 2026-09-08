// #526 / #128 — how QuasarSession reads a WebSocket close code.
//
// Three arms in `ws.onclose`, and getting any of them wrong is invisible
// without a real second tab or a real control-plane restart, so they are pinned
// here at the boundary with the browser objects stubbed:
//
//   4410  takeover -> `superseded`. Terminal, no escalation. Escalating it
//         re-attaches with a new token and displaces the tab that just
//         displaced us, forever (#526).
//   4401  token refused -> `failed`. A rebind would mint another token and be
//         refused the same way, so re-attaching cannot help (#128).
//   else  -> `signaling-lost`. NON-terminal (#128). Media and input are
//         agent<->browser and keep flowing while the control plane is away, so
//         the runtime re-attaches signalling in place. This arm used to be
//         `failed`, which made the runtime re-seat its coords and destroy the
//         peer connection that was still carrying the stream — the session
//         survived the outage and was killed by its own recovery.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { QuasarSession, WS_CLOSE_TAKEN_OVER, WS_CLOSE_TOKEN_REJECTED } from "./session";
import type { RecoveryState } from "./recovery";

/** The minimum of RTCPeerConnection QuasarSession's constructor touches. */
let lastPc: StubPeerConnection | null = null;

class StubPeerConnection {
  constructor() {
    lastPc = this;
  }
  iceConnectionState = "new";
  connectionState = "new";
  ontrack: unknown = null;
  ondatachannel: unknown = null;
  onicecandidate: unknown = null;
  oniceconnectionstatechange: unknown = null;
  onconnectionstatechange: unknown = null;
  addTransceiver() {
    return { mid: null };
  }
  getTransceivers() {
    return [];
  }
  close() {}
}

/** A WebSocket that never connects and exposes its handlers to the test. */
class StubWebSocket {
  static last: StubWebSocket | null = null;
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((e: { code: number }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((e: MessageEvent<string>) => void) | null = null;
  constructor(public url: string) {
    StubWebSocket.last = this;
  }
  send() {}
  close() {}
}

function startSession(): {
  states: RecoveryState[];
  ws: StubWebSocket;
  session: QuasarSession;
  pc: StubPeerConnection;
} {
  const states: RecoveryState[] = [];
  const session = new QuasarSession(
    "wss://cp.test/v1/signal",
    "tok",
    () => {},
    () => {},
    () => {},
    undefined,
    (state) => states.push(state),
  );
  const ws = StubWebSocket.last!;
  return { states, ws, session, pc: lastPc! };
}

const g = globalThis as unknown as Record<string, unknown>;
let realPc: unknown;
let realWs: unknown;

beforeEach(() => {
  realPc = g.RTCPeerConnection;
  realWs = g.WebSocket;
  g.RTCPeerConnection = StubPeerConnection;
  g.WebSocket = StubWebSocket;
  StubWebSocket.last = null;
});

afterEach(() => {
  g.RTCPeerConnection = realPc;
  g.WebSocket = realWs;
});

describe("QuasarSession WebSocket close handling (#526)", () => {
  it("maps the takeover code to `superseded`, not `failed`", () => {
    const { states, ws } = startSession();
    ws.onclose!({ code: WS_CLOSE_TAKEN_OVER });

    expect(states.at(-1)?.phase).toBe("superseded");
    expect(states.map((s) => s.phase)).not.toContain("failed");
  });

  // #128: an ordinary close is a SIGNALLING fault, not a session fault. `failed`
  // is the media verdict and is what used to destroy a healthy peer connection.
  it("maps an ordinary close to `signaling-lost`, never `failed`", () => {
    const { states, ws } = startSession();
    ws.onclose!({ code: 1006 });

    expect(states.at(-1)?.phase).toBe("signaling-lost");
    expect(states.map((s) => s.phase)).not.toContain("failed");
  });

  it("maps a known contract close code to `signaling-lost`, keeping its reason", () => {
    const { states, ws } = startSession();
    ws.onclose!({ code: 4500 }); // relay unavailable — host offline

    expect(states.at(-1)).toMatchObject({ phase: "signaling-lost" });
    expect(states.at(-1)?.message).toContain("host offline");
    expect(states.map((s) => s.phase)).not.toContain("failed");
  });

  // The one close code re-attaching cannot fix: a refused token stays refused.
  it("keeps a refused token terminal", () => {
    const { states, ws } = startSession();
    ws.onclose!({ code: WS_CLOSE_TOKEN_REJECTED });

    expect(states.at(-1)?.phase).toBe("failed");
    expect(states.map((s) => s.phase)).not.toContain("signaling-lost");
  });

  // #128 MB2: `connectionState` goes `failed` when ANY transport fails, ICE
  // included. Without the ICE guard this terminalises every ICE failure before
  // the bounded in-place restart ladder gets its first attempt, turning a
  // recoverable blip into a teardown.
  it("does not terminalise an ICE failure through the connection-state arm", () => {
    const { states, pc } = startSession();
    pc.iceConnectionState = "failed";
    (pc.oniceconnectionstatechange as () => void)();
    pc.connectionState = "failed";
    (pc.onconnectionstatechange as () => void)();

    expect(states.map((s) => s.phase)).not.toContain("failed");
    expect(states.at(-1)?.phase).toBe("degraded");
  });

  it("terminalises a DTLS failure, which an ICE restart cannot fix", () => {
    const { states, pc } = startSession();
    pc.connectionState = "failed"; // ICE is NOT failed
    (pc.onconnectionstatechange as () => void)();

    expect(states.at(-1)?.phase).toBe("failed");
    expect(states.at(-1)?.message).toContain("DTLS");
  });

  // Media health is independent: ICE reporting connected must not paper over an
  // outstanding signalling outage.
  it("does not clear a signalling outage when the media path reports connected", () => {
    const { states, ws, session } = startSession();
    ws.onclose!({ code: 1006 });
    session.mediaPathFlowing();

    expect(states.at(-1)?.phase).toBe("signaling-lost");
  });
});
