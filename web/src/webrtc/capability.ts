// Login-time connection + decode-capability probe. Three measurements collected
// async after login: codec acceptance (RTCRtpReceiver.getCapabilities — "browser
// will accept", NOT "hardware-decodes"), RTT (one timed HEAD/GET, best-effort),
// bandwidth (timed GET of a fixed asset -> kbps, a rough indication only, no
// optimizer relies on its precision).
//
// device_key (schema.md user_devices): random UUID in localStorage
// (DEVICE_KEY_STORAGE_KEY), app-scoped, NOT a hardware fingerprint. Cleared with
// browser storage; a new UUID on next login is an acceptable new user_devices row.

import { microphoneSupported } from "./mic";
import { pointerLockSupported } from "../input/capture";

const DEVICE_KEY_STORAGE_KEY = "quasar_device_key";

/**
 * `crypto.randomUUID()` is secure-context-only; calling it over an insecure
 * http://<lan-ip> threw and silently aborted the whole login probe (#79). Build
 * v4 from `crypto.getRandomValues()` instead, using randomUUID as a fast path.
 */
function randomUuidV4(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  b[6] = (b[6] & 0x0f) | 0x40; // version 4
  b[8] = (b[8] & 0x3f) | 0x80; // variant 10
  const hex = Array.from(b, (x) => x.toString(16).padStart(2, "0"));
  return `${hex.slice(0, 4).join("")}-${hex.slice(4, 6).join("")}-${hex
    .slice(6, 8)
    .join("")}-${hex.slice(8, 10).join("")}-${hex.slice(10, 16).join("")}`;
}

/** Returns the stable device key, creating and persisting it if absent. */
export function getOrCreateDeviceKey(): string {
  let key = localStorage.getItem(DEVICE_KEY_STORAGE_KEY);
  if (!key) {
    key = randomUuidV4();
    localStorage.setItem(DEVICE_KEY_STORAGE_KEY, key);
  }
  return key;
}

/**
 * Freshness gate for `POST /v1/me/devices`.
 *
 * The probe is re-posted on session rehydration, which is every full page
 * load — enough of them on one token and the endpoint rate-limits (429). The
 * server's staleness horizon is 30 days (probeMaxAgeDays), so a day is ample.
 * Keyed by user id: two accounts sharing a browser each need their own row.
 */
const DEVICE_POST_STORAGE_KEY = "quasar_device_posted";
const DEVICE_POST_TTL_MS = 24 * 60 * 60 * 1000;

export function deviceProbeIsFresh(userId: string, now: number = Date.now()): boolean {
  try {
    const raw = localStorage.getItem(DEVICE_POST_STORAGE_KEY);
    if (!raw) return false;
    const { user, at } = JSON.parse(raw) as { user?: string; at?: number };
    if (user !== userId || typeof at !== "number") return false;
    return now - at >= 0 && now - at < DEVICE_POST_TTL_MS;
  } catch {
    return false;
  }
}

export function markDeviceProbePosted(userId: string, now: number = Date.now()): void {
  try {
    localStorage.setItem(DEVICE_POST_STORAGE_KEY, JSON.stringify({ user: userId, at: now }));
  } catch {
    /* Storage full or blocked: the only cost is posting again next load. */
  }
}

/** Codec acceptance map: which of the four codecs the browser will handle. */
export interface CodecCapabilities {
  h264: boolean;
  hevc: boolean;
  av1: boolean;
  vp9: boolean;
}

/** "Browser will accept in SDP", NOT "hardware decoded". */
export function probeCodecs(): CodecCapabilities {
  if (!("RTCRtpReceiver" in window) || typeof RTCRtpReceiver.getCapabilities !== "function") {
    return { h264: true, hevc: false, av1: false, vp9: false }; // non-WebRTC browser fallback
  }

  const caps = RTCRtpReceiver.getCapabilities("video");
  if (!caps) {
    return { h264: false, hevc: false, av1: false, vp9: false };
  }

  const mimeTypes = caps.codecs.map((c) => c.mimeType.toLowerCase());
  return {
    h264: mimeTypes.some((m) => m.includes("h264") || m.includes("avc")),
    hevc: mimeTypes.some((m) => m.includes("hevc") || m.includes("h265")),
    av1:  mimeTypes.some((m) => m.includes("av1")),
    vp9:  mimeTypes.some((m) => m.includes("vp9")),
  };
}

