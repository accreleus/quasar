// Resume/skip offer for /admin while setup isn't complete. Reads the shared
// status cache (no extra request); renders nothing while loading or done.
// "Skip" calls POST /v1/setup/complete — completion is instance state, so the
// banner must retire for every admin everywhere, not just this browser.

import { Link } from "react-router-dom";
import { useState } from "react";
import { useAuth } from "../../auth/context";
import { useSetupStatus } from "../../setup/useSetupStatus";
import { completeSetup } from "../../api/setup";
import { reportBestEffortFailure } from "../../lib/reportBestEffortFailure";
import { Button } from "../../components/Button";

export function SetupResumeBanner() {
  const { token } = useAuth();
  const { status, setStatus } = useSetupStatus();
  const [skipping, setSkipping] = useState(false);

  if (!status || status.setup_completed) return null;

  async function skip() {
    if (!token) return;
    setSkipping(true);
    try {
      const result = await completeSetup(token);
      setStatus(result);
    } catch (err) {
      reportBestEffortFailure("console-warn", "setup: skip via POST /v1/setup/complete", err);
    } finally {
      setSkipping(false);
    }
  }

  return (
    <div
      className="login-error"
      role="status"
      style={{
        display: "flex",
        alignItems: "center",
        justifyContent: "space-between",
        gap: "var(--s4)",
        color: "var(--info-text)",
        background: "var(--info-bg)",
        borderColor: "var(--info-line)",
        margin: "0 0 var(--s5)",
      }}
    >
      <span>First-run setup isn&rsquo;t finished — instance basics and a host check are still pending.</span>
      <span style={{ display: "flex", gap: "var(--s2)", alignItems: "center", flexShrink: 0 }}>
        <Link to="/setup" className="btn btn-primary btn-sm" style={{ textDecoration: "none" }}>
          Resume setup
        </Link>
        <Button type="button" variant="ghost" size="sm" disabled={skipping} onClick={() => void skip()}>
          {skipping ? "Skipping…" : "Skip"}
        </Button>
      </span>
    </div>
  );
}
