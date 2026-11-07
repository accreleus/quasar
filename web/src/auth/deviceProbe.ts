// Posts this browser's capability record when the stored one is stale. The launch awaits it
// too, so a record from before a probe key existed never silently plans WebRTC
// (control-api.md §POST /v1/sessions — transport: `device_no_webcodecs`).

import * as authApi from "../api/auth";
import {
  deviceProbeIsFresh,
  getOrCreateDeviceKey,
  markDeviceProbePosted,
  probeCapabilities,
  probeH264Decodable,
} from "../webrtc/capability";

/** One probe-and-post is abandoned after this, so a hung post cannot leave `inFlight` set for good. */
const PROBE_POST_BOUND_MS = 20_000;

/** The launch waits this long for a stale record to be posted, then goes on without it. */
export const LAUNCH_PROBE_WAIT_MS = 4_000;

/** Rejects when `p` takes longer than the bound; `p` itself is left to finish. */
function withBound(p: Promise<void>): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  return Promise.race([
    p,
    new Promise<void>((_, reject) => {
      timer = setTimeout(() => reject(new Error("device probe timed out")), PROBE_POST_BOUND_MS);
    }),
  ]).finally(() => clearTimeout(timer));
}

let inFlight: Promise<void> | null = null;

/** Posts the probe unless this browser's last post is current. One post at a time; rejects when it fails. */
export function ensureDeviceProbe(token: string, userId: string): Promise<void> {
  inFlight ??= withBound(
    (async () => {
      if (deviceProbeIsFresh(userId, await probeH264Decodable())) return;
      const caps = await probeCapabilities();
      await authApi.postDevice(token, getOrCreateDeviceKey(), caps);
      markDeviceProbePosted(userId, caps.codecs.h264);
    })(),
  ).finally(() => {
    inFlight = null;
  });
  return inFlight;
}

/** {@link ensureDeviceProbe} bounded by {@link LAUNCH_PROBE_WAIT_MS}; never rejects. */
export async function ensureDeviceProbeForLaunch(token: string, userId: string): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  await Promise.race([
    ensureDeviceProbe(token, userId).catch(() => {}),
    new Promise<void>((r) => {
      timer = setTimeout(r, LAUNCH_PROBE_WAIT_MS);
    }),
  ]);
  clearTimeout(timer);
}
