import { describe, expect, it } from "vitest";

import {
  codecDisplayName,
  codecOutcome,
  codecOutcomeSummary,
  compareCodecs,
  normaliseCodec,
  rungRejectionLabel,
} from "./codecDisplay";

// These helpers are shared by the in-session HUD, the DiagPanel and the admin
// session drill-down. That sharing is the point: two copies of the comparison
// would eventually disagree about whether a session's codecs disagree, and a
// user and an operator contradicting each other is worse than neither being
// told. The tests below therefore pin the SHARED contract, not one caller's
// rendering.

describe("normaliseCodec", () => {
  it("folds both vocabularies and both spellings onto one wire id", () => {
    // Server side speaks h264|h265|av1; the browser reports a getStats mimeType.
    expect(normaliseCodec("video/H264")).toBe("h264");
    expect(normaliseCodec("h264")).toBe("h264");
    expect(normaliseCodec("avc1")).toBe("h264");
    expect(normaliseCodec("video/H265")).toBe("h265");
    expect(normaliseCodec("hevc")).toBe("h265");
    expect(normaliseCodec("hvc1")).toBe("h265");
    expect(normaliseCodec("video/AV1")).toBe("av1");
    expect(normaliseCodec("av01")).toBe("av1");
  });

  it("keeps an unrecognised codec rather than dropping it", () => {
    // A codec the server never resolves is the LOUDEST disagreement there is;
    // normalising it to null would hide exactly the case worth surfacing.
    expect(normaliseCodec("video/VP9")).toBe("vp9");
  });

  it("treats absent as unknown, not as a value", () => {
    expect(normaliseCodec(null)).toBeNull();
    expect(normaliseCodec(undefined)).toBeNull();
    expect(normaliseCodec("")).toBeNull();
  });
});

describe("compareCodecs", () => {
  it("agrees across vocabularies", () => {
    expect(compareCodecs("h265", "video/H265").agrees).toBe(true);
  });

  it("flags a real disagreement", () => {
    const pair = compareCodecs("h265", "video/H264");
    expect(pair.agrees).toBe(false);
    expect(pair.resolved).toBe("h265");
    expect(pair.negotiated).toBe("h264");
  });

  it("does NOT flag when either side is unknown", () => {
    // Before the client reports a codec every session would otherwise light up
    // as a mismatch — crying wolf on the normal case.
    expect(compareCodecs("h264", null).agrees).toBeNull();
    expect(compareCodecs(null, "video/H264").agrees).toBeNull();
    expect(compareCodecs(null, null).agrees).toBeNull();
  });
});

describe("codecDisplayName", () => {
  it("uses operator-facing names and passes unknowns through", () => {
    expect(codecDisplayName("h264")).toBe("H.264");
    expect(codecDisplayName("h265")).toBe("HEVC");
    expect(codecDisplayName("av1")).toBe("AV1");
    expect(codecDisplayName("vp9")).toBe("VP9");
    expect(codecDisplayName(null)).toBeNull();
  });
});

describe("rungRejectionLabel", () => {
  it("turns each clamp into something an operator can act on", () => {
    expect(rungRejectionLabel("host_encoder")).toMatch(/host cannot encode/i);
    expect(rungRejectionLabel("client_decode")).toMatch(/cannot decode/i);
    expect(rungRejectionLabel("decode_height")).toMatch(/resolution ceiling/i);
    expect(rungRejectionLabel("decode_history")).toMatch(/previously failed/i);
    expect(rungRejectionLabel("hardware_encoder")).toMatch(/no hardware encoder/i);
    expect(rungRejectionLabel("unknown_codec")).toMatch(/does not recognise/i);
  });

  it("passes an unrecognised reason through — the enum is OPEN on the wire", () => {
    expect(rungRejectionLabel("some_future_clamp")).toBe("some_future_clamp");
  });

  it("returns null for no rejection", () => {
    expect(rungRejectionLabel(null)).toBeNull();
    expect(rungRejectionLabel(undefined)).toBeNull();
  });
});

describe("codecOutcome", () => {
  // The three outcomes produce an IDENTICAL stream.codec, so this classification
  // is the only thing separating them. Collapsing any two would put an operator
  // back where they started.
  it("classifies a clean win as merit", () => {
    expect(codecOutcome({ override: null, floor: false })).toBe("merit");
  });

  it("classifies the h264 floor as floor, not merit", () => {
    expect(codecOutcome({ override: null, floor: true })).toBe("floor");
  });

  it("classifies an operator override as override, not merit", () => {
    expect(codecOutcome({ override: "h265", floor: false })).toBe("override");
  });

  it("an override wins over floor — clamp 0 pre-empts the walk entirely", () => {
    expect(codecOutcome({ override: "av1", floor: true })).toBe("override");
  });

  it("returns null with no decision recorded", () => {
    expect(codecOutcome(null)).toBeNull();
    expect(codecOutcome(undefined)).toBeNull();
  });
});

describe("codecOutcomeSummary", () => {
  it("says an override was FORCED and that clamps were skipped, not passed", () => {
    const s = codecOutcomeSummary({ override: "h265", floor: false }) ?? "";
    expect(s).toMatch(/override/i);
    expect(s).toMatch(/HEVC/);
    expect(s).toMatch(/skipped, not passed/i);
    // It must not read as having earned the codec.
    expect(s).not.toMatch(/merit/i);
  });

  it("says the floor bypassed every clamp INCLUDING the one that rejected it", () => {
    const s = codecOutcomeSummary({ override: null, floor: true }) ?? "";
    expect(s).toMatch(/no rung survived/i);
    expect(s).toMatch(/bypassing every clamp/i);
    expect(s).toMatch(/rejected it/i);
  });

  it("says a merit win survived every clamp", () => {
    const s = codecOutcomeSummary({ override: null, floor: false }) ?? "";
    expect(s).toMatch(/merit/i);
    expect(s).toMatch(/survived every clamp/i);
  });
});
