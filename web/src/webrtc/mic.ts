// Microphone capture (client -> host) — spec §3.4,
// docs/design/plans/2026-08-02-microphone-capture-spec.md.
//
// Owns getUserMedia and the captured track's lifetime; no React, no WebRTC
// (SessionPage calls start()/stop() on a user gesture, hands the track to
// QuasarSession.attachMicTrack).
//
// Invariants: capture NEVER starts without a user gesture (nothing runs on
// mount; m-line carries a null sender track until enabled). stop() must
// RELEASE the device (track.stop(), not track.enabled = false) so the OS
// recording indicator goes off.

/** Why a mic enable attempt failed. Drives the user-facing message. */
export type MicErrorKind =
  | "insecure-context"
  | "untrusted-cert"
  | "unsupported"
  | "permission-denied"
  | "no-device"
  | "device-busy"
  | "unknown";

export interface MicError {
  kind: MicErrorKind;
  /** Toast title. */
  title: string;
  /** Toast body — one sentence, actionable. */
  message: string;
}

/**
 * Cheap sync feature detect. getUserMedia is secure-context-only (same
 * constraint gates Keyboard Lock on plain-HTTP LAN deploys, #376), so an
 * insecure origin reports false regardless of browser capability.
 */
export function microphoneSupported(): boolean {
  return (
    typeof navigator !== "undefined" &&
    typeof navigator.mediaDevices?.getUserMedia === "function" &&
    typeof window !== "undefined" &&
    window.isSecureContext === true
  );
}

/**
 * Maps a getUserMedia rejection to a user-facing message (pure, exported for
 * tests). DOMException names per mediacapture-main; `SecurityError` folds into
 * permission-denied — both mean "the browser refused, ask the user".
 */
export function mapMicError(err: unknown): MicError {
  const name =
    typeof err === "object" && err !== null && "name" in err
      ? String((err as { name: unknown }).name)
      : "";

  switch (name) {
    case "NotAllowedError":
    case "SecurityError":
      return {
        kind: "permission-denied",
        title: "Microphone blocked",
        message:
          "Your browser denied microphone access. Allow it for this site in the address-bar permissions, then try again.",
      };
    case "NotFoundError":
    case "OverconstrainedError":
      return {
        kind: "no-device",
        title: "No microphone found",
        message:
          "No capture device is available. Connect a microphone and try again.",
      };
    case "NotReadableError":
    case "AbortError":
      return {
        kind: "device-busy",
        title: "Microphone unavailable",
        message:
          "Another application is using the microphone, or the device could not be started.",
      };
    default:
      return {
        kind: "unknown",
        title: "Microphone failed",
        message:
          err instanceof Error && err.message
            ? err.message
            : "The microphone could not be started.",
      };
  }
}

/**
 * Re-word a permission denial for an origin whose certificate the user
 * bypassed rather than trusted (lib/certTrust.ts said "untrusted-cert").
 *
 * On such an origin `microphoneSupported()` is true (getUserMedia exists and
 * `isSecureContext` is true — measured, see certTrust.ts), so the clear
 * "Microphone needs HTTPS" pre-flight never fires; the user instead gets the
 * generic permission-denied toast telling them to fix the address-bar
 * permission — which the browser will not durably grant on an origin it
 * distrusts. Saying "trust the certificate" is the actionable truth.
 *
 * Only the permission/security denial is re-worded: a missing or busy device
 * is a device problem on any origin, and rewriting those to talk about
 * certificates would misdiagnose them.
 */
export function withUntrustedCertContext(detail: MicError): MicError {
  if (detail.kind !== "permission-denied") return detail;
  return {
    kind: "untrusted-cert",
    title: "Microphone blocked",
    message:
      "This address uses a certificate your device doesn't trust, and the " +
      "browser won't reliably keep microphone permission for it. Download " +
      "the server certificate from /v1/tls/certificate.pem, trust it on " +
      "this device, then reload and try again.",
  };
}

/** The pre-flight failure for a context that cannot capture at all. */
export function unsupportedMicError(): MicError {
  if (typeof window !== "undefined" && window.isSecureContext === false) {
    return {
      kind: "insecure-context",
      title: "Microphone needs HTTPS",
      message:
        "Microphone capture is only available on a secure (HTTPS) address. Reopen Quasar over HTTPS to use voice.",
    };
  }
  return {
    kind: "unsupported",
    title: "Microphone unsupported",
    message: "This browser does not expose a microphone capture API.",
  };
}

/**
 * gUM constraints. AEC/NS/AGC are on by default because the game audio plays
 * out of the same machine — without echo cancellation, voice chat feeds the
 * stream's own audio back to the host (spec §3.4).
 */
export function micConstraints(deviceId?: string): MediaStreamConstraints {
  return {
    audio: {
      echoCancellation: true,
      noiseSuppression: true,
      autoGainControl: true,
      ...(deviceId ? { deviceId: { exact: deviceId } } : {}),
    },
  };
}

/** Thrown by {@link MicCapture.start}; carries the mapped, displayable error. */
export class MicCaptureError extends Error {
  constructor(readonly detail: MicError) {
    super(detail.message);
    this.name = "MicCaptureError";
  }
}

/**
 * Owns one live capture stream. One instance per session page; start/stop may be
 * called repeatedly (each start() releases any previous stream first).
 */
export class MicCapture {
  private stream: MediaStream | null = null;

  /**
   * Fired when the device disappears underneath us (unplug, OS revoke). The
   * caller flips its mic state back to off — the track is already dead, so this
   * is a notification, not a request.
   */
  onEnded: (() => void) | null = null;

  /** The live capture track, or null when the mic is off. */
  get track(): MediaStreamTrack | null {
    return this.stream?.getAudioTracks()[0] ?? null;
  }

  get active(): boolean {
    return this.track != null;
  }

  /**
   * Acquire the microphone. MUST be called from a user gesture.
   *
   * @throws MicCaptureError with a displayable {@link MicError}.
   */
  async start(deviceId?: string): Promise<MediaStreamTrack> {
    if (!microphoneSupported()) throw new MicCaptureError(unsupportedMicError());
    this.stop();
    let stream: MediaStream;
    try {
      stream = await navigator.mediaDevices.getUserMedia(micConstraints(deviceId));
    } catch (err) {
      // A pinned device that no longer exists must not lock out the mic: retry default.
      if (deviceId) {
        try {
          stream = await navigator.mediaDevices.getUserMedia(micConstraints());
        } catch (retryErr) {
          throw new MicCaptureError(mapMicError(retryErr));
        }
      } else {
        throw new MicCaptureError(mapMicError(err));
      }
    }
    const track = stream.getAudioTracks()[0];
    if (!track) {
      stream.getTracks().forEach((t) => t.stop());
      throw new MicCaptureError({
        kind: "no-device",
        title: "No microphone found",
        message: "The browser returned a stream with no audio track.",
      });
    }
    track.addEventListener("ended", () => {
      this.stop();
      this.onEnded?.();
    });
    this.stream = stream;
    return track;
  }

  /** Release the device. Idempotent; safe to call when already stopped. */
  stop(): void {
    if (!this.stream) return;
    for (const t of this.stream.getTracks()) t.stop();
    this.stream = null;
  }
}