/**
 * Heuristic: the API doesn't directly expose a max decode resolution. Presence
 * of any sdpFmtpLine is treated as "negotiated at some level" -> assume 4K
 * capable rather than decoding level-id into a real pixel cap.
 */
function probeMaxDecodeHeight(): number {
  if (!("RTCRtpReceiver" in window) || typeof RTCRtpReceiver.getCapabilities !== "function") {
    return 1080; // conservative fallback
  }
  const caps = RTCRtpReceiver.getCapabilities("video");
  if (!caps || caps.codecs.length === 0) return 1080;

  for (const codec of caps.codecs) {
    const fmtp = codec.sdpFmtpLine ?? "";
    if (fmtp) {
      return 2160;
    }
  }
  return 2160;
}

/**
 * A cold fetch measures TCP setup, not RTT (#146: 107ms reported on a 5ms LAN,
 * failing the 1080p60-lan rtt<=10 gate) — so warm the connection first, then
 * min-of-3 timed HEADs over keep-alive.
 */
async function probeRttMs(): Promise<number | null> {
  try {
    await fetch("/health", { method: "HEAD", cache: "no-store" }); // warm the connection
    let best = Infinity;
    for (let i = 0; i < 3; i++) {
      const t0 = performance.now();
      const res = await fetch("/health", { method: "HEAD", cache: "no-store" });
      const t1 = performance.now();
      if (!res.ok && res.status !== 404) return null;
      best = Math.min(best, t1 - t0);
    }
    return Number.isFinite(best) ? Math.max(1, Math.round(best)) : null;
  } catch {
    return null;
  }
}

// Multi-codec spec §6.1: navigator.mediaCapabilities.decodingInfo adds richer,
// async decode-quality signal for HEVC/AV1 (H.264 needs no refinement). Forward-
// data only — the flat `codecs` map remains the eligibility gate.

/** One navigator.mediaCapabilities.decodingInfo() result for a single codec. */
export interface CodecDecodingDetail {
  supported: boolean;
  smooth: boolean;
  power_efficient: boolean;
}

/** Forward-data decode-quality detail for the non-baseline codecs (additive JSONB field). */
export interface CodecsDetail {
  hevc?: CodecDecodingDetail;
  av1?: CodecDecodingDetail;
}

/**
 * 1080p60 8Mbps WebRTC decode config (the AS10-03 constrained-baseline rung's
 * shape). Never throws: absent API / unsupported config / runtime error all
 * resolve null, same best-effort posture as probeRttMs/probeBandwidthKbps.
 */
async function probeDecodingInfo(contentType: string): Promise<CodecDecodingDetail | null> {
  try {
    if (
      typeof navigator === "undefined" ||
      !("mediaCapabilities" in navigator) ||
      typeof navigator.mediaCapabilities?.decodingInfo !== "function"
    ) {
      return null;
    }
    const info = await navigator.mediaCapabilities.decodingInfo({
      type: "webrtc",
      video: {
        contentType,
        width: 1920,
        height: 1080,
        framerate: 60,
        bitrate: 8_000_000,
      },
    });
    return {
      supported: info.supported,
      smooth: info.smooth,
      power_efficient: info.powerEfficient,
    };
  } catch {
    return null;
  }
}

/** Best-effort: either or both may be absent from the result, never thrown. */
async function probeCodecsDetail(): Promise<CodecsDetail> {
  const [hevc, av1] = await Promise.all([
    probeDecodingInfo("video/H265"),
    probeDecodingInfo("video/AV1"),
  ]);
  const detail: CodecsDetail = {};
  if (hevc) detail.hevc = hevc;
  if (av1) detail.av1 = av1;
  return detail;
}

