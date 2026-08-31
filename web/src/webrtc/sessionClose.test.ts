// #526 — how QuasarSession reads a WebSocket close code.
//
// The whole fix hangs off one branch in `ws.onclose`: the takeover code 4410
// goes to `recovery.superseded()` (terminal, no escalation), and EVERYTHING
// else still goes to `recovery.terminal()` (phase `failed`, which sessionRuntime
// answers by minting a replacement token). Getting that backwards either
// restores the two-tab displacement loop or breaks recovery from an ordinary
// network blip, and neither is visible without a real second tab. So it is
// pinned here, at the boundary, with the browser objects stubbed.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { QuasarSession, WS_CLOSE_TAKEN_OVER } from "./session";
import type { RecoveryState } from "./recovery";

/** The minimum of RTCPeerConnection QuasarSession's constructor touches. */
class StubPeerConnection {
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

function startSession(): { states: RecoveryState[]; ws: StubWebSocket } {
  const states: RecoveryState[] = [];
  new QuasarSession(
    "wss://cp.test/v1/signal",
    "tok",
    () => {},
    () => {},
    () => {},
    undefined,
    (state) => states.push(state),
  );
  const ws = StubWebSocket.last!;
  return { states, ws };
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

  // The regression that made the loop possible: the control plane used to close
  // a displaced socket with 1000, which is indistinguishable from a hang-up, so
  // this arm fired and the tab re-minted.
  it("still escalates an ordinary close to `failed`", () => {
    const { states, ws } = startSession();
    ws.onclose!({ code: 1006 });

    expect(states.at(-1)?.phase).toBe("failed");
  });

  it("still escalates a known contract close code to `failed`", () => {
    const { states, ws } = startSession();
    ws.onclose!({ code: 4500 }); // relay unavailable — host offline

    expect(states.at(-1)).toMatchObject({ phase: "failed" });
    expect(states.at(-1)?.message).toContain("host offline");
  });
});
