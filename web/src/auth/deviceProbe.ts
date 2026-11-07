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

/** The launch waits this long for a stale record to be posted, then goes on without it. */
export const LAUNCH_PROBE_WAIT_MS = 4_000;

let inFlight: Promise<void> | null = null;

/** Posts the probe unless this browser's last post is current. One post at a time; rejects when it fails. */
export function ensureDeviceProbe(token: string, userId: string): Promise<void> {
  inFlight ??= (async () => {
    if (deviceProbeIsFresh(userId, await probeH264Decodable())) return;
    const caps = await probeCapabilities();
    await authApi.postDevice(token, getOrCreateDeviceKey(), caps);
    markDeviceProbePosted(userId, caps.codecs.h264);
  })().finally(() => {
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
