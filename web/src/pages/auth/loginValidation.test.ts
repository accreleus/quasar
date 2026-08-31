/**
 * The mock's validation contract (design_handoff_v3/screens/login-v3.html,
 * handoff-v3-spec.md §C): submit-time only, three messages, email checked
 * before password so the first invalid field is the one that gets focus.
 */

import { describe, expect, it } from "vitest";
import { validateLogin } from "./loginValidation";

describe("validateLogin", () => {
  it("reports the mock's messages", () => {
    expect(validateLogin({ email: "", password: "" })).toEqual({
      email: "Enter your email address.",
      password: "Enter your password.",
    });
    expect(validateLogin({ email: "nope", password: "x" })).toEqual({
      email: "That does not look like an email address.",
    });
    expect(validateLogin({ email: "a@b.co", password: "x" })).toEqual({});
  });

  it("trims the email before judging it, as the mock does", () => {
    expect(validateLogin({ email: "   ", password: "x" })).toEqual({
      email: "Enter your email address.",
    });
    expect(validateLogin({ email: "  a@b.co  ", password: "x" })).toEqual({});
  });

  it("reports both fields at once so nothing is discovered twice", () => {
    expect(validateLogin({ email: "nope", password: "" })).toEqual({
      email: "That does not look like an email address.",
      password: "Enter your password.",
    });
  });
});
