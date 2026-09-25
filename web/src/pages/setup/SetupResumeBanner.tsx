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
    <div className="login-error is-info row between gap4 m0 mb5" role="status">
      <span>First-run setup isn&rsquo;t finished — instance basics and a host check are still pending.</span>
      <span className="setup-resume-actions row gap2">
        <Link to="/setup" className="btn btn-primary btn-sm">
          Resume setup
        </Link>
        <Button type="button" variant="ghost" size="sm" disabled={skipping} onClick={() => void skip()}>
          {skipping ? "Skipping…" : "Skip"}
        </Button>
      </span>
    </div>
  );
}
