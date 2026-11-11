// The launch-options columns, verdict and healing (v3 handoff §B "Launch
// options"), over the real rung table.
//
// The mock hard-codes a bitrate model, a fixed 2160/1440/1080/720 ladder and
// two arbitrary rules ("no 120 fps with H.264", "no 2160p at 120 fps"). None of
// that ships: `GET /v1/me/profiles?app_id=` returns the launch profiles this
// user, app and device actually resolve to, each an ordered chain of rungs with
// its own bitrate and its own eligibility verdict. So the columns here are the
// catalogue's, and "not available" means no rung exists rather than a constant
// says so.

import { describe, expect, it } from "vitest";
import type { LaunchProfilePyroWave, ProfilesResponse } from "../../../api/types";
import { buildOptionSpace, optionsFor, resolveSelection, type LaunchDraft, type OptionSpace } from "../launchOptions";
import { heal, launchSpec, optionColumns, recommendation, verdict } from "./launchOptionRules";

const CAPS = { h264: true, hevc: true, av1: true, vp9: false } as const;

const rung = (
  codec: string,
  width: number,
  height: number,
  fps: number,
  kbps: number,
  eligibility: "eligible" | "risky" | "ineligible" = "eligible",
  reasons: { code: string; message: string }[] = [],
  position = 1,
) => ({
  id: `${codec}-${height}-${fps}`,
  display_name: `${height}p${fps}`,
  codec,
  width,
  height,
  fps,
  nominal_bitrate_kbps: kbps,
  position,
  eligibility,
  reasons,
});

const profile = (
  id: string,
  rungs: ReturnType<typeof rung>[],
  eligibility: "eligible" | "risky" | "ineligible" = "eligible",
  reasons: { code: string; message: string }[] = [],
) => ({
  id,
  display_name: id,
  description: "",
  nominal: {
    width: rungs[0].width,
    height: rungs[0].height,
    fps: rungs[0].fps,
    bitrate_kbps: rungs[0].nominal_bitrate_kbps,
  },
  eligibility,
  reasons,
  rungs,
});

const BANDWIDTH = [
  { code: "bandwidth_too_low", message: "bandwidth is below the recommended headroom" },
];

/** The plan's fixture: av1 1440p60 recommended, h264 1080p60 eligible,
 *  hevc 2160p60 risky. (Catalog vocabulary is `hevc`; `h265` is the wire's.) */
const RESPONSE = {
  recommended_id: "p-av1",
  confidence: "high",
  notes: [],
  profiles: [
    profile("p-hevc", [rung("hevc", 3840, 2160, 60, 18000, "risky", BANDWIDTH)], "risky", BANDWIDTH),
    profile("p-av1", [rung("av1", 2560, 1440, 60, 12000)]),
    profile("p-h264", [rung("h264", 1920, 1080, 60, 8000)]),
  ],
} as unknown as ProfilesResponse;

/** One profile whose chain drops to a 120 fps h264 rung: the frame-rate
 *  universe then holds a rate Auto itself has no row for. */
const MIXED_FPS = {
  recommended_id: "p-1",
  confidence: "high",
  notes: [],
  profiles: [
    profile("p-1", [
      rung("av1", 2560, 1440, 60, 12000),
      rung("h264", 1920, 1080, 120, 14000, "eligible", [], 2),
    ]),
  ],
} as unknown as ProfilesResponse;

function spaceOf(res: ProfilesResponse = RESPONSE): OptionSpace {
  return buildOptionSpace(res.profiles, CAPS, res.recommended_id);
}

const draft = (over: Partial<LaunchDraft> = {}): LaunchDraft => ({
  codec: "auto",
  fps: 60,
  height: 1440,
  ...over,
});

