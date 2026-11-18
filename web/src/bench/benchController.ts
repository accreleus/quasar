// Bench mode — in-page marker decode on every displayed frame: glass-to-glass
// from the frame's own pixels (not the RVFC captureTime Chrome never surfaces),
// missing/dup/reordered indices, and input-to-photon via the echo flag.
//
// In the SPA because the CSP blocks injecting the decoder from a harness
// (2026-08-18-benchapp-bringup.md §6) and the measurements need every frame.
// Cost control: cached-location fast path + bbox-shrunk readback once located;
// while unlocated, the video path reads and searches the pixels at most four times
// a second (the readback itself costs presentations, not just the search); the
// PyroWave texture path copies the crop every frame, off the presentation path,
// and searches on the same four-a-second cadence.
//
// Nothing here runs unless bench mode is explicitly on (benchFlag.ts).

import { decodeMarker } from "./marker_decode.js";
import type { MarkerDecodeResult, MarkerImageData, MarkerLocation } from "./marker_decode.js";
import { EVDEV } from "../input/evdev";
import {
  FLAG_INPUT_ECHO,
  classifyIndexDelta,
  summarizeWindow,
  type BenchFrame,
  type BenchWindowPayload,
  type IndexDeltaCounts,
} from "./aggregate";
import {
  diffInbound,
  parseInboundRtp,
  summarizeStages,
  type InboundCounters,
  type InboundDelta,
} from "./stages";

/** Per-frame ring-buffer capacity — `window.__qBench.dump()` returns this many. */
export const RING_CAPACITY = 5000;
/** Window cadence: one `bench.window` trace event per second. */
export const WINDOW_MS = 1000;
/**
 * Initial search crop, as a fraction of the video's intrinsic size. The marker is
 * top-left anchored and, at the image's pinned `BENCHAPP_MARKER_SCALE=0.5`, is
 * 480×304 px on a 1080p frame — a "15% of the frame" crop would cut it in half,
 * so the search window is deliberately generous and then shrinks once located.
 */
export const SEARCH_FRACTION = 0.45;
/** Floor for the search crop so a small received frame still contains the marker. */
export const SEARCH_MIN_PX = 512;
/** Minimum gap between full (unhinted) marker searches — see the cost note above. */
export const FULL_SEARCH_MIN_INTERVAL_MS = 250;
/**
 * Hinted-decode failures before re-acquiring from scratch. Without this an
 * external resolution increase (720p→1080p rung) is unrecoverable: stale cell
 * geometry plus a crop too small for the larger marker, for the whole session.
 */
export const REACQUIRE_AFTER_FAILURES = 15;
/**
 * Echo wait before abandoning a measurement. An echo can be missed for
 * non-latency reasons; without a timeout the queue wedges and input-to-photon
 * is silently empty while the harness still reports green.
 */
export const I2P_TIMEOUT_MS = 2000;
/** onTexture copies in flight before a frame goes unread (no_image) rather than queueing more. */
export const READBACKS_IN_FLIGHT = 3;
/** How many windows to keep for `window.__qBench.windows()`. */
const MAX_WINDOWS = 3600;
/**
 * How long tick() waits for in-flight texture readbacks before closing the
 * window, so a device whose maps never settle cannot stop the windows. Frames
 * are stamped at submit, so measuring them here (before closeWindow) keeps them
 * in the window their submit falls in.
 */
export const READBACK_SETTLE_MS = 250;

const NOT_READ: MarkerDecodeResult = { decoded: false, confidence: 0, error: "not read" };

