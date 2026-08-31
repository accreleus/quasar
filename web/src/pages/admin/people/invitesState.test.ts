import { describe, expect, it } from "vitest";
import { inviteState, inviteStateChipVariant, inviteStateLabel } from "./invitesState";

const NOW = new Date("2026-08-29T00:00:00Z").getTime();

function base() {
  return { revoked_at: null, used_count: 0, max_uses: 1, expires_at: null };
}

describe("inviteState", () => {
  it("is pending with no revocation, uses left and no expiry (or a future one)", () => {
    expect(inviteState(base(), NOW)).toBe("pending");
    expect(
      inviteState({ ...base(), expires_at: "2026-09-05T00:00:00Z" }, NOW),
    ).toBe("pending");
  });

  it("is expired once expires_at is in the past", () => {
    expect(
      inviteState({ ...base(), expires_at: "2026-08-01T00:00:00Z" }, NOW),
    ).toBe("expired");
  });

  it("is redeemed once used_count reaches max_uses, even past expiry", () => {
    expect(
      inviteState({ ...base(), used_count: 1, max_uses: 1 }, NOW),
    ).toBe("redeemed");
    expect(
      inviteState(
        { ...base(), used_count: 2, max_uses: 2, expires_at: "2026-01-01T00:00:00Z" },
        NOW,
      ),
    ).toBe("redeemed");
  });

  it("is revoked once revoked_at is set, even if also used up or expired", () => {
    expect(inviteState({ ...base(), revoked_at: "2026-08-20T00:00:00Z" }, NOW)).toBe(
      "revoked",
    );
    expect(
      inviteState(
        {
          revoked_at: "2026-08-20T00:00:00Z",
          used_count: 1,
          max_uses: 1,
          expires_at: "2026-01-01T00:00:00Z",
        },
        NOW,
      ),
    ).toBe("revoked");
  });
});

describe("inviteStateLabel / inviteStateChipVariant", () => {
  it("capitalises the label and maps to the STATE_CHIP colour scheme", () => {
    expect(inviteStateLabel("pending")).toBe("Pending");
    expect(inviteStateChipVariant("pending")).toBe("warning");
    expect(inviteStateLabel("redeemed")).toBe("Redeemed");
    expect(inviteStateChipVariant("redeemed")).toBe("info");
    expect(inviteStateLabel("expired")).toBe("Expired");
    expect(inviteStateChipVariant("expired")).toBe("neutral");
    expect(inviteStateLabel("revoked")).toBe("Revoked");
    expect(inviteStateChipVariant("revoked")).toBe("neutral");
  });
});
