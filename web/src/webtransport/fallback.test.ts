import { describe, expect, it } from "vitest";
import { fallbackCause, fallbackReason, rememberedFallback, rememberFallback } from "./fallback";
import { TransportError } from "./transport";

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

describe("fallbackCause (#1403)", () => {
  it("keeps the name and message of an error that is not a TransportError", () => {
    expect(fallbackCause("internal", new TypeError("boom"))).toBe("internal: TypeError: boom");
  });
  it("names a refusal by its code, plus the close detail when there is one", () => {
    expect(fallbackCause("auth_failed", new TransportError("auth_failed"))).toBe("auth_failed");
    expect(fallbackCause("internal", new TransportError("internal", "close 4400 bad"))).toBe("internal: close 4400 bad");
  });
  it("is capped at 200 characters", () => {
    expect(fallbackCause("internal", new Error("x".repeat(500)))).toHaveLength(200);
  });
  it("never splits a surrogate pair at the 200-character cap (#1478)", () => {
    // The prefix is 17 units, so 182 x's put an emoji's high half at unit 199 and its low half
    // at unit 200: the cap used to end the cause on the lone high half.
    const split = fallbackCause("internal", new Error("x".repeat(182) + "😀 tail"));
    expect(split).toHaveLength(199);
    expect(hasLoneSurrogate(split)).toBe(false);
    // A pair that ends exactly inside the cap is kept whole.
    const whole = fallbackCause("internal", new Error("x".repeat(181) + "😀 tail"));
    expect(whole).toHaveLength(200);
    expect(hasLoneSurrogate(whole)).toBe(false);
  });
});

/** True when `s` holds a UTF-16 code unit left unpaired, which a render shows as U+FFFD. */
function hasLoneSurrogate(s: string): boolean {
  const isHigh = (c: number) => c >= 0xd800 && c <= 0xdbff;
  const isLow = (c: number) => c >= 0xdc00 && c <= 0xdfff;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (isHigh(c) && !(i + 1 < s.length && isLow(s.charCodeAt(i + 1)))) return true;
    if (isLow(c) && !(i > 0 && isHigh(s.charCodeAt(i - 1)))) return true;
  }
  return false;
}