/**
 * Fetches the app's own largest script asset (cache-busted) and reads
 * transferSize over responseStart->responseEnd (excludes TTFB, body only).
 * #146: timing the ~30-byte /health body read ~2kbps on a fast LAN and
 * floor-tiered every user; the 8/12/25 Mbps tier gates need real asset bytes to
 * measure accurately. Null means unmeasured -> server uses the default tier.
 */
async function probeBandwidthKbps(): Promise<number | null> {
  try {
    // Find this page's largest loaded script — a real, served-by-us payload.
    const scripts = Array.from(document.querySelectorAll<HTMLScriptElement>("script[src]"))
      .map((s) => s.src)
      .filter((src) => src.startsWith(location.origin));
    if (scripts.length === 0) return null;
    // Prefer the main bundle under /assets/ (hashed, biggest); else first script.
    const target = scripts.find((s) => s.includes("/assets/")) ?? scripts[0];
    const url = `${target}${target.includes("?") ? "&" : "?"}probe=${Date.now()}`;

    const res = await fetch(url, { cache: "no-store" });
    await res.arrayBuffer(); // drain the body so responseEnd is stamped
    if (!res.ok) return null;

    const entries = performance.getEntriesByName(url) as PerformanceResourceTiming[];
    const e = entries[entries.length - 1];
    if (!e || e.responseEnd <= e.responseStart) return null;
    const bytes = e.transferSize > 0 ? e.transferSize : e.encodedBodySize;
    if (bytes < 50_000) return null; // too small to measure throughput meaningfully
    const durationSec = (e.responseEnd - e.responseStart) / 1000;
    return Math.round((bytes * 8) / durationSec / 1000); // kbps
  } catch {
    return null;
  }
}

// Client performance certification fields: best-effort feature detection, any
// may be absent; server sanitizes and stores the blob opaquely.

/** Browser name + version, best-effort from UA-CH with a UA-string fallback. */
export interface BrowserInfo {
  name: string;
  version: string;
}

/** Display geometry. refresh_hz is omitted when not measurable. */
export interface DisplayInfo {
  width: number;
  height: number;
  device_pixel_ratio: number;
  refresh_hz?: number;
}

/** Playout/input feature support, via feature detection. */
export interface ClientFeatures {
  /** RTCRtpReceiver.jitterBufferTarget support (playout knob). */
  jitter_buffer_target: boolean;
  /** RTCRtpReceiver playoutDelayHint support (playout knob). */
  playout_delay_hint: boolean;
  /** False on iPadOS Safari (no Element.requestPointerLock at any version). */
  pointer_lock: boolean;
  /** PointerEvent.getCoalescedEvents support (high-rate input). */
  coalesced_pointer_events: boolean;
  gamepad: boolean;
  /**
   * Mic spec §3.4: getUserMedia present AND secure context. Informational only
   * (not a permission probe, doesn't gate anything) — the launch request's `mic`
   * field + server instance setting decide the mic m-line.
   */
  microphone: boolean;
}

/**
 * Shape only; populated by the AS10-10 playback harness. `h264_profile_decoded`
 * is load-bearing: codec acceptance is not proof of hardware decode (AS10-03).
 */
export interface ProfileCertification {
  /** The H.264 profile that actually decoded, e.g. "constrained-baseline". */
  h264_profile_decoded?: string;
  /** Whether the stream decoded at all on this device. */
  decode_pass?: boolean;
  /** Whether presentation (display) met the profile's pacing bar. */
  present_pass?: boolean;
  /** Mean decode time per frame (ms). */
  decode_ms?: number;
  /** Observed presentation frame rate. */
  present_fps?: number;
  /** Fraction of frames dropped at presentation. */
  dropped_ratio?: number;
  /** When this certification was measured (RFC3339). */
  measured_at?: string;
}

/** Map of stream-profile id → certification result. Empty until AS10-10. */
export type ProfileCertifications = Record<string, ProfileCertification>;

