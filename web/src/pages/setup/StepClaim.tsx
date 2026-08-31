// Wizard step 1 — claim the instance (control-api.md "First-run setup");
// rendered only while no admin exists. Styled as LoginPage/RegisterPage.

import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { ApiError } from "../../api/client";
import { useAuth } from "../../auth/context";
import { Button } from "../../components/Button";
import { TextField } from "../../components/TextField";

interface StepClaimProps {
  /** Called after a successful claim; the caller advances to step 2. */
  onClaimed: () => void;
}

export function StepClaim({ onClaimed }: StepClaimProps) {
  const { claim } = useAuth();

  const [setupToken, setSetupToken] = useState("");
  const [email, setEmail] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  /** 409 setup_already_complete: someone else claimed the instance while this
   *  form was open. The error text is the server's own message (per spec,
   *  shown verbatim); this flag swaps the submit button for a sign-in link. */
  const [alreadyClaimed, setAlreadyClaimed] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setAlreadyClaimed(false);
    setSubmitting(true);
    try {
      await claim(setupToken, email, username, password);
      // Reset before handing off — the button must never stay stuck on
      // "Creating admin account…" if the hand-off is delayed.
      setSubmitting(false);
      onClaimed();
    } catch (err) {
      if (err instanceof ApiError) {
        // Server message verbatim — an operator debugging a claim needs the
        // real text, not a paraphrase.
        setError(err.message);
        if (err.status === 409) setAlreadyClaimed(true);
      } else {
        setError("Could not reach the server. Check your connection and try again.");
      }
      setSubmitting(false);
    }
  }

  return (
    <form className="card login-card" onSubmit={onSubmit} noValidate>
      {/* No wordmark here: the wizard's AuthCard already carries the lockup. */}
      <p className="sub" style={{ textAlign: "center", margin: 0 }}>
        This is a fresh Quasar instance with no administrator yet. Paste the
        one-time setup token to create the first admin account.
      </p>

      <div className="field">
        <label className="label" htmlFor="setup-token">
          Setup token
        </label>
        <input
          id="setup-token"
          className="input mono"
          type="text"
          name="setup-token"
          autoComplete="off"
          spellCheck={false}
          required
          value={setupToken}
          onChange={(e) => setSetupToken(e.target.value)}
          disabled={submitting}
          placeholder="paste the token here"
        />
        <span className="field-hint">
          Not printed to a log. Written to{" "}
          <code>/run/quasar/setup-token</code> on the host when the instance
          boots with no admin. Retrieve it with{" "}
          <code>
            docker compose -f deploy/docker-compose.yml exec
            quasar-control-plane cat /run/quasar/setup-token
          </code>
          . A restart before claiming mints a new token, so use the latest one.
        </span>
      </div>

      <TextField
        id="setup-email"
        label="Email"
        type="email"
        name="email"
        autoComplete="username"
        required
        value={email}
        onChange={(e) => setEmail(e.target.value)}
        disabled={submitting}
        placeholder="you@example.com"
      />

      <TextField
        id="setup-username"
        label="Username"
        type="text"
        name="username"
        autoComplete="username"
        required
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        disabled={submitting}
        placeholder="admin"
      />

      <TextField
        id="setup-password"
        label="Password"
        type="password"
        name="password"
        autoComplete="new-password"
        required
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        disabled={submitting}
        placeholder="At least 12 characters"
      />

      {error && (
        <p className="login-error" role="alert">
          {error}
        </p>
      )}

      {alreadyClaimed ? (
        <Link to="/login" className="btn btn-primary btn-block" style={{ textDecoration: "none" }}>
          Go to sign in
        </Link>
      ) : (
        <Button type="submit" variant="primary" className="btn-block" disabled={submitting}>
          {submitting ? "Creating admin account…" : "Create admin account"}
        </Button>
      )}
    </form>
  );
}