/** Everything the controller needs from the session, injected for testability. */
export interface BenchDeps {
  /** The <video> of a WebRTC session: its RVFC drives the frame loop and its pixels are read. */
  video?: HTMLVideoElement;
  /**
   * The frame to read when there is no <video> (a WebTransport session's decoded frame, read
   * before the presenter takes it). The session then calls `onFrame()` itself, once per frame.
   */
  source?: () => { image: CanvasImageSource; width: number; height: number } | null;
  /** Sends one input message over the session's input DataChannel. */
  sendInput: (msg: Record<string, unknown>) => void;
  /** Posts one completed window as a `bench.window` trace event. */
  emitWindow: (payload: BenchWindowPayload) => void;
  /**
   * ST-05 client↔host clock offset (ms) to ADD to a browser epoch stamp to put
   * it on the host clock. Null until the ping/pong estimate is stable — and the
   * honest answer stays null rather than 0 (no false precision).
   */
  getClockOffsetMs: () => number | null;
  /** Injectable clocks (tests). */
  nowEpochMs?: () => number;
  /**
   * Pixel-readback override. Production leaves this unset and the controller
   * reads the <video> through a canvas; tests supply frames directly, since
   * jsdom has no 2D context.
   */
  grabFrame?: () => MarkerImageData | null;
  /** Maps a KeyboardEvent.code to its evdev code (defaults to the SPA's table). */
  evdev?: Record<string, number>;
  /**
   * Used only for the `inbound-rtp` counters (stages.ts); absent ⇒ those
   * stage_* keys are null. A second, bench-only poller — never a hook into
   * SessionTelemetry's loop, whose schema-allow-listed stats series bench mode
   * must not extend (docs/testing-bench-mode.md).
   */
  getStats?: () => Promise<RTCStatsReport>;
  /** `performance.timeOrigin` override for tests; production reads the real one. */
  timeOriginMs?: number;
}

/** The `window.__qBench` surface the harness drives. */
export interface BenchHandle {
  enabled: true;
  pressKey: (code: string) => boolean;
  dump: () => { frames: BenchFrame[]; windows: BenchWindowPayload[] };
  windows: () => BenchWindowPayload[];
  stats: () => {
    frames: number;
    decoded: number;
    located: boolean;
    i2p_missed: number;
    no_image: number;
    skipped_reads: number;
    missed_presentations: number;
    callback_ms_mean: number | null;
    callback_ms_max: number;
  };
}

/** One key send awaiting its echo in the pixels. */
interface PendingInput {
  sentMs: number;
}

/** Turn an RVFC metadata record into a browser epoch-ms present time. */
export function presentEpochMs(
  metadata: VideoFrameCallbackMetadata | undefined,
  nowEpochMs: () => number,
): number {
  const expected = metadata?.expectedDisplayTime;
  if (
    expected != null &&
    Number.isFinite(expected) &&
    typeof performance !== "undefined" &&
    Number.isFinite(performance.timeOrigin)
  ) {
    return performance.timeOrigin + expected;
  }
  return nowEpochMs();
}

/**
 * Lift stage-relevant RVFC metadata onto a BenchFrame. Non-finite fields are
 * omitted ("the UA did not tell us" ≠ zero). Unit hazard: the spec gives
 * `processingDuration` in seconds; converted here once to `processing_ms`.
 */
export function rvfcStageMetadata(
  metadata: VideoFrameCallbackMetadata | undefined,
): Partial<BenchFrame> {
  if (!metadata) return {};
  const out: Partial<BenchFrame> = {};
  const num = (v: unknown): number | null =>
    typeof v === "number" && Number.isFinite(v) ? v : null;
  const m = metadata as unknown as Record<string, unknown>;

  const receive = num(m["receiveTime"]);
  if (receive !== null) out.receive_ms = receive;
  const presentation = num(m["presentationTime"]);
  if (presentation !== null) out.presentation_ms = presentation;
  const display = num(m["expectedDisplayTime"]);
  if (display !== null) out.expected_display_ms = display;
  const processing = num(m["processingDuration"]);
  if (processing !== null) out.processing_ms = processing * 1000;
  const rtp = num(m["rtpTimestamp"]);
  if (rtp !== null) out.rtp_timestamp = rtp;
  const presented = num(m["presentedFrames"]);
  if (presented !== null) out.presented_frames = presented;
  return out;
}

export class BenchController {
  private readonly deps: Required<Pick<BenchDeps, "nowEpochMs">> & BenchDeps;
  private readonly canvas: HTMLCanvasElement;
  private ctx: CanvasRenderingContext2D | null = null;

  private running = false;
  private windowTimer: ReturnType<typeof setInterval> | null = null;

