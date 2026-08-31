/**
 * Unit tests for the microphone capture manager (spec §3.4).
 *
 * Covered here: the pure error mapping, the secure-context/API pre-flight, the
 * gUM constraint shape, and MicCapture's start/stop/ended lifecycle against a
 * mocked navigator.mediaDevices.
 *
 * NOT covered (needs a real browser): an actual permission prompt, real device
 * enumeration, and the OS recording indicator going off on stop().
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import {
  MicCapture,
  MicCaptureError,
  mapMicError,
  micConstraints,
  microphoneSupported,
  unsupportedMicError,
  withUntrustedCertContext,
} from "./mic";

/** Minimal fake MediaStreamTrack that records stop() and fires "ended". */
function fakeTrack() {
  const listeners: Record<string, Array<() => void>> = {};
  return {
    kind: "audio",
    stopped: false,
    stop() {
      this.stopped = true;
    },
    addEventListener(type: string, fn: () => void) {
      (listeners[type] ??= []).push(fn);
    },
    fire(type: string) {
      for (const fn of listeners[type] ?? []) fn();
    },
  };
}

function fakeStream(track: ReturnType<typeof fakeTrack>) {
  return {
    getAudioTracks: () => [track],
    getTracks: () => [track],
  } as unknown as MediaStream;
}

/** Install a getUserMedia stub + secure context. Returns the stub. */
function stubGum(impl: (c: MediaStreamConstraints) => Promise<MediaStream>) {
  const gum = vi.fn(impl);
  vi.stubGlobal("navigator", {
    ...navigator,
    mediaDevices: { getUserMedia: gum },
  });
  Object.defineProperty(window, "isSecureContext", {
    value: true,
    configurable: true,
  });
  return gum;
}

afterEach(() => {
  vi.unstubAllGlobals();
  Object.defineProperty(window, "isSecureContext", {
    value: true,
    configurable: true,
  });
});

describe("mapMicError", () => {
  it("maps NotAllowedError to a permission message", () => {
    const e = mapMicError(Object.assign(new Error("denied"), { name: "NotAllowedError" }));
    expect(e.kind).toBe("permission-denied");
    expect(e.message).toMatch(/denied microphone access/i);
  });

  it("maps SecurityError to the same permission case", () => {
    expect(mapMicError({ name: "SecurityError" }).kind).toBe("permission-denied");
  });

  it("maps NotFoundError and OverconstrainedError to no-device", () => {
    expect(mapMicError({ name: "NotFoundError" }).kind).toBe("no-device");
    expect(mapMicError({ name: "OverconstrainedError" }).kind).toBe("no-device");
  });

  it("maps NotReadableError to device-busy", () => {
    expect(mapMicError({ name: "NotReadableError" }).kind).toBe("device-busy");
  });

  it("falls back to unknown, preserving an Error message", () => {
    const e = mapMicError(new Error("boom"));
    expect(e.kind).toBe("unknown");
    expect(e.message).toBe("boom");
  });

  it("handles non-error values", () => {
    expect(mapMicError("nope").kind).toBe("unknown");
  });
});

describe("unsupportedMicError", () => {
  it("names the insecure context distinctly from a missing API", () => {
    Object.defineProperty(window, "isSecureContext", { value: false, configurable: true });
    const e = unsupportedMicError();
    expect(e.kind).toBe("insecure-context");
    expect(e.message).toMatch(/https/i);
  });

  it("reports unsupported when the context is secure but the API is absent", () => {
    Object.defineProperty(window, "isSecureContext", { value: true, configurable: true });
    expect(unsupportedMicError().kind).toBe("unsupported");
  });
});

describe("withUntrustedCertContext", () => {
  it("re-words a permission denial to name the untrusted certificate", () => {
    const detail = mapMicError({ name: "NotAllowedError" });
    const reworded = withUntrustedCertContext(detail);
    expect(reworded.kind).toBe("untrusted-cert");
    expect(reworded.message).toMatch(/certificate/i);
    expect(reworded.message).toMatch(/\/v1\/tls\/certificate\.pem/);
  });

  it("leaves non-permission failures alone — a missing device is not a certificate problem", () => {
    const noDevice = mapMicError({ name: "NotFoundError" });
    expect(withUntrustedCertContext(noDevice)).toBe(noDevice);
    const busy = mapMicError({ name: "NotReadableError" });
    expect(withUntrustedCertContext(busy)).toBe(busy);
  });
});

