import { describe, it, expect } from "vitest";
import { classifyClientHealth, type ClientHealthInputs } from "./clientHealth";

// A baseline "smooth" snapshot: 60fps profile (16.7ms budget), low decode, tight pacing.
function base(over: Partial<ClientHealthInputs> = {}): ClientHealthInputs {
  return {
    decodeMs: 4,
    presentSdMs: 8,
    presentP95Ms: 20,
    freezeCount: 0,
    isHidden: false,
    profileFrameBudgetMs: 1000 / 60,
    decodeFailed: false,
    ...over,
  };
}

describe("classifyClientHealth", () => {
  it("smooth on a clean snapshot", () => {
    expect(classifyClientHealth(base()).health).toBe("smooth");
  });

  // --- hidden-wins precedence (the #1 false-positive guard) ------------------

  it("hidden tab → backgrounded_or_hidden even with zero present fps", () => {
    const r = classifyClientHealth(base({ isHidden: true, freezeCount: 10 }));
    expect(r.health).toBe("backgrounded_or_hidden");
  });

  it("hidden wins over decode saturation", () => {
    const r = classifyClientHealth(base({ isHidden: true, decodeMs: 999 }));
    expect(r.health).toBe("backgrounded_or_hidden");
  });

  it("hidden wins over presentation degradation (high σ)", () => {
    const r = classifyClientHealth(base({ isHidden: true, presentSdMs: 100, presentP95Ms: 200 }));
    expect(r.health).toBe("backgrounded_or_hidden");
  });

  it("hidden wins over a hard decode failure", () => {
    const r = classifyClientHealth(base({ isHidden: true, decodeFailed: true }));
    expect(r.health).toBe("backgrounded_or_hidden");
  });

  // --- client_unsupported (hard decode failure) ------------------------------

  it("decodeFailed → client_unsupported when visible", () => {
    expect(classifyClientHealth(base({ decodeFailed: true })).health).toBe("client_unsupported");
  });

  it("client_unsupported takes precedence over decode saturation", () => {
    const r = classifyClientHealth(base({ decodeFailed: true, decodeMs: 999 }));
    expect(r.health).toBe("client_unsupported");
  });

  // --- decode_degrading ------------------------------------------------------

  it("decode over the frame budget → decode_degrading", () => {
    // 60fps budget is 16.7ms; 0.85× = ~14.2ms. 18ms is over.
    expect(classifyClientHealth(base({ decodeMs: 18 })).health).toBe("decode_degrading");
  });

  it("decode just under the budget fraction stays smooth", () => {
    // 0.85 * 16.7 = ~14.2ms; 13ms is under.
    expect(classifyClientHealth(base({ decodeMs: 13 })).health).toBe("smooth");
  });

  it("unknown profile budget falls back to absolute decode ceiling", () => {
    // budget 0 → 25ms ceiling. 26ms is over, 20ms is under.
    expect(
      classifyClientHealth(base({ profileFrameBudgetMs: 0, decodeMs: 26 })).health,
    ).toBe("decode_degrading");
    expect(
      classifyClientHealth(base({ profileFrameBudgetMs: 0, decodeMs: 20 })).health,
    ).toBe("smooth");
  });

  it("null decodeMs does not trigger decode_degrading", () => {
    expect(classifyClientHealth(base({ decodeMs: null })).health).toBe("smooth");
  });

  it("decode_degrading wins over presentation degradation", () => {
    const r = classifyClientHealth(base({ decodeMs: 18, presentSdMs: 100 }));
    expect(r.health).toBe("decode_degrading");
  });

  // --- presentation_degrading ------------------------------------------------

  it("sustained freezes → presentation_degrading", () => {
    expect(classifyClientHealth(base({ freezeCount: 2 })).health).toBe("presentation_degrading");
  });

  it("a single freeze stays smooth (transient)", () => {
    expect(classifyClientHealth(base({ freezeCount: 1 })).health).toBe("smooth");
  });

  it("present σ well over the AS-05 line → presentation_degrading", () => {
    expect(classifyClientHealth(base({ presentSdMs: 30 })).health).toBe(
      "presentation_degrading",
    );
  });

  it("present σ at the AS-05 degraded line (18ms) is NOT yet a health flip", () => {
    // 18ms is the controller's re-inflate line, but below our 28ms health threshold.
    expect(classifyClientHealth(base({ presentSdMs: 18 })).health).toBe("smooth");
  });

  it("present p95 long tail → presentation_degrading", () => {
    expect(classifyClientHealth(base({ presentP95Ms: 50 })).health).toBe(
      "presentation_degrading",
    );
  });

  it("null present metrics do not trigger presentation_degrading", () => {
    expect(
      classifyClientHealth(base({ presentSdMs: null, presentP95Ms: null })).health,
    ).toBe("smooth");
  });

  // --- reason strings present ------------------------------------------------

  it("degraded classes carry a non-empty reason; smooth is empty", () => {
    expect(classifyClientHealth(base()).reason).toBe("");
    expect(classifyClientHealth(base({ decodeMs: 18 })).reason).not.toBe("");
    expect(classifyClientHealth(base({ presentSdMs: 30 })).reason).not.toBe("");
    expect(classifyClientHealth(base({ isHidden: true })).reason).not.toBe("");
  });
});
