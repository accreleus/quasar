/**
 * Sign-in validation, kept pure so the rules can be tested without a DOM.
 *
 * The messages and the pattern are the mock's verbatim
 * (design_handoff_v3/screens/login-v3.html). The pattern is deliberately the
 * loose one: a client cannot know which addresses a server will accept, so it
 * only catches the typo class ("no @", "no dot", "a space in the middle").
 */

export interface LoginValues {
  email: string;
  password: string;
}

/** Only the fields that failed are present — an empty object means valid. */
export interface LoginErrors {
  email?: string;
  password?: string;
}

export const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export function validateLogin({ email, password }: LoginValues): LoginErrors {
  const errors: LoginErrors = {};

  const trimmed = email.trim();
  if (!trimmed) errors.email = "Enter your email address.";
  else if (!EMAIL_PATTERN.test(trimmed)) errors.email = "That does not look like an email address.";

  if (!password) errors.password = "Enter your password.";

  return errors;
}

/** Field order for focusing the first invalid input (email before password). */
export const LOGIN_FIELD_ORDER: (keyof LoginErrors)[] = ["email", "password"];
