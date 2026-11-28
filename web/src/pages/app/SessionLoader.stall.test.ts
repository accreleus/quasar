import { describe, expect, it } from "vitest";
import { PHASE_STALL_MS, PHASE_STALL_COPY } from "./SessionLoader";
import { statusPhase } from "./launchStall";

// The status string no longer writes any visible copy — the rail and headline
// come from the signals (loaderPhases.test.ts). All it still decides is which
// stall budget the launch is being held to.
describe("statusPhase — the stall budget the status string names", () => {
  // §3.2: the two stalls (host allocation, then game boot) plus the transport
  // leg must be separately nameable — a stall that cannot say WHICH stage it is
  // in is exactly the unattributable spinner this replaces.
  it.each([
    ["scheduling host", "host"],
    ["preparing resources and image", "game"],
    ["container started; first frame ready", "game"],
    ["media pipeline ready", "stream"],
    ["answer sent — awaiting ICE", "stream"],
    ["ws open — waiting for offer", "stream"],
    ["connected", "stream"],
  ])("attributes %s to the %s phase", (status, phase) => {
    expect(statusPhase(status)).toBe(phase);
  });

  // The old fallthrough was commented "Default / initial / error states" and
  // claimed "Establishing connection" forever. It is now an honest initial
  // state that the host-phase stall budget covers.
  it("treats an unrecognised status as the initial host phase, not an error", () => {
    expect(statusPhase("connecting…")).toBe("host");
    expect(statusPhase("")).toBe("host");
  });

  it("gives the game-boot phase the longest budget (image pull)", () => {
    expect(PHASE_STALL_MS.game).toBeGreaterThan(PHASE_STALL_MS.host);
    expect(PHASE_STALL_MS.game).toBeGreaterThan(PHASE_STALL_MS.stream);
  });

  // #484 §3.2: the container is up (channelOpen may already be true) but the
  // app itself hasn't drawn a frame yet — a fourth, distinct stall phase from
  // the three above, keyed to the agent's exact "app booting" detail string.
  describe("#484: the app-booting stage", () => {
    it("maps the exact wire detail string to the app phase", () => {
      expect(statusPhase("app booting")).toBe("app");
    });

    it("wins over other status text also present (e.g. a stale WebRTC status)", () => {
      expect(statusPhase("connected ✓ app booting")).toBe("app");
    });

    it("gets its own stall budget, matching the game-boot budget", () => {
      expect(PHASE_STALL_MS.app).toBe(PHASE_STALL_MS.game);
    });

    it("has its own stall copy that does not claim the stream is still opening", () => {
      expect(PHASE_STALL_COPY.app.title).toMatch(/starting the game/i);
      expect(PHASE_STALL_COPY.app.message).not.toMatch(/opening the stream/i);
    });
  });
});
