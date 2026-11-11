import { describe, expect, it } from "vitest";
import { MIN_CLOCK_SAMPLES } from "../webrtc/clockOffset";
import { summarizePresentWindow } from "../webrtc/presentCadence";
import { isFreeze, WtTelemetry } from "./telemetry";

/** A telemetry on fake clocks: `mono` is performance.now(), `epoch` is Date.now(). */
function harness(profileFps = 60) {
  const clock = { mono: 0, epoch: 1_000_000 };
  const posts: { url: string; body: Record<string, unknown> }[] = [];
  let hidden = false;
  /** Callbacks waiting for the next display frame (requestAnimationFrame's queue). */
  const rafQueue: ((displayMs: number) => void)[] = [];
  let rafCalls = 0;
  const t = new WtTelemetry({
    sessionId: "s1",
    authToken: "tok",
    profileFps,
    deviceKey: "dev",
    nowMs: () => clock.mono,
    nowEpochMs: () => clock.epoch,
    isHidden: () => hidden,
    requestFrame: (cb) => {
      rafCalls++;
      rafQueue.push(cb);
    },
    fetch: (async (url: string, init: RequestInit) => {
      posts.push({ url, body: JSON.parse(init.body as string) });
      return new Response("{}");
    }) as unknown as typeof fetch,
  });
  const advance = (ms: number) => {
    clock.mono += ms;
    clock.epoch += ms;
  };
  /** A display frame starts at `displayMs`: what a browser does for requestAnimationFrame callbacks. */
  const vsync = (displayMs: number) => rafQueue.splice(0).forEach((cb) => cb(displayMs));
  /** Presents `n` frames `gapMs` apart, each captured `ageMs` earlier on the AGENT's clock, each in a display frame of its own. */
  const frames = (n: number, gapMs: number, ageMs = 0, agentAheadMs = 0) => {
    for (let i = 0; i < n; i++) {
      advance(gapMs);
      t.presented((clock.epoch + agentAheadMs - ageMs) * 1000);
      vsync(clock.mono);
    }
  };
  /** Enough pongs for a stable estimate, from an agent whose clock is `aheadMs` ahead. */
  const sync = (aheadMs: number, rttMs = 4) => {
    for (let i = 0; i < MIN_CLOCK_SAMPLES; i++) t.pong(rttMs, clock.epoch + aheadMs - rttMs / 2);
  };
  return { t, clock, posts, advance, frames, vsync, sync, rafCalls: () => rafCalls, hide: () => (hidden = true), show: () => (hidden = false) };
}