describe("optionColumns", () => {
  const space = spaceOf();

  it("offers Auto plus every codec the catalogue has and the device can decode", () => {
    const cols = optionColumns(space, draft());
    expect(cols.codec.map((r) => r.label)).toEqual(["Auto", "H.264", "HEVC", "AV1", "Ultra-low latency (PyroWave)"]);
    expect(cols.codec.filter((r) => r.value !== "pyrowave").every((r) => r.enabled)).toBe(true);
    expect(cols.codec.find((r) => r.selected)?.value).toBe("auto");
  });

  it("drops a codec this device cannot decode — it is never a row to explain", () => {
    const noAv1 = buildOptionSpace(RESPONSE.profiles, { h264: true, hevc: true, av1: false, vp9: false }, "p-av1");
    const cols = optionColumns(noAv1, draft({ codec: "h264", height: 1080 }));
    expect(cols.codec.map((r) => r.label)).toEqual(["Auto", "H.264", "HEVC", "Ultra-low latency (PyroWave)"]);
  });

  it("explains the disabled frame rates once, under their column", () => {
    const cols = optionColumns(spaceOf(MIXED_FPS), draft({ height: 1440 }));
    expect(cols.fps.some((r) => !r.enabled)).toBe(true);
    expect(cols.fpsHint).toBe(
      "A frame rate this codec cannot reach is shown but cannot be picked.",
    );
  });

  it("says nothing under a frame-rate column where every rate is offered", () => {
    const cols = optionColumns(spaceOf(), draft());
    expect(cols.fps.every((r) => r.enabled)).toBe(true);
    expect(cols.fpsHint).toBeUndefined();
  });

  it("prices every frame-rate row at the drafted height, not only the selected one", () => {
    const res = {
      ...RESPONSE,
      profiles: [
        profile("p-60", [rung("av1", 2560, 1440, 60, 12000)]),
        profile("p-120", [rung("av1", 2560, 1440, 120, 20000)]),
      ],
    } as unknown as ProfilesResponse;
    const cols = optionColumns(spaceOf(res), draft({ codec: "av1", height: 1440, fps: 60 }));
    expect(cols.fps.map((r) => [r.value, r.sub])).toEqual([
      [60, "12 Mbps at 1440p"],
      [120, "20 Mbps at 1440p"],
    ]);
  });

  it("blames the frame rate, not Auto, when no codec has been chosen", () => {
    const cols = optionColumns(spaceOf(MIXED_FPS), draft({ height: 1440 }));
    const at120 = cols.fps.find((r) => r.value === 120)!;
    expect(at120.enabled).toBe(false);
    expect(at120.title).toBe("Not available at 120 fps on this device");
    expect(at120.sub).toBe("Not available on this device");
  });

  it("blames the resolution, not Auto, on a dead resolution row", () => {
    const res = {
      ...RESPONSE,
      profiles: [
        profile("p-4k", [rung("av1", 3840, 2160, 60, 25000)]),
        profile("p-1080", [rung("av1", 1920, 1080, 120, 12000)]),
      ],
    } as unknown as ProfilesResponse;
    const cols = optionColumns(spaceOf(res), draft({ height: 2160, fps: 60 }));
    const fhd = cols.resolution.find((r) => r.value === 1080)!;
    expect(fhd.enabled).toBe(false);
    expect(fhd.title).toBe("Not available at 1080p on this device");
    expect(fhd.sub).toBe("1920×1080 · not available at 60 fps");
  });

  it("names what Auto lands on for the drafted row", () => {
    const cols = optionColumns(space, draft());
    expect(cols.codecHint).toMatch(/Your host picks the codec/);
    expect(cols.codecHint).toMatch(/Here it lands on AV1\./);
  });

  it("gives an explicit codec its own basis, verbatim from the mock", () => {
    const cols = optionColumns(space, draft({ codec: "hevc", height: 2160 }));
    expect(cols.codecHint).toBe(
      "HEVC gives you the same picture as H.264 for noticeably less bandwidth.",
    );
  });

  it("subs each resolution row with its pixels and its own bitrate", () => {
    const cols = optionColumns(space, draft());
    expect(cols.resolution.map((r) => r.label)).toEqual(["4K", "1440p", "1080p"]);
    expect(cols.resolution[1]).toMatchObject({
      value: 1440,
      enabled: true,
      selected: true,
      tags: ["recommended"],
      sub: "2560×1440 · 12 Mbps at 60 fps",
    });
    expect(cols.resolution[0]).toMatchObject({ value: 2160, tags: ["risky"], enabled: true });
    // A risky row keeps its spec line and says what the risk is.
    expect(cols.resolution[0].sub).toBe("3840×2160 · 18 Mbps at 60 fps");
    expect(cols.resolution[0].why).toMatch(/little room to spare/);
  });

  it("disables a row the chosen codec has no rung for, and says so", () => {
    const cols = optionColumns(space, draft({ codec: "h264", height: 1080 }));
    const uhd = cols.resolution.find((r) => r.value === 2160)!;
    expect(uhd.enabled).toBe(false);
    expect(uhd.title).toBe("Not available with H.264 on this device");
    expect(uhd.sub).toBe("3840×2160 · not available with H.264 at 60 fps");
    expect(cols.resolution.find((r) => r.value === 1080)?.enabled).toBe(true);
  });

  it("keeps an ineligible row visible, disabled, and carrying its reason", () => {
    const res = {
      ...RESPONSE,
      profiles: [
        profile(
          "p-4k",
          [
            rung("h264", 3840, 2160, 60, 25000, "ineligible", [
              { code: "decode_height_too_low", message: "client decode height is below the profile" },
            ]),
          ],
          "ineligible",
        ),
        profile("p-h264", [rung("h264", 1920, 1080, 60, 8000)]),
      ],
    } as unknown as ProfilesResponse;
    const cols = optionColumns(spaceOf(res), draft({ codec: "h264", height: 1080 }));
    const uhd = cols.resolution.find((r) => r.value === 2160)!;
    expect(uhd.enabled).toBe(false);
    expect(uhd.sub).toMatch(/can't decode a picture this large/);
  });

  it("lists every frame rate the catalogue has and disables the ones this codec lacks", () => {
    const res = {
      ...RESPONSE,
      profiles: [
        profile("p-av1-120", [rung("av1", 2560, 1440, 120, 20000)]),
        profile("p-h264", [rung("h264", 1920, 1080, 60, 8000)]),
      ],
    } as unknown as ProfilesResponse;
    const cols = optionColumns(spaceOf(res), draft({ codec: "h264", height: 1080, fps: 60 }));
    expect(cols.fps.map((r) => r.label)).toEqual(["60 fps", "120 fps"]);
    const at120 = cols.fps.find((r) => r.value === 120)!;
    expect(at120.enabled).toBe(false);
    expect(at120.title).toBe("Not available with H.264 on this device");
    expect(at120.sub).toBe("Not available with H.264");
    expect(cols.fps.find((r) => r.value === 60)).toMatchObject({
      enabled: true,
      selected: true,
      sub: "8 Mbps at 1080p",
    });
  });

  it("freezes the frame-rate and resolution columns for a policy-pinned app, and says why", () => {
    // #525: `profile_policy: "force"` means the server refuses any other
    // profile. The codec column stays live — a codec-only override is accepted.
    const cols = optionColumns(space, draft(), { pinned: true });
    expect(cols.fps.every((r) => !r.enabled)).toBe(true);
    expect(cols.resolution.every((r) => !r.enabled)).toBe(true);
    expect(cols.codec.filter((r) => r.value !== "pyrowave").every((r) => r.enabled)).toBe(true);
    expect(cols.pinnedNote).toBe("Fixed by this app, it always launches at its own setting.");
    expect(cols.resolution[0].title).toBe(cols.pinnedNote);
    // A frozen column says why in its own hint, not only in a tooltip.
    expect(cols.fpsHint).toBe(cols.pinnedNote);
    expect(cols.resolutionHint).toBe(cols.pinnedNote);
  });
});

describe("verdict", () => {
  const space = spaceOf();

  it("recommends the recommended row on a measured network", () => {
    expect(verdict(space, draft(), { confidence: "high" })).toEqual({
      tone: "ok",
      text: "Recommended for this device. Your measured network carries 1440p at 60 fps with room to spare.",
    });
  });

  it("says what an ordinary row costs", () => {
    expect(
      verdict(space, draft({ codec: "h264", height: 1080 }), { confidence: "high" }),
    ).toEqual({
      tone: "ok",
      text: "1080p at 60 fps needs 8 Mbps, which your measured network carries comfortably.",
    });
  });

  it("admits when the network has not been measured rather than claiming it has", () => {
    expect(
      verdict(space, draft({ codec: "h264", height: 1080 }), { confidence: "low" }),
    ).toEqual({
      tone: "ok",
      text: "1080p at 60 fps needs 8 Mbps. Your network has not been measured yet, so that is an estimate.",
    });
  });

  it("gives a risky row the server's own reason", () => {
    const v = verdict(space, draft({ codec: "hevc", height: 2160 }), { confidence: "high" });
    expect(v.tone).toBe("risky");
    expect(v.text).toMatch(/little room to spare/);
  });

  it("names the frame rate, not Auto, when nothing was chosen", () => {
    const res = {
      ...RESPONSE,
      profiles: [profile("p-1080", [rung("av1", 1920, 1080, 120, 12000)])],
    } as unknown as ProfilesResponse;
    expect(verdict(spaceOf(res), draft({ height: 2160, fps: 60 }))).toEqual({
      tone: "off",
      text: "4K is not available at 60 fps on this device. Pick a lower resolution or frame rate.",
    });
  });

  it("is off for a combination the catalogue has no rung for", () => {
    const v = verdict(space, draft({ codec: "h264", height: 2160 }), { confidence: "high" });
    expect(v).toEqual({
      tone: "off",
      text: "4K is not available with H.264 at 60 fps. Pick a lower resolution or frame rate.",
    });
  });

  it("is off with the reason when the only rung is ineligible", () => {
    const res = {
      ...RESPONSE,
      profiles: [
        profile(
          "p-4k",
          [
            rung("h264", 3840, 2160, 60, 25000, "ineligible", [
              { code: "rtt_too_high", message: "rtt above the profile ceiling" },
            ]),
          ],
          "ineligible",
        ),
      ],
    } as unknown as ProfilesResponse;
    const v = verdict(spaceOf(res), draft({ codec: "h264", height: 2160 }), { confidence: "high" });
    expect(v.tone).toBe("off");
    expect(v.text).toMatch(/takes too long to reach the host/);
  });
});

describe("heal", () => {
  const space = spaceOf();

  it("snaps the resolution to the recommended row when the new codec cannot reach it", () => {
    // auto@1440 → H.264, which only has 1080p.
    expect(heal(space, draft({ codec: "h264" }), "codec")).toEqual({
      codec: "h264",
      fps: 60,
      height: 1080,
    });
  });

  it("prefers the recommended rung when several rows would do", () => {
    const res = {
      ...RESPONSE,
      profiles: [
        profile("p-4k", [rung("av1", 3840, 2160, 60, 25000)]),
        profile("p-1080", [rung("av1", 1920, 1080, 60, 8000)]),
        profile("p-1440", [rung("av1", 2560, 1440, 60, 12000)]),
      ],
    } as unknown as ProfilesResponse;
    const s = buildOptionSpace(res.profiles, CAPS, "p-1440");
    // 720p is not in this catalogue at all, so the draft must move.
    expect(heal(s, { codec: "auto", fps: 60, height: 720 }, "codec").height).toBe(1440);
  });

  it("snaps the frame rate when the new codec has no rung at it", () => {
    const res = {
      ...RESPONSE,
      profiles: [
        profile("p-av1", [rung("av1", 2560, 1440, 120, 20000)]),
        profile("p-h264", [rung("h264", 1920, 1080, 60, 8000)]),
      ],
    } as unknown as ProfilesResponse;
    const s = buildOptionSpace(res.profiles, CAPS, "p-av1");
    expect(heal(s, { codec: "h264", fps: 120, height: 1080 }, "codec")).toEqual({
      codec: "h264",
      fps: 60,
      height: 1080,
    });
  });

  it("keeps a resolution the user just chose and moves the frame rate instead", () => {
    // 4K exists only at 60; the draft is at 120. Editing resolution means the
    // resolution is the thing that must survive.
    const res = {
      ...RESPONSE,
      profiles: [
        profile("p-4k", [rung("av1", 3840, 2160, 60, 25000)]),
        profile("p-1080", [rung("av1", 1920, 1080, 120, 12000)]),
      ],
    } as unknown as ProfilesResponse;
    const s = buildOptionSpace(res.profiles, CAPS, "p-1080");
    expect(heal(s, { codec: "av1", fps: 120, height: 2160 }, "resolution")).toEqual({
      codec: "av1",
      fps: 60,
      height: 2160,
    });
  });

  it("leaves a legal draft alone", () => {
    expect(heal(space, draft(), "codec")).toEqual(draft());
  });
});

describe("launchSpec", () => {
  it("reads back the drafted row for the head and the band's spec strip", () => {
    expect(launchSpec(spaceOf(), draft())).toEqual({
      resolution: "2560×1440",
      fps: "60 fps",
      bitrate: "12 Mbps",
      codec: "Auto · AV1",
    });
  });

  it("falls back to the app's advertised stream when nothing resolves", () => {
    expect(
      launchSpec(spaceOf(), draft({ codec: "h264", height: 2160 }), {
        width: 1920,
        height: 1080,
        fps: 60,
        bitrate_kbps: 8000,
      }),
    ).toEqual({
      resolution: "1920×1080",
      fps: "60 fps",
      bitrate: "8 Mbps",
      codec: "H.264",
    });
  });
});

describe("recommendation", () => {
  const loaded = { state: "loaded" as const, deadEnd: false, selection: null };

  it("waits rather than guessing while the evaluation is in flight", () => {
    expect(recommendation({ ...loaded, state: "loading" })).toEqual({
      tone: "ok",
      text: "Loading recommendation…",
    });
  });

  it("says the evaluation failed rather than showing a stale line", () => {
    expect(recommendation({ ...loaded, state: "failed" })).toEqual({
      tone: "off",
      text: "Could not load stream profiles.",
    });
  });

  it("is off when nothing in the response can stream here", () => {
    expect(recommendation({ ...loaded, deadEnd: true })).toEqual({
      tone: "off",
      text: "Nothing in this list can stream to this device.",
    });
  });

  it("warns on a risky selection before it names the recommendation", () => {
    expect(
      recommendation({
        ...loaded,
        codec: "auto",
        selection: { eligibility: "risky", recommended: true },
      }),
    ).toEqual({ tone: "risky", text: "This quality may not hold up on this device." });
  });

  it("claims the recommendation only for an unmoved Auto selection", () => {
    const selection = { eligibility: "eligible", recommended: true };
    expect(recommendation({ ...loaded, codec: "auto", selection }).text).toBe(
      "Recommended for this device.",
    );
    // An explicit codec is a custom setting even at the recommended rung.
    expect(recommendation({ ...loaded, codec: "h264", selection }).text).toBe("Custom setting.");
  });

  it("names what the recommendation would have been once the user has moved off it", () => {
    expect(
      recommendation({
        ...loaded,
        codec: "auto",
        selection: { eligibility: "eligible", recommended: false },
        recommended: { height: 1440, fps: 60 },
      }),
    ).toEqual({ tone: "ok", text: "Custom setting. The recommendation is 1440p at 60 fps." });
  });
});

// ── Ultra-low latency (PyroWave): the fifth Codec entry (amendment 58) ──────

const block = (over: Partial<LaunchProfilePyroWave> = {}): LaunchProfilePyroWave => ({
  mode: "prefer",
  available: true,
  reason: null,
  auto: false,
  auto_reason: null,
  ...over,
});

/** RESPONSE with the same PyroWave block on every profile. */
const withBlock = (b: LaunchProfilePyroWave | undefined): ProfilesResponse =>
  ({
    ...RESPONSE,
    profiles: RESPONSE.profiles.map((p) => ({ ...p, pyrowave: b })),
  }) as ProfilesResponse;

const pyrowaveRow = (res: ProfilesResponse, d: LaunchDraft = draft()) =>
  optionColumns(spaceOf(res), d).codec.find((r) => r.value === "pyrowave")!;

describe("Ultra-low latency (PyroWave) in the Codec column", () => {
  it("state 1: Auto picked it, so the head and the note say so and the rate is PyroWave's", () => {
    const res = withBlock(block({ auto: true }));
    const space = spaceOf(res);
    const d = draft();
    expect(launchSpec(space, d).codec).toBe("Auto · PyroWave");
    // The 1440p row's own figure: 300 Mb/s at 60 fps under 1440, as a ceiling.
    expect(launchSpec(space, d).bitrate).toBe("up to 300 Mbps");
    expect(optionColumns(space, d).codecHint).toBe(
      "Your host picks the codec, best first, and falls back to one this device can decode. " +
        "Here it lands on PyroWave. Auto chose PyroWave: you're on the local network.",
    );
    expect(optionColumns(space, d).resolution.find((r) => r.value === 1440)?.sub).toBe(
      "2560×1440 · up to 300 Mbps at 60 fps",
    );
  });

  it("state 2: Auto did not pick it, the entry stays enabled, and the note says why", () => {
    const res = withBlock(block({ auto: false, auto_reason: "link_too_slow" }));
    const cols = optionColumns(spaceOf(res), draft());
    expect(cols.codecHint).toBe(
      "Your host picks the codec, best first, and falls back to one this device can decode. " +
        "Here it lands on AV1. Not PyroWave: the link is too slow.",
    );
    expect(launchSpec(spaceOf(res), draft()).codec).toBe("Auto · AV1");
    expect(cols.codec.find((r) => r.value === "pyrowave")).toMatchObject({
      enabled: true,
      sub: "Local network only",
    });
  });

  it.each([
    ["no_measurement", "Not PyroWave: your network hasn't been measured recently."],
    ["rtt_too_high", "Not PyroWave: the connection takes too long to reach the host."],
    ["link_too_slow", "Not PyroWave: the link is too slow."],
    ["something_new", "Not PyroWave: this connection doesn't qualify."],
  ])("explains auto_reason %s in the user's words", (auto_reason, note) => {
    const hint = optionColumns(spaceOf(withBlock(block({ auto_reason }))), draft()).codecHint;
    expect(hint.endsWith(` ${note}`)).toBe(true);
  });

  it("adds no note on an allow profile, whose Auto never picks it, or on one that is not available", () => {
    const base = "Your host picks the codec, best first, and falls back to one this device can decode. Here it lands on AV1.";
    expect(optionColumns(spaceOf(withBlock(block({ mode: "allow" }))), draft()).codecHint).toBe(base);
    expect(
      optionColumns(spaceOf(withBlock(block({ available: false, reason: "device_no_webgpu" }))), draft()).codecHint,
    ).toBe(base);
  });

  it("state 3: picked, the head reads PyroWave, the note is its own, and the launch asks for it with no codec", () => {
    const space = spaceOf(withBlock(block({ mode: "allow" })));
    const d = draft({ codec: "pyrowave" });
    const cols = optionColumns(space, d);
    expect(cols.codec.find((r) => r.selected)?.value).toBe("pyrowave");
    expect(cols.codecHint).toBe("Ultra-low latency has the lowest delay of any codec, and needs a fast local network.");
    expect(launchSpec(space, d)).toMatchObject({ codec: "PyroWave", bitrate: "up to 300 Mbps" });
    expect(resolveSelection(space, d)).toMatchObject({ codec: null, wantsPyroWave: true, profileId: "p-av1" });
    // Auto and an explicit codec do not ask for it.
    expect(resolveSelection(space, draft())?.wantsPyroWave).toBe(false);
    expect(resolveSelection(space, draft({ codec: "h264", height: 1080 }))?.wantsPyroWave).toBe(false);
  });

  it("states 4 and 5: disabled, never hidden, with the reason in the sub-line and the tooltip", () => {
    expect(pyrowaveRow(withBlock(block({ available: false, reason: "device_no_webgpu" })))).toMatchObject({
      label: "Ultra-low latency (PyroWave)",
      enabled: false,
      sub: "Your browser can't decode PyroWave",
      title: "Your browser can't decode PyroWave.",
    });
    expect(pyrowaveRow(withBlock(block({ mode: "off", available: false, reason: "profile_disallows" })))).toMatchObject({
      enabled: false,
      sub: "This profile doesn't allow PyroWave",
    });
  });

  it.each([
    ["host_no_pyrowave", "No host here can stream PyroWave right now"],
    ["device_no_webtransport", "Your browser can't use WebTransport, which PyroWave needs"],
  ])("gives %s its own reason", (reason, sub) => {
    expect(pyrowaveRow(withBlock(block({ available: false, reason }))).sub).toBe(sub);
  });

  it("says its own fallback for a reason it does not know, not the generic one", () => {
    expect(pyrowaveRow(withBlock(block({ available: false, reason: "something_new" })))).toMatchObject({
      enabled: false,
      sub: "Ultra-low latency isn't available here",
      title: "Ultra-low latency isn't available here.",
    });
    expect(pyrowaveRow(withBlock(block({ available: false, reason: null })))).toMatchObject({
      sub: "Ultra-low latency isn't available here",
    });
  });

  describe("when no profile can stream the device, the disabled entry still gives a PyroWave reason", () => {
    const noneSelectable = (blocks: Record<string, LaunchProfilePyroWave>): ProfilesResponse =>
      ({
        ...RESPONSE,
        profiles: RESPONSE.profiles.map((p) => ({
          ...p,
          eligibility: "ineligible",
          reasons: BANDWIDTH,
          rungs: p.rungs.map((r) => ({ ...r, eligibility: "ineligible", reasons: BANDWIDTH })),
          pyrowave: blocks[p.id],
        })),
      }) as unknown as ProfilesResponse;

    it("takes the reason from a blocked profile, never a row's bandwidth sentence", () => {
      const gone = block({ available: false, reason: "device_no_webgpu" });
      const row = pyrowaveRow(noneSelectable({ "p-av1": block({ mode: "allow" }), "p-h264": gone, "p-hevc": gone }));
      expect(row).toMatchObject({ enabled: false, sub: "Your browser can't decode PyroWave" });
      expect(row.title).not.toMatch(/connection|slower/);
    });

    it("falls back to its own line when no block is the cause", () => {
      const ok = block({ mode: "allow" });
      const row = pyrowaveRow(noneSelectable({ "p-av1": ok, "p-h264": ok, "p-hevc": ok }));
      expect(row).toMatchObject({ enabled: false, sub: "Ultra-low latency isn't available here" });
    });
  });

  it("reads a server that sends no pyrowave block as a profile that does not allow it", () => {
    expect(pyrowaveRow(withBlock(undefined))).toMatchObject({
      enabled: false,
      sub: "This profile doesn't allow PyroWave",
    });
  });

  it("is enabled when any profile offers it, and the rows of the others are disabled with their reason", () => {
    const res = {
      ...RESPONSE,
      profiles: RESPONSE.profiles.map((p) => ({
        ...p,
        pyrowave: p.id === "p-av1" ? block({ mode: "allow" }) : block({ mode: "off", available: false, reason: "profile_disallows" }),
      })),
    } as ProfilesResponse;
    const cols = optionColumns(spaceOf(res), draft({ codec: "pyrowave" }));
    expect(cols.codec.find((r) => r.value === "pyrowave")?.enabled).toBe(true);
    const hevc = cols.resolution.find((r) => r.value === 2160)!;
    expect(hevc).toMatchObject({ enabled: false, sub: "This profile doesn't allow PyroWave." });
    expect(cols.resolution.find((r) => r.value === 1440)?.enabled).toBe(true);
  });

  it("answers a pinned app from the pinned profile alone", () => {
    const res = {
      ...RESPONSE,
      profiles: RESPONSE.profiles.map((p) => ({
        ...p,
        pyrowave: p.id === "p-av1" ? block({ mode: "off", available: false, reason: "profile_disallows" }) : block({ mode: "allow" }),
      })),
    } as ProfilesResponse;
    const app = { profile_policy: "force", default_profile_id: "p-av1" } as never;
    const pinned = optionsFor({ app, data: res, caps: CAPS, isAdmin: false });
    expect(pinned.pinnedProfileId).toBe("p-av1");
    expect(optionColumns(pinned.space, pinned.seed, { pinned: true }).codec.find((r) => r.value === "pyrowave")).toMatchObject({
      enabled: false,
      sub: "This profile doesn't allow PyroWave",
    });
    const free = optionsFor({ app: { profile_policy: "allow" } as never, data: res, caps: CAPS, isAdmin: false });
    expect(optionColumns(free.space, free.seed).codec.find((r) => r.value === "pyrowave")?.enabled).toBe(true);
  });

  it("heals to a row PyroWave can run when it is picked", () => {
    const res = {
      ...RESPONSE,
      profiles: RESPONSE.profiles.map((p) => ({
        ...p,
        pyrowave: p.id === "p-h264" ? block({ mode: "allow" }) : block({ mode: "off", available: false, reason: "profile_disallows" }),
      })),
    } as ProfilesResponse;
    expect(heal(spaceOf(res), draft({ codec: "pyrowave" }), "codec")).toEqual({ codec: "pyrowave", fps: 60, height: 1080 });
  });
});

describe("the footer line for a PyroWave pick", () => {
  // The 1440p row is the recommended, eligible one: the standard codecs' sentence would claim
  // "Your measured network carries 1440p ... with room to spare" for PyroWave too.
  const line = (b: LaunchProfilePyroWave) => verdict(spaceOf(withBlock(b)), draft({ codec: "pyrowave" }));

  it.each([
    ["link_too_slow", "the link is too slow"],
    ["rtt_too_high", "the connection takes too long to reach the host"],
  ])("says what the server said when auto_reason is %s", (auto_reason, why) => {
    expect(line(block({ auto_reason }))).toEqual({
      tone: "risky",
      text: `Ultra-low latency may not hold up: ${why}. It needs up to 300 Mbps on your local network.`,
    });
  });

  it.each([null, "no_measurement", "something_new"])("is neutral otherwise (auto_reason %s)", (auto_reason) => {
    const v = line(block({ mode: "allow", auto_reason }));
    expect(v).toEqual({ tone: "ok", text: "Needs up to 300 Mbps on your local network." });
    expect(v.text).not.toMatch(/measured network|comfortably|room to spare/);
  });

  it("leaves the standard codecs' sentence alone", () => {
    expect(verdict(spaceOf(withBlock(block({ auto_reason: "link_too_slow" }))), draft()).text).toMatch(/room to spare/);
  });
});
