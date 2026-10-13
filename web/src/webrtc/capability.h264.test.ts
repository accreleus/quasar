// #565 — a Firefox without its OpenH264 plugin lists H.264 in
// RTCRtpReceiver.getCapabilities yet answers an H.264 offer with `m=video 0`.
// The probe hands one connection a fixed H.264-only offer, on Firefox only, and
// only the device record built by probeCapabilities carries the verdict. The
// module caches it for the page's life, so each test loads a fresh copy.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const FIREFOX_UA = "Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0";
const CHROME_UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36";

const KEPT = "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 126\r\na=rtpmap:126 H264/90000\r\n";
const REFUSED = "v=0\r\nm=video 0 UDP/TLS/RTP/SAVPF 120\r\na=rtpmap:120 VP8/90000\r\n";

interface Fake {
  offers: string[];
  close: ReturnType<typeof vi.fn>;
}
let made: Fake[] = [];

function stubBrowser(
  ua: string,
  listsH264: boolean,
  opts: { answer?: () => Promise<{ sdp: string }>; setRemote?: () => Promise<void> } = {},
) {
  Object.defineProperty(navigator, "userAgent", { value: ua, configurable: true });
  vi.stubGlobal(
    "RTCPeerConnection",
    class {
      fake: Fake = { offers: [], close: vi.fn() };
      constructor() {
        made.push(this.fake);
      }
      async setRemoteDescription(d: { sdp: string }) {
        this.fake.offers.push(d.sdp);
        await opts.setRemote?.();
      }
      createAnswer = opts.answer ?? (async () => ({ sdp: KEPT }));
      close = this.fake.close;
    },
  );
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
  made = [];
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  delete (navigator as unknown as { userAgent?: unknown }).userAgent;
});

describe("probeH264Decodable", () => {
  it("is true when the answer keeps the video line, and offers only H.264", async () => {
    stubBrowser(FIREFOX_UA, true, { answer: async () => ({ sdp: KEPT }) });
    const { probeH264Decodable } = await load();
    expect(await probeH264Decodable()).toBe(true);
    expect(made).toHaveLength(1);
    expect(made[0].offers[0]).toMatch(/^a=rtpmap:\d+ H264\/90000/m);
    expect(made[0].offers[0]).not.toMatch(/VP8|VP9|AV1/);
  });

  it("is false when the answer refuses the video line", async () => {
    stubBrowser(FIREFOX_UA, true, { answer: async () => ({ sdp: REFUSED }) });
    const { probeH264Decodable } = await load();
    expect(await probeH264Decodable()).toBe(false);
  });

  it("is false when the line is open but H.264 is not what it kept", async () => {
    stubBrowser(FIREFOX_UA, true, { answer: async () => ({ sdp: "m=video 9 UDP/TLS/RTP/SAVPF 120\r\na=rtpmap:120 VP8/90000\r\n" }) });
    const { probeH264Decodable } = await load();
    expect(await probeH264Decodable()).toBe(false);
  });

  it("falls back to the capability list when createAnswer throws", async () => {
    stubBrowser(FIREFOX_UA, true, {
      answer: async () => {
        throw new Error("boom");
      },
    });
    const { probeH264Decodable } = await load();
    expect(await probeH264Decodable()).toBe(true);
  });

  it("falls back to the capability list when setRemoteDescription throws", async () => {
    stubBrowser(FIREFOX_UA, true, {
      setRemote: async () => {
        throw new Error("bad offer");
      },
    });
    const { probeH264Decodable } = await load();
    expect(await probeH264Decodable()).toBe(true);
  });

  it("falls back to the capability list when the connection never answers", async () => {
    vi.useFakeTimers();
    stubBrowser(FIREFOX_UA, true, { answer: () => new Promise(() => {}) });
    const { probeH264Decodable } = await load();
    const verdict = probeH264Decodable();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(await verdict).toBe(true);
    expect(made[0].close).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["keeps", async () => ({ sdp: KEPT })],
    ["refuses", async () => ({ sdp: REFUSED })],
    [
      "throws",
      async (): Promise<{ sdp: string }> => {
        throw new Error("boom");
      },
    ],
  ])("closes the connection when the answer %s", async (_name, answer) => {
    stubBrowser(FIREFOX_UA, true, { answer });
    const { probeH264Decodable } = await load();
    await probeH264Decodable();
    expect(made).toHaveLength(1);
    expect(made[0].close).toHaveBeenCalledTimes(1);
  });

  it("negotiates once per page", async () => {
    stubBrowser(FIREFOX_UA, true, { answer: async () => ({ sdp: REFUSED }) });
    const { probeH264Decodable } = await load();
    await probeH264Decodable();
    await probeH264Decodable();
    expect(made).toHaveLength(1);
  });

  it("does not negotiate on Chrome or Safari", async () => {
    stubBrowser(CHROME_UA, true, { answer: async () => ({ sdp: REFUSED }) });
    const { probeH264Decodable } = await load();
    expect(await probeH264Decodable()).toBe(true);
    expect(made).toHaveLength(0);
  });

  it("does not negotiate when the list already leaves H.264 out", async () => {
    stubBrowser(FIREFOX_UA, false);
    const { probeH264Decodable } = await load();
    expect(await probeH264Decodable()).toBe(false);
    expect(made).toHaveLength(0);
  });
});

describe("where the verdict lands", () => {
  async function postedAndListed() {
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: false, status: 500 })) as unknown as typeof fetch);
    const { probeCapabilities, probeCodecs } = await load();
    const record = await probeCapabilities();
    return { posted: record.codecs, listed: probeCodecs() };
  }

  it("the device record says h264 false for a Firefox that refuses its own H.264 offer, and probeCodecs still reports the list", async () => {
    stubBrowser(FIREFOX_UA, true, { answer: async () => ({ sdp: REFUSED }) });
    const { posted, listed } = await postedAndListed();
    expect(posted.h264).toBe(false);
    expect(listed.h264).toBe(true);
  });

  it("the device record is the list's answer on Chrome", async () => {
    stubBrowser(CHROME_UA, true, { answer: async () => ({ sdp: REFUSED }) });
    const { posted, listed } = await postedAndListed();
    expect(posted).toEqual(listed);
  });
});