describe("WtTelemetry", () => {
  it("reports fps, bitrate and the present cadence of one window", () => {
    const h = harness();
    h.t.presented(0);
    h.vsync(h.clock.mono);
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
        "bitrate_kbps", "fps", "freeze_count", "is_hidden", "packets_lost", "playout_target_ms", "present_beat_fraction", "present_fps",
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

  it("posts the display cushion as playout_target_ms and the arrival jitter behind it", async () => {
    const h = harness();
    h.frames(10, 8);
    h.t.tick();
    h.t.setPlayout(12.5, 3.25);
    h.frames(10, 8);
    expect(h.t.tick().playoutTargetMs).toBe(12.5);
    await h.t.flush();
    const [first, second] = (h.posts[0].body.samples as { metrics: Record<string, number> }[]).map((s) => s.metrics);
    expect(first).toMatchObject({ playout_target_ms: 0 });
    expect(first).not.toHaveProperty("wt_arrival_jitter_ms");
    expect(second).toMatchObject({ playout_target_ms: 12.5, wt_arrival_jitter_ms: 3.25 });
  });

  it("posts audio dropouts cumulative once audio has been measured, and none before", async () => {
    const h = harness();
    h.frames(10, 16);
    h.t.tick();
    h.t.addAudioDropouts(0, 0); // audio is up and has dropped nothing
    h.t.tick();
    h.t.addAudioDropouts(960, 2);
    h.t.tick();
    h.t.addAudioDropouts(480, 1); // a reconnect's new player adds, it does not replace
    expect(h.t.tick()).toMatchObject({ audioConcealedSamples: 1_440, audioConcealmentEvents: 3 });
    await h.t.flush();
    const metrics = (h.posts[0].body.samples as { metrics: Record<string, number> }[]).map((s) => s.metrics);
    expect(metrics[0]).not.toHaveProperty("audio_concealed_samples");
    expect(metrics.map((m) => m.audio_concealed_samples)).toEqual([undefined, 0, 960, 1_440]);
    expect(metrics.map((m) => m.audio_concealment_events)).toEqual([undefined, 0, 2, 3]);
  });

  it("counts long frames and present rate by the one definition WebRTC uses", () => {
    const h = harness();
    h.t.presented(0);
    h.vsync(h.clock.mono);
    const gaps = [8, 8, 8, 8, 2, 2, 8, 8, 30, 8, 8, 8, 2, 2, 8, 30, 8, 8, 8, 8];
    for (const g of gaps) h.frames(1, g);
    const snap = h.t.tick();
    const reference = summarizePresentWindow(gaps, null, 60);
    expect(snap.presentCadence).toEqual(reference);
    expect(reference.longFrames).toBe(2);
  });

  describe("present cadence is stamped at the display frame (#1328)", () => {
    const VSYNC = 1000 / 120;
    /** `vsyncs` display frames at 120 Hz, each with `perFrame` pictures submitted inside it (a decoder burst). */
    const stream = (h: ReturnType<typeof harness>, vsyncs: number, perFrame: number, from = 0) => {
      for (let i = 0; i < vsyncs; i++) {
        const at = (from + i) * VSYNC;
        for (let k = 0; k < perFrame; k++) {
          h.clock.mono = at - VSYNC + 1 + k;
          h.t.presented(0);
        }
        h.vsync(at);
      }
    };

    it("counts several frames submitted inside one display frame as one displayed frame, with no interval under a display frame", () => {
      const h = harness(120);
      stream(h, 120, 3);
      const snap = h.t.tick();
      expect(snap.presentCadence.n).toBe(119);
      expect(snap.presentCadence.medianMs).toBeCloseTo(VSYNC, 6);
      expect(snap.presentCadence.fpsFromMedian).toBeCloseTo(120, 6);
      expect(snap.presentCadence.longFrames).toBe(0);
      expect(snap.freezeCount).toBe(0);
      // Every submitted picture is still a frame for the fps and the decoded total.
      expect(snap.framesDecodedTotal).toBe(360);
    });

    it("reads the display rate and no long frames for a steady 120 fps stream on a 120 Hz display, however the submits fall in the frame", () => {
      const h = harness(120);
      for (let i = 0; i < 240; i++) {
        h.clock.mono = i * VSYNC - VSYNC + (i % 2 ? 7.5 : 0.5); // late, then early, in each display frame
        h.t.presented(0);
        h.vsync(i * VSYNC);
      }
      const c = h.t.tick().presentCadence;
      expect(c.fpsFromMedian).toBeCloseTo(120, 6);
      expect(c.sdMs).toBeCloseTo(0, 6);
      expect(c.p95Ms).toBeCloseTo(VSYNC, 6);
      expect(c.longFrames).toBe(0);
    });

    it("still reads a real gap as one long frame", () => {
      const h = harness(120);
      stream(h, 60, 3);
      stream(h, 60, 3, 60 + 5); // five display frames with no picture
      const c = h.t.tick().presentCadence;
      expect(c.longFrames).toBe(1);
      expect(c.maxMs).toBeCloseTo(6 * VSYNC, 6);
    });

    it("reports no cadence for a hidden tab and does not grow a queue", () => {
      const h = harness(120);
      stream(h, 30, 1);
      h.t.tick();
      h.hide(); // no display frames: the callbacks never run
      h.clock.mono = 5000;
      for (let i = 0; i < 500; i++) h.t.presented(0);
      expect(h.rafCalls()).toBe(31); // 30 for the stream, one waiting for the hidden tab
      const snap = h.t.tick();
      expect(snap.presentCadence.n).toBe(0);
      expect(snap.freezeCount).toBe(0);
    });

    it("does not report a hidden interval as a long frame when a throttled callback runs, or when the tab is shown again", () => {
      const h = harness(120);
      stream(h, 30, 1);
      h.hide();
      h.t.presented(0);
      h.vsync(10_000); // a background tab's one-a-second callback
      h.show();
      h.t.breakCadence(); // the runtime does this when the tab is visible again
      stream(h, 30, 1, 10_000 / VSYNC);
      const snap = h.t.tick();
      expect(snap.presentCadence.n).toBe(58);
      expect(snap.presentCadence.longFrames).toBe(0);
      expect(snap.freezeCount).toBe(0);
    });
  });

  it("opens its first window when the ticks start, not when it was made", () => {
    const h = harness();
    h.advance(5000); // the presenter, the token and the connect
    h.t.startWindow();
    h.frames(60, 1000 / 60);
    h.t.bytes(125_000);
    const snap = h.t.tick();
    expect(snap.fps).toBeCloseTo(60, 0);
    expect(snap.bitrateKbps).toBeCloseTo(1000, -1);
  });

  it("counts the stall of a window too thin for a median, against the last window's cadence", () => {
    const h = harness();
    h.frames(60, 1000 / 60);
    expect(h.t.tick().freezeCount).toBe(0);
    h.frames(1, 2900); // a 2.9 s stall, then two frames before the tick
    h.frames(2, 16);
    const snap = h.t.tick();
    expect(snap.presentCadence.n).toBe(3);
    expect(snap.freezeCount).toBe(1);
  });

  it("judges a first thin window against the profile's frame time", () => {
    const h = harness(60);
    h.frames(1, 16);
    h.frames(1, 1500);
    expect(h.t.tick().freezeCount).toBe(1);
  });

  it("reports fragments found missing as cumulative packet loss", async () => {
    const h = harness();
    h.t.addLost(3);
    expect(h.t.tick().packetsLost).toBe(3);
    h.t.addLost(2);
    expect(h.t.tick().packetsLost).toBe(2);
    h.t.addLost(1); // a new connection's first loss
    h.t.addLost(0);
    expect(h.t.tick().packetsLost).toBe(1);
    await h.t.flush();
    const samples = h.posts[0].body.samples as { metrics: Record<string, number> }[];
    expect(samples.map((s) => s.metrics.packets_lost)).toEqual([3, 5, 6]);
  });

  it("does not count the gap across a hidden tab or a reconnect as a freeze", () => {
    const h = harness();
    h.frames(30, 16);
    h.advance(4000); // hidden, or reconnecting
    h.t.breakCadence();
    h.frames(30, 16);
    const snap = h.t.tick();
    expect(snap.freezeCount).toBe(0);
    expect(snap.presentCadence.n).toBe(58);
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
