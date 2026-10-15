// #610 — Firefox's RTCRtpReceiver.getParameters() has no headerExtensions. The
// probe runs inside every telemetry poll, so a throw there stopped all stats.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { QuasarSession } from "./session";

let receivers: unknown[] = [];

class StubPeerConnection {
  addTransceiver() {
    return { mid: null };
  }
  getTransceivers() {
    return [];
  }
  getReceivers() {
    return receivers;
  }
  close() {}
}

class StubWebSocket {
  readyState = 0;
  send() {}
  close() {}
}

const URI = "http://www.webrtc.org/experiments/rtp-hdrext/abs-capture-time";
const g = globalThis as unknown as Record<string, unknown>;
let realPc: unknown;
let realWs: unknown;

beforeEach(() => {
  realPc = g.RTCPeerConnection;
  realWs = g.WebSocket;
  g.RTCPeerConnection = StubPeerConnection;
  g.WebSocket = StubWebSocket;
});

afterEach(() => {
  g.RTCPeerConnection = realPc;
  g.WebSocket = realWs;
});

function probe(parameters: unknown): boolean {
  receivers = [{ track: { kind: "video" }, getParameters: () => parameters }];
  const session = new QuasarSession("wss://cp.test/v1/signal", "tok", () => {}, () => {}, () => {});
  return session.hasAbsCaptureTimeExtension(URI);
}

describe("QuasarSession.hasAbsCaptureTimeExtension (#610)", () => {
  it("finds the extension when the browser lists it", () => {
    expect(probe({ headerExtensions: [{ uri: URI }] })).toBe(true);
  });

  it("answers false when the extension is not listed", () => {
    expect(probe({ headerExtensions: [] })).toBe(false);
  });

  it("answers false, not a throw, when the browser reports no headerExtensions", () => {
    expect(probe({ codecs: [] })).toBe(false);
  });

  it("answers false when getParameters returns nothing", () => {
    expect(probe(undefined)).toBe(false);
  });
});
