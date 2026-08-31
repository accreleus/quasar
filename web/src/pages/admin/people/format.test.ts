import { describe, expect, it } from "vitest";
import { ACTIVE_STATES, avatarGradient, fmtDuration, friendlyError, sessionStateDot } from "./format";
import { ApiError } from "../../../api/client";

describe("fmtDuration", () => {
  it("returns empty string when startedAt is null", () => {
    expect(fmtDuration(null, null)).toBe("");
    expect(fmtDuration(undefined, null)).toBe("");
  });

  it("formats a duration under an hour in minutes", () => {
    const start = new Date("2024-01-01T10:00:00Z").toISOString();
    const end = new Date("2024-01-01T10:30:00Z").toISOString();
    expect(fmtDuration(start, end)).toBe("30m");
  });

  it("formats a duration of exactly one hour", () => {
    const start = new Date("2024-01-01T10:00:00Z").toISOString();
    const end = new Date("2024-01-01T11:00:00Z").toISOString();
    expect(fmtDuration(start, end)).toBe("1h 0m");
  });

  it("formats a duration over an hour in h/m notation", () => {
    const start = new Date("2024-01-01T10:00:00Z").toISOString();
    const end = new Date("2024-01-01T11:45:00Z").toISOString();
    expect(fmtDuration(start, end)).toBe("1h 45m");
  });
});

describe("avatarGradient", () => {
  it("returns a CSS gradient string", () => {
    const result = avatarGradient("alice");
    expect(result).toMatch(/^linear-gradient\(150deg,#[0-9A-Fa-f]{6},#[0-9A-Fa-f]{6}\)$/);
  });

  it("returns the same gradient for the same username (deterministic)", () => {
    expect(avatarGradient("bob")).toBe(avatarGradient("bob"));
  });

  it("returns different gradients for different usernames", () => {
    // Not guaranteed by design but true for these inputs given the hash
    // (tests the hash distributes, not that all names differ)
    const results = new Set(["alice", "bob", "carol", "dave", "eve"].map(avatarGradient));
    expect(results.size).toBeGreaterThan(1);
  });
});

describe("sessionStateDot", () => {
  it("maps running to success color", () => {
    expect(sessionStateDot("running")).toBe("var(--success)");
  });

  it("maps active states to warning color", () => {
    for (const state of ACTIVE_STATES) {
      if (state === "running") continue; // running is caught first
      expect(sessionStateDot(state)).toBe("var(--warning)");
    }
  });

  it("maps failed to danger color", () => {
    expect(sessionStateDot("failed")).toBe("var(--danger)");
  });

  it("maps unknown states to muted text color", () => {
    expect(sessionStateDot("ended")).toBe("var(--text-4)");
    expect(sessionStateDot("unknown")).toBe("var(--text-4)");
  });
});

describe("friendlyError", () => {
  it("returns generic message for non-ApiError", () => {
    expect(friendlyError(new Error("boom"))).toBe("Unexpected error");
    expect(friendlyError("string error")).toBe("Unexpected error");
  });

  it("detects last-admin refusal", () => {
    const err = new ApiError(409, "conflict", "last_admin constraint violated");
    expect(friendlyError(err)).toBe("Cannot demote or delete the last admin account.");
  });

  it("detects self-action refusal", () => {
    const err = new ApiError(400, "forbidden", "cannot perform action on yourself");
    expect(friendlyError(err)).toBe("You cannot perform this action on your own account.");
  });

  it("detects active session refusal", () => {
    const err = new ApiError(409, "conflict", "user has active_session");
    expect(friendlyError(err)).toBe("User has active sessions — stop them first.");
  });

  it("falls back to the raw ApiError message", () => {
    const err = new ApiError(400, "bad_request", "some other server error");
    expect(friendlyError(err)).toBe("some other server error");
  });
});
