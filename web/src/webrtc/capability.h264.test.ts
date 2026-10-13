// #565 — a Firefox without its OpenH264 plugin lists H.264 in
// RTCRtpReceiver.getCapabilities yet answers an H.264 offer with `m=video 0`.
// The probe asks it with a loopback negotiation, on Firefox only. The module
// caches the verdict for the page's life, so each test loads a fresh copy.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const FIREFOX_UA = "Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0";
const CHROME_UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36";

const KEPT = "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 126\r\na=rtpmap:126 H264/90000\r\n";
const REFUSED = "v=0\r\nm=video 0 UDP/TLS/RTP/SAVPF 120\r\na=rtpmap:120 VP8/90000\r\n";

type Answer = () => Promise<{ sdp: string }>;
let built = 0;

function fakePeerConnection(answer: Answer) {
  return class {
    constructor() {
      built++;
    }
    addTransceiver() {
      return { setCodecPreferences() {} };
    }
    async createOffer() {
      return { sdp: "offer" };
    }
    async setRemoteDescription() {}
    createAnswer = answer;
    close = vi.fn();
  };
}

function stubBrowser(ua: string, listsH264: boolean, answer: Answer) {
  Object.defineProperty(navigator, "userAgent", { value: ua, configurable: true });
  vi.stubGlobal("RTCPeerConnection", fakePeerConnection(answer));
  vi.stubGlobal("RTCRtpReceiver", {
    getCapabilities: () => ({
      codecs: [{ mimeType: "video/VP8" }, ...(listsH264 ? [{ mimeType: "video/H264" }] : [])],
    }),
  });
}

async function load() {
  vi.resetModules();
  return import("./capability");
}

beforeEach(() => {
  built = 0;
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  delete (navigator as unknown as { userAgent?: unknown }).userAgent;
});

describe("probeH264Decodable", () => {
  it("is true when the answer keeps the video line", async () => {
    stubBrowser(FIREFOX_UA, true, async () => ({ sdp: KEPT }));
    const { probeH264Decodable, probeCodecs } = await load();
    expect(await probeH264Decodable()).toBe(true);
    expect(probeCodecs().h264).toBe(true);
  });

  it("is false when the answer refuses the video line, and probeCodecs follows", async () => {
    stubBrowser(FIREFOX_UA, true, async () => ({ sdp: REFUSED }));
    const { probeH264Decodable, probeCodecs } = await load();
    expect(await probeH264Decodable()).toBe(false);
    expect(probeCodecs().h264).toBe(false);
  });

  it("falls back to the capability list when the negotiation throws", async () => {
    stubBrowser(FIREFOX_UA, true, async () => {
      throw new Error("boom");
    });
    const { probeH264Decodable, probeCodecs } = await load();
    expect(await probeH264Decodable()).toBe(true);
    expect(probeCodecs().h264).toBe(true);
  });

  it("falls back to the capability list when the negotiation never answers", async () => {
    vi.useFakeTimers();
    stubBrowser(FIREFOX_UA, true, () => new Promise(() => {}));
    const { probeH264Decodable } = await load();
    const verdict = probeH264Decodable();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(await verdict).toBe(true);
  });

  it("negotiates once per page", async () => {
    stubBrowser(FIREFOX_UA, true, async () => ({ sdp: REFUSED }));
    const { probeH264Decodable } = await load();
    await probeH264Decodable();
    await probeH264Decodable();
    expect(built).toBe(2); // one loopback pair
  });

  it("does not negotiate on Chrome or Safari", async () => {
    stubBrowser(CHROME_UA, true, async () => ({ sdp: REFUSED }));
    const { probeH264Decodable, probeCodecs } = await load();
    expect(await probeH264Decodable()).toBe(true);
    expect(probeCodecs().h264).toBe(true);
    expect(built).toBe(0);
  });

  it("does not negotiate when the list already leaves H.264 out", async () => {
    stubBrowser(FIREFOX_UA, false, async () => ({ sdp: KEPT }));
    const { probeH264Decodable, probeCodecs } = await load();
    expect(await probeH264Decodable()).toBe(false);
    expect(probeCodecs().h264).toBe(false);
    expect(built).toBe(0);
  });
});

describe("probeCapabilities", () => {
  it("reports h264 false for a Firefox that refuses its own H.264 offer", async () => {
    stubBrowser(FIREFOX_UA, true, async () => ({ sdp: REFUSED }));
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: false, status: 500 })) as unknown as typeof fetch);
    const { probeCapabilities } = await load();
    expect((await probeCapabilities()).codecs.h264).toBe(false);
  });
});
