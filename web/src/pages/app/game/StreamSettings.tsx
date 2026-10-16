// The stream line is labelled, names only what Play sends (a size or a codec the
// server may not pick is never promised), and wraps only between groups; a
// game's own settings get a "This game" marker and a one-press way back to the
// device recommendation.

import { Fragment } from "react";
import { Chip } from "../../../components/Chip";
import { IconSliders } from "../../../components/icons";
import { codecLabel } from "../launchOptions";
import { formatBitrate, resolutionLabel } from "../home/launchOptionRules";
import type { GamePage } from "./useGamePage";

export function StreamSettings({ g }: { g: GamePage }) {
  const r = g.ready ? g.selection.resolved : null;
  const groups = !r
    ? null
    : g.oneClick
      ? ["Default settings"]
      : [resolutionLabel(r.height), `${r.fps} fps`, formatBitrate(r.bitrateKbps), ...(r.codec ? [codecLabel(r.codec)] : [])];
  const chosenBy = g.oneClick ? "(chosen by the server at launch)" : r && !r.codec ? "(codec chosen by the server)" : null;
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
              <span className="gp-skel">Checking this device…</span>
            )}
          </p>
        </div>
        {g.profilesError ? (
          <button type="button" className="btn btn-sm" onClick={g.notes.onRetryProfiles}>
            Retry
          </button>
        ) : (
          <button
            type="button"
            ref={g.changeRef}
            className="btn btn-sm"
            disabled={!g.model}
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
