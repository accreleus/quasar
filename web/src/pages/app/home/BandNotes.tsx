// The detail band's stacked notes, under the recommendation line and above
// Play. Each answers a defect rather than a mockup: the eligibility reasons as
// sentences (risky and dead-end alike, with a re-check that re-runs the
// evaluation), Resume rather than relaunch when this app owns the live session
// (UX §3.3), the blocked-sibling note naming the app, and #494's slot wait.

import { Link } from "react-router-dom";
import type { ProfileReason } from "../../../api/types";
import { Button } from "../../../components/Button";
import { IconInfo } from "../../../components/icons";
import { reasonSentences } from "../launchOptions";
import { UNEXPLAINED } from "./launchOptionRules";

export interface BandNotesProps {
  appName: string;
  /** The committed selection is `risky`. */
  risky: boolean;
  riskyReasons: readonly ProfileReason[];
  /** Nothing in the response can stream to this device. */
  deadEnd: boolean;
  deadEndReasons: readonly ProfileReason[];
  /** This app owns the live session. */
  isLive: boolean;
  isBlocked: boolean;
  blockedByName: string | null;
  liveSessionId: string | null;
  canDecodeH264: boolean;
  waitingForSlot: boolean;
  onRetryProfiles: () => void;
}

export function BandNotes({
  appName,
  risky,
  riskyReasons,
  deadEnd,
  deadEndReasons,
  isLive,
  isBlocked,
  blockedByName,
  liveSessionId,
  canDecodeH264,
  waitingForSlot,
  onRetryProfiles,
}: BandNotesProps) {
  return (
    <>
      {risky && (
        <div className="note warn lib-why" role="status">
          <IconInfo />
          <div>
            <b>This may not stream well here</b>
            <ReasonList reasons={riskyReasons} severity="risky" />
            <p>Play anyway, or use Adjust to pick something steadier.</p>
          </div>
        </div>
      )}

      {deadEnd && (
        <div className="note warn lib-why" role="alert">
          <IconInfo />
          <div>
            <b>Nothing here can stream to this device</b>
            <ReasonList reasons={deadEndReasons} severity="ineligible" />
            <p>
              An admin can add a lower-quality option for {appName}. If your network or device has
              changed since Quasar last measured it, check again.
            </p>
            <Button size="sm" onClick={onRetryProfiles}>
              Check again
            </Button>
          </div>
        </div>
      )}

      {/* Resume, not relaunch (§3.3) — and say so, because the button below has
          become a different button. */}
      {isLive && (
        <div className="note">
          <IconInfo />
          <div>
            You're playing <b>{appName}</b> right now. Resume returns you to that session; these
            launch settings apply the next time you start it.
          </div>
        </div>
      )}

      {isBlocked && (
        <div className="note warn">
          <IconInfo />
          <div>
            <b>{blockedByName}</b> is already running.{" "}
            {liveSessionId ? (
              <Link to={`/app/session/${liveSessionId}`}>Go to your session</Link>
            ) : (
              "Stop that session first, then try again."
            )}
          </div>
        </div>
      )}

      {!canDecodeH264 && (
        <p className="lm-cap-fatal" role="alert">
          Without H.264 this device cannot play any stream. Try a different browser.
        </p>
      )}

      {waitingForSlot && (
        <div className="note" role="status">
          <IconInfo />
          <div>All capacity is in use right now, waiting for a slot to free up…</div>
        </div>
      )}
    </>
  );
}

/** Reasons in the user's words (launchOptions.reasonSentences degrades an
 *  unrecognised code to the server's own message). */
function ReasonList({
  reasons,
  severity,
}: {
  reasons: readonly ProfileReason[];
  severity: "risky" | "ineligible";
}) {
  const lines = reasonSentences(reasons, severity);
  return (
    <ul>
      {(lines.length > 0 ? lines : [UNEXPLAINED]).map((line) => (
        <li key={line}>{line}</li>
      ))}
    </ul>
  );
}