function probeBrowser(): BrowserInfo {
  const uaData = (navigator as Navigator & { userAgentData?: NavigatorUAData }).userAgentData;
  if (uaData && Array.isArray(uaData.brands)) {
    // Skip "Not.A/Brand" GREASE entries.
    const real = uaData.brands.find(
      (b) => b.brand && !/not.?a.?brand/i.test(b.brand),
    );
    if (real) return { name: real.brand, version: real.version };
  }
  const ua = navigator.userAgent || "";
  const m =
    /(Edg|OPR|Chrome|Firefox|Safari)\/([\d.]+)/.exec(ua) ?? null;
  if (m) {
    const name = m[1] === "Edg" ? "Edge" : m[1] === "OPR" ? "Opera" : m[1];
    return { name, version: m[2] };
  }
  return { name: "unknown", version: "" };
}

function probePlatform(): string {
  const uaData = (navigator as Navigator & { userAgentData?: NavigatorUAData }).userAgentData;
  if (uaData && typeof uaData.platform === "string" && uaData.platform) {
    return uaData.platform;
  }
  return navigator.platform || "unknown";
}

async function probeDisplay(): Promise<DisplayInfo> {
  const info: DisplayInfo = {
    width: screen.width,
    height: screen.height,
    device_pixel_ratio: window.devicePixelRatio || 1,
  };
  const hz = await sampleRefreshHz();
  if (hz != null) info.refresh_hz = hz;
  return info;
}

/** Null when rAF is unavailable or the sample is unusable (e.g. throttled bg tab). */
function sampleRefreshHz(): Promise<number | null> {
  return new Promise((resolve) => {
    if (typeof requestAnimationFrame !== "function") {
      resolve(null);
      return;
    }
    const frames: number[] = [];
    const SAMPLES = 10;
    let count = 0;
    const tick = (t: number) => {
      frames.push(t);
      count++;
      if (count <= SAMPLES) {
        requestAnimationFrame(tick);
        return;
      }
      // Need at least a couple of intervals to estimate.
      if (frames.length < 3) {
        resolve(null);
        return;
      }
      const deltas: number[] = [];
      for (let i = 1; i < frames.length; i++) deltas.push(frames[i] - frames[i - 1]);
      deltas.sort((a, b) => a - b);
      const median = deltas[Math.floor(deltas.length / 2)];
      if (!Number.isFinite(median) || median <= 0) {
        resolve(null);
        return;
      }
      const hz = Math.round(1000 / median);
      // Reject implausible values (a throttled background tab reports very low).
      if (hz < 20 || hz > 480) {
        resolve(null);
        return;
      }
      resolve(hz);
    };
    requestAnimationFrame(tick);
  });
}

export function probeFeatures(): ClientFeatures {
  const rtpReceiverProto =
    typeof RTCRtpReceiver !== "undefined" ? RTCRtpReceiver.prototype : undefined;
  const pointerEventProto =
    typeof PointerEvent !== "undefined" ? PointerEvent.prototype : undefined;
  return {
    jitter_buffer_target: !!rtpReceiverProto && "jitterBufferTarget" in rtpReceiverProto,
    playout_delay_hint: !!rtpReceiverProto && "playoutDelayHint" in rtpReceiverProto,
    // Probed on Element.prototype, NOT `"pointerLockElement" in document` — that
    // document property ships independently of the element method the grab
    // gesture actually calls, so it was never a real proxy for this capability.
    pointer_lock: pointerLockSupported(),
    coalesced_pointer_events: !!pointerEventProto && "getCoalescedEvents" in pointerEventProto,
    gamepad: typeof navigator !== "undefined" && typeof navigator.getGamepads === "function",
    microphone: microphoneSupported(),
  };
}

/** Minimal structural type for the UA-CH navigator.userAgentData API. */
interface NavigatorUAData {
  brands?: Array<{ brand: string; version: string }>;
  platform?: string;
}

