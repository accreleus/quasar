// Sign-in screen. Authenticates against control-api.md POST /v1/auth/login;
// on success the AuthProvider persists the bearer token and we redirect into
// the app (back to wherever the user was headed, if anything).
//
// v3 surface: the pre-auth glass card (AuthCard + styles/login.css), built to
// handoff-v3-spec.md §C. Validation is submit-time with the mock's three
// messages and clears per field as the user types; the server's 401 lands
// under the password field. There is deliberately no "Forgot password?" link:
// no reset flow exists server-side, and a dead link is worse than none
// (2026-08-28-ui-v3-design.md §9).

import { useRef, useState, type FormEvent } from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";
import { ApiError } from "../api/client";
import { useAuth } from "../auth/context";
import { AuthCard } from "./auth/AuthCard";
import { validateLogin, type LoginErrors } from "./auth/loginValidation";

interface FromState {
  from?: { pathname: string };
}

export function LoginPage() {
  const { status, login } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const redirectTo = (location.state as FromState | null)?.from?.pathname ?? "/app";

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [remember, setRemember] = useState(true);
  const [reveal, setReveal] = useState(false);
  const [errors, setErrors] = useState<LoginErrors>({});
  const [submitting, setSubmitting] = useState(false);

  const emailRef = useRef<HTMLInputElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);

  // Already signed in (e.g. navigated to /login manually) — bounce to the app.
  if (status === "authenticated") return <Navigate to={redirectTo} replace />;

  function toggleReveal() {
    setReveal((shown) => !shown);
    passwordRef.current?.focus();
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();

    const found = validateLogin({ email, password });
    setErrors(found);
    if (found.email) {
      emailRef.current?.focus();
      return;
    }
    if (found.password) {
      passwordRef.current?.focus();
      return;
    }

    setSubmitting(true);
    try {
      await login(email.trim(), password, remember);
      navigate(redirectTo, { replace: true });
    } catch (err) {
      // Every server-side failure is a fact about the credentials as far as
      // this form is concerned, so it renders in the password field's slot.
      if (err instanceof ApiError && (err.status === 401 || err.code === "invalid_credentials")) {
        setErrors({ password: "Email or password is incorrect." });
      } else if (err instanceof ApiError) {
        setErrors({ password: err.message });
      } else {
        setErrors({ password: "Could not reach the server. Check your connection and try again." });
      }
      setSubmitting(false);
    }
  }

  return (
    <AuthCard>
      <form onSubmit={onSubmit} noValidate>
        <div className="field">
          <label htmlFor="login-email">Email</label>
          <input
            id="login-email"
            ref={emailRef}
            className="input"
            type="email"
            name="email"
            inputMode="email"
            autoComplete="username"
            placeholder="you@example.com"
            required
            aria-invalid={errors.email ? true : undefined}
            aria-describedby="login-email-error"
            value={email}
            onChange={(e) => {
              setEmail(e.target.value);
              if (errors.email) setErrors((prev) => ({ ...prev, email: undefined }));
            }}
            disabled={submitting}
          />
          <span className="error" id="login-email-error" role="alert">
            {errors.email ?? ""}
          </span>
        </div>

        <div className="field">
          <label htmlFor="login-password">Password</label>
          <div className="pw">
            <input
              id="login-password"
              ref={passwordRef}
              className="input"
              type={reveal ? "text" : "password"}
              name="password"
              autoComplete="current-password"
              placeholder="Your password"
              required
              aria-invalid={errors.password ? true : undefined}
              aria-describedby="login-password-error"
              value={password}
              onChange={(e) => {
                setPassword(e.target.value);
                if (errors.password) setErrors((prev) => ({ ...prev, password: undefined }));
              }}
              disabled={submitting}
            />
            <button className="reveal" type="button" aria-pressed={reveal} onClick={toggleReveal}>
              {reveal ? "Hide" : "Show"}
            </button>
          </div>
          <span className="error" id="login-password-error" role="alert">
            {errors.password ?? ""}
          </span>
        </div>

        <label className="check" htmlFor="login-remember">
          <input
            id="login-remember"
            type="checkbox"
            name="remember"
            checked={remember}
            onChange={(e) => setRemember(e.target.checked)}
            disabled={submitting}
          />
          Keep me signed in on this device
        </label>

        <button
          className={`btn btn-primary btn-block${submitting ? " is-submitting" : ""}`}
          type="submit"
          disabled={submitting}
        >
          {submitting ? (
            <>
              <span className="spin" aria-hidden="true" />
              Signing in
            </>
          ) : (
            "Sign in"
          )}
        </button>

        <p className="link-line">
          Have an invite?{" "}
          <Link className="link-quiet" to="/register">
            Create an account
          </Link>
        </p>
      </form>
    </AuthCard>
  );
}
