/**
 * AS10-06 — unit tests for the stream-health banner mapper (pure, no React).
 */

import { describe, expect, it } from "vitest";
import { healthBanner, signalQuality, signalLabel, qualityLabelFor } from "./streamHealth";
import type { SignalInputs } from "./streamHealth";
import type { Session } from "../../api/types";

type HealthInput = Pick<
  Session,
  "state" | "state_detail" | "health_state" | "health_reason"
>;

function base(overrides: Partial<HealthInput>): HealthInput {
  return {
    state: "running",
    state_detail: null,
    health_state: "healthy",
    health_reason: undefined,
    ...overrides,
  };
}

describe("healthBanner", () => {
  it("returns null when healthy", () => {
    expect(healthBanner(base({ health_state: "healthy" }))).toBeNull();
  });

  it("returns null when health_state is absent", () => {
    expect(healthBanner(base({ health_state: undefined }))).toBeNull();
  });

  it("shows a non-blocking warning for network_degrading", () => {
    const b = healthBanner(base({ health_state: "network_degrading" }));
    expect(b).not.toBeNull();
    expect(b!.kind).toBe("warning");
    expect(b!.actionable).toBe(false);
  });

  it("shows a non-blocking warning for abr_at_floor", () => {
    const b = healthBanner(base({ health_state: "abr_at_floor" }));
    expect(b!.kind).toBe("warning");
    expect(b!.actionable).toBe(false);
  });

  it("shows an actionable critical banner for unsustainable", () => {
    const b = healthBanner(base({ health_state: "unsustainable" }));
    expect(b!.kind).toBe("critical");
    expect(b!.actionable).toBe(true);
    expect(b!.title).toMatch(/unsustainable/i);
  });

  it("shows the critical banner when a session failed with an unsustainable detail", () => {
    const b = healthBanner(
      base({
        state: "failed",
        state_detail: "unsustainable: below ABR floor for over 3m",
        // health_state may have been cleared/omitted on a failed row.
        health_state: undefined,
      }),
    );
    expect(b!.kind).toBe("critical");
    expect(b!.actionable).toBe(true);
  });

  it("prefers the server-supplied reason as the message when present", () => {
    const reason = "ABR pinned at floor for 92s";
    const b = healthBanner(
      base({ health_state: "abr_at_floor", health_reason: reason }),
    );
    expect(b!.message).toBe(reason);
  });

  it("ignores host_lost (handled by the host-offline path, not here)", () => {
    const b = healthBanner(
      base({ state: "failed", state_detail: "host_lost", health_state: undefined }),
    );
    expect(b).toBeNull();
  });

  it("AS10-11: surfaces client_* states as a distinct device-advice warning", () => {
    const decode = healthBanner(base({ health_state: "client_decode_degrading" }));
    expect(decode?.kind).toBe("warning");
    expect(decode?.actionable).toBe(false);
    expect(decode?.title).toMatch(/decode/i);

    const present = healthBanner(base({ health_state: "client_presentation_degrading" }));
    expect(present?.kind).toBe("warning");
    expect(present?.title).toMatch(/frame rate/i);
  });

  it("AS10-11: client health_reason overrides the default advice message", () => {
    const b = healthBanner(
      base({ health_state: "client_decode_degrading", health_reason: "client decode_degrading" }),
    );
    expect(b?.message).toBe("client decode_degrading");
  });

  // #484 §3.2: suppression table — every non-critical banner kind is null
  // while appPresented is false, and unaffected once it's true (or omitted,
  // the default). The compositor's black boot-window scene must not be able
  // to trigger a network/device warning about a game that isn't on screen.
  describe("#484: suppressed while the app hasn't presented", () => {
    it.each([
      "network_degrading",
      "abr_at_floor",
      "client_decode_degrading",
      "client_presentation_degrading",
    ] as const)("suppresses %s while appPresented is false", (health_state) => {
      expect(healthBanner(base({ health_state }), false)).toBeNull();
    });

    it.each([
      "network_degrading",
      "abr_at_floor",
      "client_decode_degrading",
      "client_presentation_degrading",
    ] as const)("still shows %s once appPresented is true", (health_state) => {
      expect(healthBanner(base({ health_state }), true)).not.toBeNull();
    });

    it("does NOT suppress the critical unsustainable banner while booting", () => {
      const b = healthBanner(base({ health_state: "unsustainable" }), false);
      expect(b).not.toBeNull();
      expect(b!.kind).toBe("critical");
    });

    it("does NOT suppress a failed/unsustainable session while booting", () => {
      const b = healthBanner(
        base({
          state: "failed",
          state_detail: "unsustainable: below ABR floor for over 3m",
          health_state: undefined,
        }),
        false,
      );
      expect(b).not.toBeNull();
      expect(b!.kind).toBe("critical");
    });

    it("defaults appPresented to true so existing call sites are unaffected", () => {
      const b = healthBanner(base({ health_state: "network_degrading" }));
      expect(b).not.toBeNull();
    });
  });
});