  /** Cached marker location once found — the fast path (crop-local coords). */
  private location: MarkerLocation | null = null;
  /** Search crop size in video pixels; shrinks to the marker bbox once located. */
  private cropW = 0;
  private cropH = 0;
  /** The video size the current crop/location geometry was derived from. */
  private geomW = 0;
  private geomH = 0;
  /** Consecutive frames that produced an image but no decode. */
  private failStreak = 0;
  /** Last full (unhinted) search attempt, epoch ms — throttles the expensive path. */
  private lastFullSearchMs = 0;

  private readonly ring: BenchFrame[] = [];
  private readonly windowsOut: BenchWindowPayload[] = [];

  private windowFrames: BenchFrame[] = [];
  private windowDeltas: IndexDeltaCounts = { missing: 0, duplicated: 0, reordered: 0 };
  private windowI2p: number[] = [];
  private windowNoImage = 0;
  private windowI2pMissed = 0;
  private windowStartMs = 0;

  private prevIndex: number | null = null;
  private prevPulse = false;
  /** Sends awaiting an echo, oldest first — a queue so a pulse cadence shorter
   *  than the round trip is queued, not refused; FIFO matches the input path. */
  private readonly pendingInputs: PendingInput[] = [];

  /** onTexture's readbacks: measured in submission order; buffers reused. */
  private readTail: Promise<void> = Promise.resolve();
  private readsInFlight = 0;
  private readonly spareReads: GPUBuffer[] = [];
  private readFaulted = false;
  private limitWarned = false;

  private totalDecoded = 0;
  private totalI2pMissed = 0;
  private totalNoImage = 0;
  /** Frames whose pixels were not read because a full search was not due. */
  private totalSkippedReads = 0;
  /** Presentations RVFC coalesced away before the callback ran (presentedFrames gaps). */
  private totalMissedPresentations = 0;
  private prevPresented: number | null = null;
  private totalCallbacks = 0;
  private totalCallbackMs = 0;
  private maxCallbackMs = 0;

  /** Time origin used to lift RVFC high-res stamps into the epoch (stages.ts). */
  private readonly timeOriginMs: number;
  /** Previous poll's `inbound-rtp` counters; the diff basis for the next window. */
  private prevInbound: InboundCounters | null = null;
  /** The delta covering the window about to close; null before the second poll. */
  private inboundDelta: InboundDelta | null = null;

  constructor(deps: BenchDeps) {
    this.deps = { nowEpochMs: () => Date.now(), ...deps };
    this.canvas = document.createElement("canvas");
    this.timeOriginMs =
      deps.timeOriginMs ??
      (typeof performance !== "undefined" && Number.isFinite(performance.timeOrigin)
        ? performance.timeOrigin
        : 0);
  }

  start(): void {
    if (this.running) return;
    this.running = true;
    this.windowStartMs = this.deps.nowEpochMs();
    this.windowTimer = setInterval(() => void this.tick(), WINDOW_MS);
    this.schedule();
  }

  /**
   * Refresh the inbound-rtp counters, wait for in-flight texture readbacks,
   * then close the window — in that order.
   *
   * refreshInbound first, or the counter delta describes a window up to a
   * second away from the frames it is quoted beside. The readback chain first,
   * or a texture frame stamped at its submit could land in the next window
   * (#1454): onTexture measures when its map resolves, so a slow map would put
   * the frame in whatever window is open then. Frames are stamped at submit, so
   * measuring them here keeps them in the window their submit falls in. The
   * wait is bounded (#1454): a device whose maps never settle must not stop the
   * windows.
   */
  private async tick(): Promise<void> {
    await this.refreshInbound();
    await Promise.race([this.readTail, this.settleDelay(READBACK_SETTLE_MS)]);
    this.closeWindow();
  }

  /** Resolves after `ms`; bounds how long tick() waits for the readback chain. */
  private settleDelay(ms: number): Promise<void> {
    return new Promise<void>((resolve) => {
      setTimeout(resolve, ms);
    });
  }

