// The stream line is labelled, names only what Play asks for (with no profile
// sent it names nothing), and wraps only between groups; a game's own settings
// get a "This game" marker and a one-press way back to the device recommendation.
// Settings that did not load say so, with Retry in Change's place.

import { Fragment } from "react";
import { Chip } from "../../../components/Chip";
import { IconSliders } from "../../../components/icons";
import { codecLabel } from "../launchOptions";
import { formatBitrate, resolutionLabel } from "../home/launchOptionRules";
import type { GamePage } from "./useGamePage";

export function StreamSettings({ g }: { g: GamePage }) {
  const r = g.ready ? g.selection.resolved : null;
  const prefsFailed = g.locked === "prefs";
  const groups = !r
    ? null
    : prefsFailed
      ? ["Your stream settings didn't load"]
      : g.oneClick
        ? ["Default settings"]
        : [resolutionLabel(r.height), `${r.fps} fps`, formatBitrate(r.bitrateKbps), ...(r.codec ? [codecLabel(r.codec)] : [])];
  const chosenBy = prefsFailed
    ? "(Play lets the server choose)"
    : g.oneClick
      ? "(chosen by the server at launch)"
      : r && !r.codec
        ? "(codec chosen by the server)"
        : null;
  const rec = g.recommended;
  return (
    <>
      <div className="gp-settings">
        <div className="gp-stream">
          <span className="gp-stream-label">Stream</span>
          <p className="gp-sentence gp-stream-line" aria-live="polite">
            {groups ? (
              <>
                {groups.map((t, i) => (
                  <Fragment key={i}>
                    {i > 0 && " "}
                    <span className="gp-stream-group">{i > 0 ? `· ${t}` : t}</span>
                  </Fragment>
                ))}
                {chosenBy && (
                  <>
                    {" "}
                    <span className="gp-stream-auto">{chosenBy}</span>
                  </>
                )}
              </>
            ) : (
              <span className="gp-skel">{g.profilesError ? "Not available" : "Checking this device…"}</span>
            )}
          </p>
        </div>
        {g.profilesError || prefsFailed ? (
          <button type="button" className="btn btn-sm" onClick={g.profilesError ? g.notes.onRetryProfiles : g.retryPrefs}>
            Retry
          </button>
        ) : (
          <button
            type="button"
            ref={g.changeRef}
            className="btn btn-sm"
            disabled={!g.model || g.locked !== null}
            aria-haspopup="dialog"
            aria-expanded={g.optionsOpen}
            onClick={g.openOptions}
          >
            <IconSliders className="ic" />
            Change
          </button>
        )}
        {g.perGame && rec && (
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => {
              g.resetToRecommended();
              // The button goes away with the per-game state; keep focus on the line.
              requestAnimationFrame(() => g.changeRef.current?.focus());
            }}
          >
            Use recommended ({resolutionLabel(rec.height)} · {rec.fps} fps)
          </button>
        )}
      </div>
      <div className="d-rec gp-rec" data-tone={g.rec.tone}>
        {g.perGame ? (
          <>
            <Chip variant="info">This game</Chip>
            <span className="d-rec-text">Your last settings for this game.</span>
          </>
        ) : (
          <>
            <span className="dot" aria-hidden />
            <span className="d-rec-text">{g.rec.text}</span>
          </>
        )}
      </div>
    </>
  );
}
