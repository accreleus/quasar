import { describe, expect, it } from "vitest";
import { fallbackReason } from "./fallback";

describe("fallbackReason", () => {
  it.each([
    "connect_timeout",
    "connect_failed",
    "auth_failed",
    "unsupported_version",
    "takeover_rate_limited",
    "decoder_unsupported",
    "decode_error",
    "stream_lost",
    "internal",
  ])("reports %s as it is", (code) => expect(fallbackReason(code)).toBe(code));

  it("does not fall back when the session ended or another device displaced this one", () => {
    for (const code of ["session_ended", "taken_over", "closed"]) expect(fallbackReason(code)).toBeNull();
  });

  it("maps the presenter's failures and unknown codes into the contract's vocabulary", () => {
    expect(fallbackReason("no_webgpu")).toBe("decoder_unsupported");
    expect(fallbackReason("gpu_failed")).toBe("internal");
    expect(fallbackReason("bad_request")).toBe("internal");
  });
});