  /** Test seam: run one window boundary exactly as the interval timer would. */
  async tickForTest(): Promise<void> {
    await this.tick();
  }

  private async refreshInbound(): Promise<void> {
    const getStats = this.deps.getStats;
    if (!getStats) return;
    try {
      const cur = parseInboundRtp(await getStats());
      if (!cur) return;
      this.inboundDelta = diffInbound(this.prevInbound, cur);
      this.prevInbound = cur;
    } catch {
      // getStats can reject on a closing peer connection. A failed poll must
      // leave the marker loop untouched; the window simply reports null means.
      this.inboundDelta = null;
    }
  }

  stop(): void {
    if (!this.running) return;
    this.running = false;
    if (this.windowTimer !== null) {
      clearInterval(this.windowTimer);
      this.windowTimer = null;
    }
    // Flush the last partial window so its frames are not lost; readbacks still in flight are not measured.
    this.closeWindow();
    for (const b of this.spareReads.splice(0)) b.destroy();
  }

  /** The `window.__qBench` object. */
  handle(): BenchHandle {
    return {
      enabled: true,
      pressKey: (code: string) => this.pressKey(code),
      dump: () => ({ frames: [...this.ring], windows: [...this.windowsOut] }),
      windows: () => [...this.windowsOut],
      stats: () => ({
        frames: this.ring.length,
        decoded: this.totalDecoded,
        located: this.location !== null,
        i2p_missed: this.totalI2pMissed,
        no_image: this.totalNoImage,
        skipped_reads: this.totalSkippedReads,
        missed_presentations: this.totalMissedPresentations,
        callback_ms_mean: this.totalCallbacks > 0 ? this.totalCallbackMs / this.totalCallbacks : null,
        callback_ms_max: this.maxCallbackMs,
      }),
    };
  }

  /**
   * Send one key down+up over the input DataChannel and arm an input-to-photon
   * measurement. Returns false only when the key is unknown or the send threw —
   * overlapping pulses are queued, not refused.
   */
  pressKey(code: string): boolean {
    const table = this.deps.evdev ?? EVDEV;
    const evdev = table[code];
    if (evdev === undefined) return false;
    const sentAt = this.deps.nowEpochMs();
    try {
      this.deps.sendInput({ t: "k", code: evdev, pressed: true });
      this.deps.sendInput({ t: "k", code: evdev, pressed: false });
    } catch {
      return false;
    }
    this.pendingInputs.push({ sentMs: sentAt });
    return true;
  }

  // ── RVFC loop ───────────────────────────────────────────────────────────────

  private schedule(): void {
    if (!this.running) return;
    const video = this.deps.video;
    if (typeof video?.requestVideoFrameCallback !== "function") return;
    video.requestVideoFrameCallback((_now, metadata) => {
      this.onFrame(metadata);
      this.schedule();
    });
  }

  /** Exposed for tests: process one displayed frame. */
  onFrame(metadata?: VideoFrameCallbackMetadata): void {
    if (!this.running) return;
    const started = performance.now();
    const stageMeta = rvfcStageMetadata(metadata);
    const presented = stageMeta.presented_frames;
    if (presented !== undefined && this.prevPresented !== null && presented - this.prevPresented > 1) {
      this.totalMissedPresentations += presented - this.prevPresented - 1;
    }
    if (presented !== undefined) this.prevPresented = presented;
    // Between full searches nothing reads pixels: the readback, not the decode, is what costs presentations.
    const searchDue = this.location !== null || this.deps.nowEpochMs() - this.lastFullSearchMs >= FULL_SEARCH_MIN_INTERVAL_MS;
    this.measure(presentEpochMs(metadata, this.deps.nowEpochMs), stageMeta, searchDue ? this.grab() : undefined);
    const took = performance.now() - started;
    this.totalCallbackMs += took;
    this.maxCallbackMs = Math.max(this.maxCallbackMs, took);
    this.totalCallbacks += 1;
  }