describe("signalQuality (UI-09 connection glyph)", () => {
  function sig(overrides: Partial<SignalInputs>): SignalInputs {
    return { presentSdMs: 2, fps: 60, packetsLost: 0, freezeCount: 0, ...overrides };
  }

  it("is excellent when smooth, clean, and frames flowing", () => {
    expect(signalQuality(sig({ presentSdMs: 2, fps: 60 }))).toBe("excellent");
  });

  it("is good (not excellent) before a present-σ reading exists", () => {
    // null σ → no smoothness evidence yet; the healthy default is "good".
    expect(signalQuality(sig({ presentSdMs: null }))).toBe("good");
  });

  it("is good (not excellent) before frames flow (fps 0)", () => {
    expect(signalQuality(sig({ presentSdMs: 2, fps: 0 }))).toBe("good");
  });

  it("drops to fair on moderate present σ", () => {
    expect(signalQuality(sig({ presentSdMs: 14 }))).toBe("fair");
  });

  it("drops to poor on high present σ", () => {
    expect(signalQuality(sig({ presentSdMs: 20 }))).toBe("poor");
  });

  it("is poor on any freeze this window", () => {
    expect(signalQuality(sig({ presentSdMs: 2, freezeCount: 1 }))).toBe("poor");
  });

  it("is poor on heavy packet loss", () => {
    expect(signalQuality(sig({ presentSdMs: 2, packetsLost: 20 }))).toBe("poor");
  });

  it("is fair on light packet loss even when smooth", () => {
    expect(signalQuality(sig({ presentSdMs: 2, packetsLost: 5 }))).toBe("fair");
  });

  it("freeze/loss outranks a good σ", () => {
    // A freeze present alongside a clean σ still reads poor.
    expect(signalQuality(sig({ presentSdMs: 1, freezeCount: 2 }))).toBe("poor");
  });

  describe("dead-stream detection (fps 0 after frames have flowed)", () => {
    it("never-delivered + fps 0 stays good (warm-up, no false alarm)", () => {
      expect(
        signalQuality(sig({ presentSdMs: null, fps: 0, hasDeliveredFrames: false })),
      ).toBe("good");
      // Explicitly omitted also defaults to "never delivered".
      expect(signalQuality(sig({ presentSdMs: null, fps: 0 }))).toBe("good");
    });

    it("delivered then fps 0 reads poor (media path died)", () => {
      expect(
        signalQuality(sig({ presentSdMs: 2, fps: 0, hasDeliveredFrames: true })),
      ).toBe("poor");
    });

    it("delivered then fps 0 is poor even with an otherwise-clean σ and no loss", () => {
      expect(
        signalQuality(
          sig({ presentSdMs: 1, fps: 0, packetsLost: 0, freezeCount: 0, hasDeliveredFrames: true }),
        ),
      ).toBe("poor");
    });

    it("does not affect a flowing stream (fps > 0) regardless of hasDeliveredFrames", () => {
      expect(signalQuality(sig({ presentSdMs: 2, fps: 60, hasDeliveredFrames: true }))).toBe(
        "excellent",
      );
      expect(signalQuality(sig({ presentSdMs: 2, fps: 60, hasDeliveredFrames: false }))).toBe(
        "excellent",
      );
    });
  });

  // #484 §3.2: while the app hasn't presented, the compositor's own black
  // scene decodes perfectly — the glyph must never claim "excellent" over it.
  describe("appPresented cap (#484)", () => {
    it("caps an otherwise-excellent reading at good while appPresented is false", () => {
      expect(
        signalQuality({ presentSdMs: 2, fps: 60, packetsLost: 0, freezeCount: 0, appPresented: false }),
      ).toBe("good");
    });

    it("does not upgrade a poor/fair reading while appPresented is false", () => {
      expect(
        signalQuality(sig({ presentSdMs: 20, appPresented: false })),
      ).toBe("poor");
      expect(
        signalQuality(sig({ presentSdMs: 14, appPresented: false })),
      ).toBe("fair");
    });

    it("reads excellent again once appPresented is true", () => {
      expect(signalQuality(sig({ presentSdMs: 2, fps: 60, appPresented: true }))).toBe(
        "excellent",
      );
    });

    it("defaults appPresented to true so existing call sites are unaffected", () => {
      expect(signalQuality(sig({ presentSdMs: 2, fps: 60 }))).toBe("excellent");
    });
  });
});

describe("signalLabel", () => {
  it("capitalizes the quality string", () => {
    expect(signalLabel("excellent")).toBe("Excellent");
    expect(signalLabel("poor")).toBe("Poor");
  });
});

// UX assessment §2.2: the strip reported quality "Good" for 118 s on a session
// that had never connected. `hasDeliveredFrames` covers "was flowing, has now
// stopped"; this covers the hole underneath it — never flowing in the first
// place, with every counter at zero and no media path to measure.
describe("no media path", () => {
  const dead = {
    presentSdMs: null,
    fps: 0,
    packetsLost: 0,
    freezeCount: 0,
    hasDeliveredFrames: false,
    mediaPathUp: false,
  };

  it("never reports a healthy default when the channel is closed", () => {
    expect(signalQuality(dead)).toBe("poor");
  });

  it("outranks every other rule, including an otherwise-perfect sample", () => {
    expect(
      signalQuality({
        presentSdMs: 1,
        fps: 60,
        packetsLost: 0,
        freezeCount: 0,
        hasDeliveredFrames: true,
        mediaPathUp: false,
      }),
    ).toBe("poor");
  });

  it("defaults to connected so existing call sites are unaffected", () => {
    const { mediaPathUp: _omitted, ...withoutFlag } = dead;
    expect(signalQuality(withoutFlag)).toBe("good");
  });

  it("labels a missing media path rather than claiming a quality", () => {
    expect(qualityLabelFor("poor", false)).toBe("No signal");
    expect(qualityLabelFor("good", false)).toBe("No signal");
    expect(qualityLabelFor("good", true)).toBe("Good");
  });
});