/** Payload sent to POST /v1/me/devices (control-api.md). */
export interface DeviceCapabilities {
  client_type: "web";
  codecs: CodecCapabilities;
  /** Additive; absent on browsers without the MediaCapabilities API (spec §6.1). */
  codecs_detail?: CodecsDetail;
  max_decode_height: number;
  browser: BrowserInfo;
  platform: string;
  display: DisplayInfo;
  features: ClientFeatures;
  /** Keyed by stream-profile id. Shape only (AS10-08); populated by AS10-10. */
  profiles: ProfileCertifications;
  /** Best-effort, not a real throughput test. */
  bandwidth_kbps: number | null;
  rtt_ms: number | null;
}

// ── Native client capability report (AS10-12) ────────────────────────────────
//
// The native client (future; see protocol/native-client.md +
// docs/phase9/native-client-architecture.md) reports an *additive superset* of
// DeviceCapabilities to the SAME POST /v1/me/devices endpoint and JSONB column.
// These interfaces document the native shape for implementers; no web code
// sends them. All native-only fields are optional — absent != false.

export interface NativeOsInfo {
  name: string;
  version: string;
  arch: string;
}

export interface NativeDisplayInfo extends DisplayInfo {
  hdr?: boolean;
  vrr?: boolean;
}

/** Forward-data only — NOT an eligibility gate (flat `codecs` map is). */
export interface CodecDecodeInfo {
  hw: boolean;
  profiles: string[];
  levels: string[];
  max_height: number;
}

export interface NativeDecodeMatrix {
  h264?: CodecDecodeInfo;
  /** Placeholder (CodecFuture in profile.go). */
  hevc?: CodecDecodeInfo;
  /** Placeholder (CodecFuture in profile.go). */
  av1?: CodecDecodeInfo;
}

export interface NativeAudioInfo {
  channels: number;
  sample_rate: number;
  codecs: string[];
}

export interface NativeController {
  type: string;
  rumble: boolean;
  haptics: boolean;
  player: number;
}

export interface NativeInputInfo {
  raw_mouse: boolean;
  keyboard: boolean;
  high_rate_input: boolean;
  controllers: NativeController[];
}

/** Reuses the BrowserMetrics vocabulary (web/src/api/types.ts). Forward-data. */
export interface NativeMetrics {
  decode_ms?: number;
  present_fps?: number;
  /** Judder; the #108 pacing metric. */
  present_interval_sd_ms?: number;
  glass_to_glass_ms_p50?: number;
  /** Both p50 AND p95 matter — the spike residual was a p95 tail, not the median. */
  glass_to_glass_ms_p95?: number;
  interactive_ms_p50?: number;
  jitter_buffer_ms?: number;
  render_path?: string;
}

/** Reuses the AS10-11 ClientHealth vocabulary. */
export interface NativeHealth {
  class: string;
  reason?: string;
}

/** Additive superset of DeviceCapabilities — protocol/native-client.md. */
export interface NativeCapabilities extends Omit<DeviceCapabilities, "client_type"> {
  client_type: "native" | "web";
  report_version?: number;
  client_name?: string;
  client_version?: string;
  os?: NativeOsInfo;
  display: NativeDisplayInfo;
  decode?: NativeDecodeMatrix;
  audio?: NativeAudioInfo;
  input?: NativeInputInfo;
  metrics?: NativeMetrics;
  health?: NativeHealth;
}

/** All measurements are best-effort; null values are valid (JSONB column). */
export async function probeCapabilities(): Promise<DeviceCapabilities> {
  const [rttMs, bandwidthKbps, display, codecsDetail] = await Promise.all([
    probeRttMs(),
    probeBandwidthKbps(),
    probeDisplay(),
    probeCodecsDetail(),
  ]);

  return {
    client_type:       "web",
    codecs:            probeCodecs(),
    codecs_detail:     codecsDetail,
    max_decode_height: probeMaxDecodeHeight(),
    browser:           probeBrowser(),
    platform:          probePlatform(),
    display,
    features:          probeFeatures(),
    profiles:          {}, // populated by the AS10-10 playback harness
    bandwidth_kbps:    bandwidthKbps,
    rtt_ms:            rttMs,
  };
}