  /**
   * A WebTransport PyroWave frame, just submitted to the presenter: its marker crop is copied off
   * the decoded texture and measured when the copy is mapped, in submission order. Presentation
   * never waits for it. present_ms is the submit, as on the WebCodecs path (#1035).
   */
  onTexture(device: GPUDevice, texture: GPUTexture): void {
    if (!this.running) return;
    const present = this.deps.nowEpochMs();
    const pixels = this.readBack(device, texture);
    this.readTail = this.readTail
      .then(async () => {
        const image = await pixels;
        if (this.running) this.measure(present, {}, image);
      })
      .catch((err: unknown) => console.warn("bench:", err));
  }

  private async readBack(device: GPUDevice, texture: GPUTexture): Promise<MarkerImageData | null> {
    if (this.readsInFlight >= READBACKS_IN_FLIGHT) {
      // The frame had pixels; the readbacks fell behind. Still no_image, but said once.
      if (!this.limitWarned) console.warn(`bench: readbacks fell behind (${READBACKS_IN_FLIGHT} in flight), frames count as no_image`);
      this.limitWarned = true;
      return null;
    }
    this.region(texture.width, texture.height);
    const w = this.cropW;
    const h = this.cropH;
    // copyTextureToBuffer rows are 256-byte aligned.
    const stride = Math.ceil((w * 4) / 256) * 256;
    let buf: GPUBuffer | undefined;
    this.readsInFlight++;
    try {
      // The device is the presenter's: an uncaptured error there ends the session, so the readback's errors stay in its own scopes.
      device.pushErrorScope("out-of-memory");
      device.pushErrorScope("validation");
      let scopes: Promise<GPUError | null>[];
      let mapped: Promise<void>;
      try {
        const i = this.spareReads.findIndex((b) => b.size >= stride * h);
        buf = i >= 0 ? this.spareReads.splice(i, 1)[0]! : device.createBuffer({ size: stride * h, usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST });
        const enc = device.createCommandEncoder();
        enc.copyTextureToBuffer({ texture }, { buffer: buf, bytesPerRow: stride }, [w, h]);
        device.queue.submit([enc.finish()]);
        mapped = buf.mapAsync(GPUMapMode.READ, 0, stride * h);
      } finally {
        // Popped even on a throw, or the scopes would swallow the presenter's own errors.
        scopes = [device.popErrorScope(), device.popErrorScope()].map((p) => p.catch(() => null));
      }
      const [[invalid, oom]] = await Promise.all([Promise.all(scopes), mapped]);
      if (invalid ?? oom) throw invalid ?? oom;
      const src = new Uint8Array(buf.getMappedRange(0, stride * h));
      const data = new Uint8ClampedArray(w * h * 4);
      for (let y = 0; y < h; y++) data.set(src.subarray(y * stride, y * stride + w * 4), y * w * 4);
      buf.unmap();
      if (this.running && this.spareReads.length < READBACKS_IN_FLIGHT) this.spareReads.push(buf);
      else buf.destroy();
      return { width: w, height: h, data };
    } catch (err) {
      // No readback on this device: the frames count as no_image, never as silence.
      if (!this.readFaulted) console.warn("bench: texture readback", err);
      this.readFaulted = true;
      buf?.destroy();
      return null;
    } finally {
      this.readsInFlight--;
    }
  }

