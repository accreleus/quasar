// The password form on /app/account/profile (handoff §A.22).
//
// CP-01: POST /v1/me/password returns 204 and revokes every token, so on
// success the local session is already dead — clear it and go to /login rather
// than leaving the page holding a token the server has forgotten.

import { useState } from "react";
import { useNavigate } from "react-router-dom";
import * as authApi from "../../../api/auth";
import { ApiError } from "../../../api/client";
import { useAuth } from "../../../auth/context";
import { clearSession } from "../../../auth/storage";
import { Button } from "../../../components/Button";
import { TextField } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";

export function PasswordCard() {
  const { token, logout } = useAuth();
  const navigate = useNavigate();
  const { addToast } = useToast();

  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);

    if (next !== confirm) {
      setError("New passwords do not match.");
      return;
    }
    if (!token) return;

    setSubmitting(true);
    try {
      await authApi.changePassword(token, current, next);
      addToast({
        variant: "success",
        title: "Password updated",
        body: "You have been signed out of all devices. Please sign in again.",
        duration: 5000,
      });
      clearSession();
      await logout();
      navigate("/login", { replace: true });
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.code === "invalid_credentials" || err.status === 401) {
          setError("Current password is incorrect.");
        } else if (err.code === "validation_failed" || err.status === 400) {
          setError("New password is too short or weak (minimum 12 characters).");
        } else {
          setError(err.message ?? "Password change failed.");
        }
      } else {
        setError("An unexpected error occurred.");
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="card sec-card mt4">
      <div className="sec-head">
        <div>
          <h3>Password</h3>
          <div className="desc">Use at least 12 characters.</div>
        </div>
      </div>

      <form onSubmit={(e) => void handleSubmit(e)}>
        <div className="ac-pw">
          <TextField
            label="Current password"
            type="password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            autoComplete="current-password"
            placeholder="••••••••••••"
            required
          />
          <div className="grid g2 mt4">
            <TextField
              label="New password"
              type="password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
              autoComplete="new-password"
              placeholder="••••••••••••"
              required
            />
            <TextField
              label="Confirm new password"
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
              placeholder="••••••••••••"
              required
            />
          </div>
        </div>

        {error && <p className="form-error mt4" role="alert">{error}</p>}

        <p className="note warn mt5">
          Changing your password signs you out of <strong>every device</strong>, including
          this one, and ends any session you have running.
        </p>

        <div className="row gap3 mt5">
          <Button type="submit" variant="primary" disabled={submitting}>
            {submitting ? "Updating…" : "Update password"}
          </Button>
        </div>
      </form>
    </div>
  );
}
