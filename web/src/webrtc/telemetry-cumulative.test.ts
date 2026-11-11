// #1270: the stats post carries packets_lost, frames_dropped and freeze_count cumulative
// (protocol/openapi.yaml BrowserMetrics); the HUD's snapshot keeps the per-window counts.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { newPostedCounters, SessionTelemetry, type PostedCounters, type TelemetrySnapshot } from "./telemetry";

type Totals = { packetsLost: number; framesDropped: number; freezeCount: number };

/** A telemetry whose getStats answers with `totals.now`, as one peer connection's receiver does. */
function telemetry(totals: { now: Totals; audio?: { concealedSamples: number; concealmentEvents: number } }, counters?: PostedCounters) {
  const video = { videoWidth: 0, videoHeight: 0, requestVideoFrameCallback: () => 1 } as unknown as HTMLVideoElement;
  const channel = { readyState: "open", bufferedAmount: 0, onmessage: null, send: () => {} } as unknown as RTCDataChannel;
  const stats = async () =>
    new Map<string, unknown>([
      ["rtp", { type: "inbound-rtp", kind: "video", ...totals.now }],
      ...(totals.audio ? [["audio", { type: "inbound-rtp", kind: "audio", ...totals.audio }] as [string, unknown]] : []),
    ]) as unknown as RTCStatsReport;
  const t = new SessionTelemetry(video, stats, channel, "s1", "token", undefined, undefined, undefined, undefined, counters);
  const windows: Pick<TelemetrySnapshot, "packetsLost" | "framesDropped" | "freezeCount">[] = [];
  t.onUpdate(({ packetsLost, framesDropped, freezeCount }) => windows.push({ packetsLost, framesDropped, freezeCount }));
  t.start();
  return { t, windows };
}

let posts: Record<string, number>[];
const posted = (key: string) => posts.map((m) => m[key]);
const second = (totals: { now: Totals }, next: Totals) => {
  totals.now = next;
  return vi.advanceTimersByTimeAsync(1_000);
};

beforeEach(() => {
  vi.useFakeTimers();
  posts = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: { body: string }) => {
      if (url.endsWith("/stats")) {
        for (const s of (JSON.parse(init.body) as { samples: { metrics: Record<string, number> }[] }).samples) posts.push(s.metrics);
      }
      return new Response(null, { status: 204 });
    }),
  );
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("SessionTelemetry cumulative counters", () => {
  it("posts the running totals, and gives the HUD each window's own count", async () => {
    const totals = { now: { packetsLost: 5, framesDropped: 1, freezeCount: 0 } };
    const { t, windows } = telemetry(totals);
    await vi.advanceTimersByTimeAsync(1_000);
    await second(totals, { packetsLost: 8, framesDropped: 1, freezeCount: 1 });
    await second(totals, { packetsLost: 8, framesDropped: 4, freezeCount: 1 });
    await vi.advanceTimersByTimeAsync(2_000);
    t.stop();
    expect(posted("packets_lost").slice(0, 3)).toEqual([5, 8, 8]);
    expect(posted("frames_dropped").slice(0, 3)).toEqual([1, 1, 4]);
    expect(posted("freeze_count").slice(0, 3)).toEqual([0, 1, 1]);
    expect(windows.slice(0, 3)).toEqual([
      { packetsLost: 5, framesDropped: 1, freezeCount: 0 },
      { packetsLost: 3, framesDropped: 0, freezeCount: 1 },
      { packetsLost: 0, framesDropped: 3, freezeCount: 0 },
    ]);
  });

  it("never falls when the receiver's count dips (a resent packet arrived), and counts no loss twice on the way back up", async () => {
    const counters = newPostedCounters();
    const totals = { now: { packetsLost: 8, framesDropped: 4, freezeCount: 2 } };
    const first = telemetry(totals, counters);
    await vi.advanceTimersByTimeAsync(1_000);
    await second(totals, { packetsLost: 6, framesDropped: 4, freezeCount: 2 });
    await second(totals, { packetsLost: 10, framesDropped: 6, freezeCount: 3 });
    await vi.advanceTimersByTimeAsync(2_000);
    first.t.stop();
    expect(posted("packets_lost").slice(0, 3)).toEqual([8, 8, 10]);

    // A replacement peer connection: a new telemetry on the page view's counters.
    posts = [];
    const next = { now: { packetsLost: 2, framesDropped: 1, freezeCount: 1 } };
    const replacement = telemetry(next, counters);
    await vi.advanceTimersByTimeAsync(5_000);
    replacement.t.stop();
    expect(posted("packets_lost")[0]).toBe(12);
    expect(posted("frames_dropped")[0]).toBe(7);
    expect(posted("freeze_count")[0]).toBe(4);
  });

  it("posts the browser's audio concealment cumulative across a replaced peer connection, and nothing for a session without audio", async () => {
    const counters = newPostedCounters();
    const quiet = { now: { packetsLost: 0, framesDropped: 0, freezeCount: 0 } };
    const none = telemetry(quiet, counters);
    await vi.advanceTimersByTimeAsync(5_000);
    none.t.stop();
    expect(posts[0]).not.toHaveProperty("audio_concealed_samples");

    posts = [];
    const totals = { now: quiet.now, audio: { concealedSamples: 480, concealmentEvents: 1 } };
    const first = telemetry(totals, counters);
    await vi.advanceTimersByTimeAsync(1_000);
    totals.audio = { concealedSamples: 1_920, concealmentEvents: 3 };
    await vi.advanceTimersByTimeAsync(4_000);
    first.t.stop();
    expect(posted("audio_concealed_samples").slice(0, 2)).toEqual([480, 1_920]);
    expect(posted("audio_concealment_events").slice(0, 2)).toEqual([1, 3]);

    // The replacement peer connection's counters start again at 0; the page view's totals go on.
    posts = [];
    const next = { now: quiet.now, audio: { concealedSamples: 96, concealmentEvents: 1 } };
    const replacement = telemetry(next, counters);
    await vi.advanceTimersByTimeAsync(5_000);
    replacement.t.stop();
    expect(posted("audio_concealed_samples")[0]).toBe(2_016);
    expect(posted("audio_concealment_events")[0]).toBe(4);
  });
});