  /** `image` is null when the readback yielded nothing, undefined when this frame's pixels were not read (no search due). */
  private measure(present: number, stageMeta: Partial<BenchFrame>, image: MarkerImageData | null | undefined): void {
    const offset = this.deps.getClockOffsetMs();
    if (image === null) {
      // No readable pixels (pre-decode, tainted canvas, teardown) — counted
      // separately; must not inflate `n` or the undecoded count.
      this.windowNoImage += 1;
      this.totalNoImage += 1;
      this.expireStaleInputs(present);
      return;
    }

    let result: MarkerDecodeResult = NOT_READ;
    if (image) result = decodeMarker(image, this.decodeOptions());
    else this.totalSkippedReads += 1;
    let frame: BenchFrame = {
      present_ms: present, decoded: false, confidence: 0, offset_ms: offset, ...stageMeta,
    };
    if (result.decoded) {
      this.failStreak = 0;
      this.lastFullSearchMs = 0;
      this.location = { x: result.marker_x, y: result.marker_y, cellSize: result.cell_size };
      this.tightenCrop(result.marker_x, result.marker_y, result.cell_size);
      // Offset direction (clockOffset.ts): host_epoch ≈ client_epoch + offset.
      // So the present instant on the host clock is present + offset, and the
      // frame's own submit stamp is host_time_ms. Both are then host-clock.
      const g2g = present + (offset ?? 0) - result.host_time_ms;
      frame = {
        present_ms: present,
        decoded: true,
        confidence: result.confidence,
        frame_index: result.frame_index,
        host_time_ms: result.host_time_ms,
        scene_id: result.scene_id,
        load_level: result.load_level,
        event_flags: result.event_flags,
        render_w: result.render_w,
        render_h: result.render_h,
        g2g_ms: g2g,
        offset_ms: offset,
        ...stageMeta,
      };
    } else if (image) {
      this.failStreak += 1;
      if (this.location !== null && this.failStreak >= REACQUIRE_AFTER_FAILURES) {
        // Re-acquire from scratch: the geometry we cached no longer describes
        // what is on screen (rung change, app restart, marker moved).
        this.resetGeometry();
      }
    }

    if (frame.decoded && frame.frame_index != null) {
      this.totalDecoded += 1;
      const d = classifyIndexDelta(this.prevIndex, frame.frame_index);
      this.windowDeltas.missing += d.missing;
      this.windowDeltas.duplicated += d.duplicated;
      this.windowDeltas.reordered += d.reordered;
      this.prevIndex = frame.frame_index;

      // Input-to-photon: match on the RISING edge of the echo flag, so a pulse
      // that was already lit when we sent can never be miscounted as the answer.
      const pulse = ((frame.event_flags ?? 0) & FLAG_INPUT_ECHO) !== 0;
      if (pulse && !this.prevPulse) {
        const pending = this.pendingInputs.shift();
        if (pending) {
          // Pure client clock on both ends — carries no clock-offset error.
          this.windowI2p.push(present - pending.sentMs);
        }
      }
      this.prevPulse = pulse;
    }

    this.expireStaleInputs(present);

    this.windowFrames.push(frame);
    this.ring.push(frame);
    if (this.ring.length > RING_CAPACITY) this.ring.splice(0, this.ring.length - RING_CAPACITY);
  }

  /**
   * Decode options for this frame: the cached-location fast path when we have
   * one, otherwise a THROTTLED full search (see the cost note in the header).
   * Returning `searchOnHintFailure: false` keeps a stale hint from silently
   * falling through to the expensive path on every frame.
   */
  private decodeOptions(): { location?: MarkerLocation; searchOnHintFailure?: boolean } | undefined {
    if (this.location) return { location: this.location, searchOnHintFailure: false };
    const now = this.deps.nowEpochMs();
    if (now - this.lastFullSearchMs < FULL_SEARCH_MIN_INTERVAL_MS) {
      // Hand the decoder a location it cannot use, with search disabled: it
      // returns "not decoded" immediately and allocates nothing.
      return { location: { x: -1, y: -1, cellSize: 0 }, searchOnHintFailure: false };
    }
    this.lastFullSearchMs = now;
    return undefined;
  }

  /** Abandon sends whose echo never arrived, so the queue can never wedge. */
  private expireStaleInputs(nowMs: number): void {
    while (this.pendingInputs.length > 0 && nowMs - this.pendingInputs[0]!.sentMs > I2P_TIMEOUT_MS) {
      this.pendingInputs.shift();
      this.windowI2pMissed += 1;
      this.totalI2pMissed += 1;
    }
  }

  /** Test seam: close the current window as the 1 s timer would. */
  closeWindowForTest(): void {
    this.closeWindow();
  }

