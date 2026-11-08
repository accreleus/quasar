import { describe, expect, it } from "vitest";
import { fallbackReason, rememberedFallback, rememberFallback } from "./fallback";

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

describe("rememberFallback", () => {
  it("keeps the reason per session id for this tab", () => {
    sessionStorage.clear();
    expect(rememberedFallback("s1")).toBeNull();
    rememberFallback("s1", "connect_timeout");
    expect(rememberedFallback("s1")).toBe("connect_timeout");
    expect(rememberedFallback("s2")).toBeNull();
    expect(rememberedFallback(undefined)).toBeNull();
  });
});