describe("microphoneSupported", () => {
  it("is false on an insecure origin even when getUserMedia exists", () => {
    stubGum(() => Promise.resolve(fakeStream(fakeTrack())));
    Object.defineProperty(window, "isSecureContext", { value: false, configurable: true });
    expect(microphoneSupported()).toBe(false);
  });

  it("is false when mediaDevices is absent", () => {
    vi.stubGlobal("navigator", { ...navigator, mediaDevices: undefined });
    expect(microphoneSupported()).toBe(false);
  });

  it("is true on a secure origin with getUserMedia", () => {
    stubGum(() => Promise.resolve(fakeStream(fakeTrack())));
    expect(microphoneSupported()).toBe(true);
  });
});

describe("micConstraints", () => {
  it("always enables AEC/NS/AGC (game audio plays on the same machine)", () => {
    const audio = micConstraints().audio as MediaTrackConstraints;
    expect(audio.echoCancellation).toBe(true);
    expect(audio.noiseSuppression).toBe(true);
    expect(audio.autoGainControl).toBe(true);
    expect("deviceId" in audio).toBe(false);
  });

  it("pins deviceId exactly when a preferred device is given", () => {
    const audio = micConstraints("dev-1").audio as MediaTrackConstraints;
    expect(audio.deviceId).toEqual({ exact: "dev-1" });
  });
});

describe("MicCapture", () => {
  it("returns the audio track and exposes it as active", async () => {
    const track = fakeTrack();
    stubGum(() => Promise.resolve(fakeStream(track)));
    const mic = new MicCapture();
    const got = await mic.start();
    expect(got).toBe(track as unknown as MediaStreamTrack);
    expect(mic.active).toBe(true);
  });

  it("stop() releases the device and is idempotent", async () => {
    const track = fakeTrack();
    stubGum(() => Promise.resolve(fakeStream(track)));
    const mic = new MicCapture();
    await mic.start();
    mic.stop();
    mic.stop();
    expect(track.stopped).toBe(true);
    expect(mic.active).toBe(false);
  });

  it("throws a mapped MicCaptureError when permission is denied", async () => {
    stubGum(() =>
      Promise.reject(Object.assign(new Error("no"), { name: "NotAllowedError" })),
    );
    const mic = new MicCapture();
    await expect(mic.start()).rejects.toBeInstanceOf(MicCaptureError);
    await mic.start().catch((e: MicCaptureError) => {
      expect(e.detail.kind).toBe("permission-denied");
    });
    expect(mic.active).toBe(false);
  });

  it("refuses to prompt on an insecure context", async () => {
    stubGum(() => Promise.resolve(fakeStream(fakeTrack())));
    Object.defineProperty(window, "isSecureContext", { value: false, configurable: true });
    const mic = new MicCapture();
    await mic.start().catch((e: MicCaptureError) => {
      expect(e.detail.kind).toBe("insecure-context");
    });
  });

  it("retries with the default device when a pinned deviceId fails", async () => {
    const track = fakeTrack();
    const gum = stubGum((c) => {
      const audio = c.audio as MediaTrackConstraints;
      if (audio.deviceId) {
        return Promise.reject(Object.assign(new Error("gone"), { name: "OverconstrainedError" }));
      }
      return Promise.resolve(fakeStream(track));
    });
    const mic = new MicCapture();
    await expect(mic.start("stale-device")).resolves.toBeTruthy();
    expect(gum).toHaveBeenCalledTimes(2);
  });

  it("releases and notifies when the device ends underneath us", async () => {
    const track = fakeTrack();
    stubGum(() => Promise.resolve(fakeStream(track)));
    const mic = new MicCapture();
    const onEnded = vi.fn();
    mic.onEnded = onEnded;
    await mic.start();
    track.fire("ended");
    expect(onEnded).toHaveBeenCalledOnce();
    expect(mic.active).toBe(false);
  });
});
