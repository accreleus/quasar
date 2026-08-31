// Register screen — SEC-06a. Creates an account against control-api.md
// POST /v1/auth/register, then signs the new user straight in (POST /v1/auth/login)
// and redirects into the app.
//
// Same pre-auth card as /login (AuthCard + styles/login.css, handoff-v3-spec.md
// §C) so both read as one product: the same lockup, labels, inputs and error
// slots. The fields and copy are this page's own.
//
// The invite code is never typed by the user — it rides the magic link's ?invite=
// query param (LP-SEC-01 §B.5) and is submitted invisibly with the register call.

import { useState, type FormEvent } from "react";
import { Link, Navigate, useNavigate, useSearchParams } from "react-router-dom";
import * as authApi from "../api/auth";
import { ApiError } from "../api/client";
import { useAuth } from "../auth/context";
import { AuthCard } from "./auth/AuthCard";

export function RegisterPage() {
  const { status, login } = useAuth();
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const inviteCode = params.get("invite") ?? undefined;

  const [email, setEmail] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  // Two slots, because the two kinds of message belong in different places:
  // the confirmation mismatch is a fact about one field and sits under it;
  // everything the server says is about the submission as a whole.
  const [confirmError, setConfirmError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // Already signed in — bounce to the app.
  if (status === "authenticated") return <Navigate to="/app" replace />;

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setConfirmError(null);
    setFormError(null);

    if (password !== confirm) {
      setConfirmError("Passwords do not match.");
      return;
    }

    setSubmitting(true);
    try {
      await authApi.register(email, username, password, inviteCode);
      // Registration succeeded — sign the new account straight in.
      await login(email, password);
      navigate("/app", { replace: true });
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.code === "invalid_invite") {
          setFormError("This invite link is invalid or has expired.");
        } else if (err.code === "registration_closed") {
          setFormError("Registration is currently closed. Ask an administrator for an invite.");
        } else if (err.code === "conflict") {
          setFormError("That email or username is already taken.");
        } else {
          setFormError(err.message);
        }
      } else {
        setFormError("Could not reach the server. Check your connection and try again.");
      }
      setSubmitting(false);
    }
  }

  return (
    <AuthCard>
      <form onSubmit={onSubmit} noValidate>
        {inviteCode && (
          <p className="auth-note" role="status">
            You&rsquo;ve been invited to join Quasar.
          </p>
        )}

        <div className="field">
          <label htmlFor="register-email">Email</label>
          <input
            id="register-email"
            className="input"
            type="email"
            name="email"
            inputMode="email"
            autoComplete="email"
            placeholder="you@example.com"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            disabled={submitting}
          />
        </div>

        <div className="field">
          <label htmlFor="register-username">Username</label>
          <input
            id="register-username"
            className="input"
            type="text"
            name="username"
            autoComplete="username"
            placeholder="yourname"
            required
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            disabled={submitting}
          />
        </div>

        <div className="field">
          <label htmlFor="register-password">Password</label>
          <input
            id="register-password"
            className="input"
            type="password"
            name="password"
            autoComplete="new-password"
            placeholder="At least 12 characters"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            disabled={submitting}
          />
        </div>

        <div className="field">
          <label htmlFor="register-confirm">Confirm password</label>
          <input
            id="register-confirm"
            className="input"
            type="password"
            name="confirm-password"
            autoComplete="new-password"
            placeholder="Repeat your password"
            required
            aria-invalid={confirmError ? true : undefined}
            aria-describedby="register-confirm-error"
            value={confirm}
            onChange={(e) => {
              setConfirm(e.target.value);
              if (confirmError) setConfirmError(null);
            }}
            disabled={submitting}
          />
          <span className="error" id="register-confirm-error" role="alert">
            {confirmError ?? ""}
          </span>
        </div>

        <p className="error" role="alert">
          {formError ?? ""}
        </p>

        <button
          className={`btn btn-primary btn-block${submitting ? " is-submitting" : ""}`}
          type="submit"
          disabled={submitting}
        >
          {submitting ? (
            <>
              <span className="spin" aria-hidden="true" />
              Creating account
            </>
          ) : (
            "Create account"
          )}
        </button>

        <p className="link-line">
          Already have an account?{" "}
          <Link className="link-quiet" to="/login">
            Sign in
          </Link>
        </p>
      </form>
    </AuthCard>
  );
}
