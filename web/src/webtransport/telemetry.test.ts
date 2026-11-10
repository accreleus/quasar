import { describe, expect, it } from "vitest";
import { MIN_CLOCK_SAMPLES } from "../webrtc/clockOffset";
import { isFreeze, WtTelemetry } from "./telemetry";

/** A telemetry on fake clocks: `mono` is performance.now(), `epoch` is Date.now(). */
function harness(profileFps = 60) {
  const clock = { mono: 0, epoch: 1_000_000 };
  const posts: { url: string; body: Record<string, unknown> }[] = [];
  let hidden = false;
  const t = new WtTelemetry({
    sessionId: "s1",
    authToken: "tok",
    profileFps,
    deviceKey: "dev",
    nowMs: () => clock.mono,
    nowEpochMs: () => clock.epoch,
    isHidden: () => hidden,
    fetch: (async (url: string, init: RequestInit) => {
      posts.push({ url, body: JSON.parse(init.body as string) });
      return new Response("{}");
    }) as unknown as typeof fetch,
  });
  const advance = (ms: number) => {
    clock.mono += ms;
    clock.epoch += ms;
  };
  /** Presents `n` frames `gapMs` apart, each captured `ageMs` earlier on the AGENT's clock. */
  const frames = (n: number, gapMs: number, ageMs = 0, agentAheadMs = 0) => {
    for (let i = 0; i < n; i++) {
      advance(gapMs);
      t.presented((clock.epoch + agentAheadMs - ageMs) * 1000);
    }
  };
  /** Enough pongs for a stable estimate, from an agent whose clock is `aheadMs` ahead. */
  const sync = (aheadMs: number, rttMs = 4) => {
    for (let i = 0; i < MIN_CLOCK_SAMPLES; i++) t.pong(rttMs, clock.epoch + aheadMs - rttMs / 2);
  };
  return { t, clock, posts, advance, frames, sync, hide: () => (hidden = true) };
}

describe("WtTelemetry", () => {
  it("reports fps, bitrate and the present cadence of one window", () => {
    const h = harness();
    h.t.presented(0);
    h.frames(60, 1000 / 60);
    h.t.bytes(125_000);
    const snap = h.t.tick();
    expect(snap.fps).toBeCloseTo(61, 0);
    expect(snap.bitrateKbps).toBeCloseTo(1000, -1);
    expect(snap.presentCadence.n).toBe(60);
    expect(snap.presentCadence.fpsFromMedian).toBeCloseTo(60, 3);
    expect(snap.presentSdMs).toBeCloseTo(0, 6);
    expect(snap.clientHealth).toBe("smooth");
    expect(snap.framesDecodedTotal).toBe(61);
    expect(snap.bytesReceivedTotal).toBe(125_000);
    // The next window starts empty.
    h.advance(1000);
    expect(h.t.tick()).toMatchObject({ fps: 0, bitrateKbps: 0 });
  });

  it("measures decode time from the unit going in to its frame coming out", () => {
    const h = harness();
    h.t.decodeIn(111);
    h.advance(3);
    h.t.decodeIn(222);
    h.advance(2);
    h.t.decodeOut(111); // 5 ms
    h.advance(2);
    h.t.decodeOut(222); // 4 ms
    h.t.decodeOut(333); // never went in: ignored
    expect(h.t.tick().decodeMs).toBeCloseTo(4.5, 6);
  });

  it("has no capture-to-present number until the clock offset is known, then measures on the agent's clock", () => {
    const h = harness();
    h.frames(30, 16, 20, 7_000);
    expect(h.t.clockOffsetMs()).toBeNull();
    expect(h.t.tick().g2gMs).toBeNull();
    // The agent's clock is 7 s ahead of this machine's: uncorrected, every sample would be dropped or absurd.
    h.sync(7_000);
    expect(h.t.clockOffsetMs()).toBeCloseTo(7_000, 6);
    h.frames(30, 16, 20, 7_000);
    const snap = h.t.tick();
    expect(snap.g2gMs).toBeCloseTo(20, 6);
    expect(snap.rttMs).toBe(4);
  });

  it("takes the offset from the pong with the shortest round trip", () => {
    const h = harness();
    h.sync(500, 40);
    h.t.pong(2, h.clock.epoch + 500 - 1); // the clean sample
    h.t.pong(90, h.clock.epoch + 530); // a delayed one, 75 ms off
    expect(h.t.clockOffsetMs()).toBeCloseTo(500, 6);
  });

  it("counts a freeze by the WebRTC definition", () => {
    expect(isFreeze(49, 16.7)).toBe(false);
    expect(isFreeze(167, 16.7)).toBe(true);
    const h = harness();
    h.frames(30, 16);
    h.frames(1, 400);
    h.frames(30, 16);
    expect(h.t.tick().freezeCount).toBe(1);
  });

  it("posts the manifest's keys, and a hidden tab's class instead of a verdict on its cadence", async () => {
    const h = harness();
    h.t.setCodec("video/H264");
    h.sync(0);
    h.frames(60, 1000 / 60, 12);
    h.t.keyframeRequested();
    h.t.tick();
    h.hide();
    h.advance(1000);
    h.t.tick();
    await h.t.flush();
    const stats = h.posts.find((p) => p.url === "/v1/sessions/s1/stats")!;
    const [first, second] = stats.body.samples as { metrics: Record<string, number>; client_health?: string; codec_mime_type: string; device_key: string }[];
    expect(Object.keys(first.metrics).sort()).toEqual(
      [
        "bitrate_kbps", "fps", "freeze_count", "is_hidden", "playout_target_ms", "present_beat_fraction", "present_fps",
        "present_fps_median", "present_interval_max_ms", "present_interval_median_ms", "present_interval_p95_ms",
        "present_interval_sd_ms", "present_long_frames", "present_n", "rtt_ms", "wt_capture_to_present_ms", "wt_keyframe_requests",
      ].sort(),
    );
    expect(first.metrics.wt_capture_to_present_ms).toBeCloseTo(12, 6);
    expect(first.metrics.wt_keyframe_requests).toBe(1);
    expect(first).toMatchObject({ client_health: "smooth", codec_mime_type: "video/H264", device_key: "dev" });
    expect(second.metrics).toMatchObject({ is_hidden: 1, present_n: 0, wt_keyframe_requests: 0 });
    expect(second.client_health).toBe("backgrounded_or_hidden");
    // Posted once: a second flush has nothing to send.
    await h.t.flush();
    expect(h.posts.filter((p) => p.url.endsWith("/stats"))).toHaveLength(1);
  });

  it("does not call a window with too few frames smooth", async () => {
    const h = harness();
    h.frames(2, 16);
    h.t.tick();
    await h.t.flush();
    expect("client_health" in (h.posts[0].body.samples as object[])[0]).toBe(false);
  });

  it("posts the clock row once it is stable, and not again while it holds", async () => {
    const h = harness();
    await h.t.flush();
    expect(h.posts).toEqual([]);
    h.sync(250, 6);
    await h.t.flush();
    expect(h.posts).toEqual([{ url: "/v1/sessions/s1/trace/clock", body: { client_offset_ms: 250, uncertainty_ms: 3 } }]);
    h.advance(5000);
    await h.t.flush();
    expect(h.posts).toHaveLength(1);
  });
});
