/**
 * The host page's "must update before it can be managed" note and its one action
 * (design_handoff_v3 rh06 floor.png, floor-actor.png, floor-unknown.png, floor-failed.png;
 * `pageRhFloor` in assets/pages-rh06.js).
 */

import type { Host } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { IconChevronRight, IconDownload, IconRefresh } from "../../../../components/icons";
import { floorPhrase, type FloorState } from "../hostFloor";
import { versionLabel } from "../hostServices";
import { eligibilityText, failureText, releaseLabel } from "../releasesCopy";

export interface FloorNoteProps {
  host: Host;
  state: FloorState;
  /** Live sessions on the host, for "Updating ends its N live sessions". */
  liveSessions: number;
  onUpdate: () => void;
}

const vOf = (v: string | null | undefined) => versionLabel(v) ?? "not reported";

/** "Its node agent and recovery actor are v0.4.1", naming only what the update moves. */
function versionsSentence(host: Host, state: FloorState): string {
  const agent = vOf(host.agent_version);
  const actor = vOf(host.recovery_actor_version);
  if (state.movesAgent && state.movesActor) {
    return agent === actor
      ? `Its node agent and recovery actor are ${agent}`
      : `Its node agent is ${agent} and its recovery actor is ${actor}`;
  }
  return state.movesActor ? `Its recovery actor is ${actor}` : `Its node agent is ${agent}`;
}

function sessionsSentence(state: FloorState, live: number): string {
  if (!state.movesAgent) {
    return "Sessions keep running. The only thing offered to this host is an update, which replaces the recovery actor and ends no sessions.";
  }
  const ends =
    live === 0
      ? "No session is running on it now."
      : `Updating ends its ${live} live session${live === 1 ? "" : "s"}.`;
  return `It keeps running sessions, but the only thing offered to it is an update. ${ends}`;
}

export function FloorNote({ host, state, liveSessions, onUpdate }: FloorNoteProps) {
  if (state.kind === "unknown") {
    return (
      <p className="note host-note">
        <b>Version not reported.</b> {host.node_name} has not said which release it runs, so the
        console cannot tell whether this control plane manages it. Nothing is offered until it
        reports.
      </p>
    );
  }

  const managed = floorPhrase(state.floor);
  const canUpdate = !!state.release && !!state.target?.eligible;
  const blocked = !canUpdate && state.target?.reason ? eligibilityText(state.target.reason) : null;

  if (state.failed) {
    const a = state.failed;
    const actorMoved =
      a.requested_digests.some((c) => c.name === "recovery-actor") && !state.movesActor;
    return (
      <div className="note warn host-note host-floor">
        <div className="host-floor-text">
          <b>The update did not finish; {host.node_name} still must update.</b>{" "}
          {actorMoved &&
            `As every update does, it replaced the recovery actor first, which is now ${vOf(host.recovery_actor_version)}. `}
          {failureText(a.reason)}
          {blocked && <span className="hint host-floor-why">{blocked}</span>}
          <details className="diag">
            <summary>
              <IconChevronRight />
              Details
            </summary>
            <pre>
              {[
                `attempt: ${a.id} · outcome: ${a.state}`,
                `reason: ${a.reason ?? "none"}`,
                `components: ${a.requested_digests.map((c) => c.name).join(", ")}`,
              ].join("\n")}
            </pre>
          </details>
        </div>
        <Button variant="primary" disabled={!canUpdate} onClick={onUpdate}>
          <IconRefresh />
          Try again
        </Button>
      </div>
    );
  }

  return (
    <div className="note warn host-note host-floor">
      <div className="host-floor-text">
        <b>{host.node_name} must update before it can be managed.</b>{" "}
        {versionsSentence(host, state)}
        {managed
          ? `; this control plane manages ${managed}. `
          : "; this control plane no longer manages that release. "}
        {sessionsSentence(state, liveSessions)}
        {blocked && <span className="hint host-floor-why">{blocked}</span>}
      </div>
      {state.release && (
        <Button variant="primary" disabled={!canUpdate} onClick={onUpdate}>
          <IconDownload />
          Update to {versionLabel(releaseLabel(state.release))}
        </Button>
      )}
    </div>
  );
}
