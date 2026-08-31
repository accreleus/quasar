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
import type { ProfilesResponse } from "../../../api/types";
import { buildOptionSpace, type LaunchDraft, type OptionSpace } from "../launchOptions";
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
    expect(cols.codec.map((r) => r.label)).toEqual(["Auto", "H.264", "HEVC", "AV1"]);
    expect(cols.codec.every((r) => r.enabled)).toBe(true);
    expect(cols.codec.find((r) => r.selected)?.value).toBe("auto");
  });

  it("drops a codec this device cannot decode — it is never a row to explain", () => {
    const noAv1 = buildOptionSpace(RESPONSE.profiles, { h264: true, hevc: true, av1: false, vp9: false }, "p-av1");
    const cols = optionColumns(noAv1, draft({ codec: "h264", height: 1080 }));
    expect(cols.codec.map((r) => r.label)).toEqual(["Auto", "H.264", "HEVC"]);
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
    expect(cols.codec.every((r) => r.enabled)).toBe(true);
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
