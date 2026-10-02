/**
 * The host page's note for a removal in flight, failed, or done (design_handoff_v3
 * fleet-rh06-v3.html `rhRemove`; screenshots rh06/remove-progress, remove-failed). The
 * states are removeHost.ts's.
 */

import type { Host, Session } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { IconRefresh } from "../../../../components/icons";
import { elapsedWords } from "../../../../lib/format/relativeTime";
import { Diag } from "../Diag";
import type { Removal } from "../removeHost";

export interface RemovalNoteProps {
  host: Host;
  removal: Removal;
  sessions: Pick<Session, "created_at">[];
  now: number;
  pending: boolean;
  onRetry: () => void;
  onForget: () => void;
}

export function RemovalNote({ host, removal, sessions, now, pending, onRetry, onForget }: RemovalNoteProps) {
  const node = host.node_name;
  if (removal.phase === "waiting") {
    const oldest = sessions
      .map((s) => s.created_at)
      .filter((at): at is string => !!at)
      .sort()[0];
    return (
      <p className="note host-note" role="status">
        <strong>Removing {node}.</strong>{" "}
        {sessions.length > 0 ? (
          <>
            It takes no new sessions and is waiting for {sessions.length} live session
            {sessions.length === 1 ? "" : "s"} to end
            {oldest ? ` (longest: ${elapsedWords(oldest, now)} so far)` : ""}. Its node agent and
            recovery actor are removed after that.
          </>
        ) : (
          <>It takes no new sessions; its node agent and recovery actor are being removed.</>
        )}
      </p>
    );
  }
  if (removal.phase === "sent") {
    return (
      <p className="note host-note" role="status">
        <strong>Removing {node}.</strong> Its recovery actor is removing the node agent, then
        itself; the host goes offline when that is done.
      </p>
    );
  }
  if (removal.phase === "removed") {
    return (
      <div className="note host-note rh-note-row" role="status">
        <div className="rh-note-body">
          <strong>{node} was removed.</strong> Its node agent and recovery actor are gone; homes and
          data stay on the machine. Forget it to take it off the fleet list, or add a host with the
          same node name to bring it back.
        </div>
        <Button size="sm" onClick={onForget}>
          Forget host
        </Button>
      </div>
    );
  }
  return (
    <div className="note warn host-note rh-note-row" role="alert">
      <div className="rh-note-body">
        <strong>Removing {node} did not finish.</strong> The host is drained and still enrolled.{" "}
        {removal.summary}
        {removal.detail && <Diag lines={removal.detail.split("\n")} />}
      </div>
      <Button variant="danger" size="sm" disabled={pending} onClick={onRetry}>
        <IconRefresh />
        Retry removal
      </Button>
    </div>
  );
}