  private closeWindow(): void {
    const nowMs = this.deps.nowEpochMs();
    // Empty windows are emitted, not skipped: a freeze must read as frameless
    // windows, never as a hole compressing the timeline. Two summarizers merged
    // (counts+g2g vs stage_* split); dependency runs stages.ts → aggregate.ts only.
    const payload: BenchWindowPayload = {
      ...summarizeWindow(this.windowFrames, this.windowDeltas, this.windowI2p, {
        t_start_ms: this.windowStartMs,
        t_end_ms: nowMs,
        offset_ms: this.deps.getClockOffsetMs(),
        no_image: this.windowNoImage,
        i2p_missed: this.windowI2pMissed,
      }),
      ...summarizeStages(this.windowFrames, this.timeOriginMs, this.inboundDelta),
    };
    this.windowFrames = [];
    this.windowDeltas = { missing: 0, duplicated: 0, reordered: 0 };
    this.windowI2p = [];
    this.windowNoImage = 0;
    this.windowI2pMissed = 0;
    this.windowStartMs = nowMs;

    this.windowsOut.push(payload);
    if (this.windowsOut.length > MAX_WINDOWS) this.windowsOut.shift();
    try {
      this.deps.emitWindow(payload);
    } catch {
      // Emission is best-effort; it must never break the sampling loop.
    }
  }

  // ── Pixel readback ──────────────────────────────────────────────────────────

  /** Drop cached geometry and re-expand the crop to a full search window. */
  private resetGeometry(): void {
    this.location = null;
    this.cropW = 0;
    this.cropH = 0;
    this.failStreak = 0;
    this.lastFullSearchMs = 0;
  }

  /**
   * Read the top-left crop of the video at its INTRINSIC size. Never a CSS-scaled
   * screenshot: `videoWidth/videoHeight` is object-fit independent, so the marker's
   * cell geometry is whatever the encoder actually sent (webrtc-testing rule).
   */
  private grab(): MarkerImageData | null {
    if (this.deps.grabFrame) return this.deps.grabFrame();
    const video = this.deps.video;
    const src = this.deps.source ? this.deps.source() : video ? { image: video, width: video.videoWidth, height: video.videoHeight } : null;
    if (!src) return null;
    const { width: vw, height: vh } = src;
    if (!vw || !vh) return null;
    this.region(vw, vh);

    if (this.canvas.width !== this.cropW || this.canvas.height !== this.cropH) {
      this.canvas.width = this.cropW;
      this.canvas.height = this.cropH;
      this.ctx = null;
    }
    if (!this.ctx) {
      this.ctx = this.canvas.getContext("2d", { willReadFrequently: true });
      if (!this.ctx) return null;
    }
    try {
      this.ctx.drawImage(src.image, 0, 0, this.cropW, this.cropH, 0, 0, this.cropW, this.cropH);
      return this.ctx.getImageData(0, 0, this.cropW, this.cropH);
    } catch {
      return null; // tainted canvas / not enough data yet
    }
  }

  /** Sizes the top-left crop for a frame of vw x vh. */
  private region(vw: number, vh: number): void {
    // ANY change in the received size invalidates the cached cell geometry —
    // an increase as much as a decrease. Checking only "crop now bigger than the
    // video" caught the downscale and missed the upscale.
    if (vw !== this.geomW || vh !== this.geomH) {
      this.resetGeometry();
      this.geomW = vw;
      this.geomH = vh;
    }

    if (this.cropW === 0 || this.cropH === 0) {
      this.cropW = Math.min(vw, Math.max(SEARCH_MIN_PX, Math.round(vw * SEARCH_FRACTION)));
      this.cropH = Math.min(vh, Math.max(SEARCH_MIN_PX, Math.round(vh * SEARCH_FRACTION)));
    }
  }

  /** Once located, shrink the readback to the marker's bounding box plus a cell. */
  private tightenCrop(x: number, y: number, cell: number): void {
    const wanted = Math.ceil(x + 30 * cell + cell);
    const wantedH = Math.ceil(y + 19 * cell + cell);
    // The size grab() last read, whichever source it came from.
    const vw = this.geomW;
    const vh = this.geomH;
    const w = Math.min(vw || wanted, wanted);
    const h = Math.min(vh || wantedH, wantedH);
    if (w > 0 && h > 0 && (w < this.cropW || h < this.cropH)) {
      this.cropW = w;
      this.cropH = h;
    }
  }
}
