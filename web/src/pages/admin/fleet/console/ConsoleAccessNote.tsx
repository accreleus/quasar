/**
 * The note above the "Local console" panel for a host whose agent reports
 * console access (amendment 18): off / applying / restored (failed) /
 * unsupported. Absent entirely when the host reports no `access` — that
 * host's page is unchanged (control-api.md §Console mode).
 *
 * Copy rule (design_handoff_v3/screens/rh07/README.md): console mode never
 * reads as on until the new node agent is healthy, and a replacement is said
 * to end the host's live sessions; wire identifiers (attempt id, the raw
 * failure reason) stay under the closed Details disclosure — the visible
 * sentence uses the mapped, plain-words reason.
 */

import { Diag } from "../Diag";
import { Button } from "../../../../components/Button";
import { IconRefresh } from "../../../../components/icons";
import type { ConsoleAccess } from "../../../../api/types";
import { clockTime } from "../../../../lib/format/clockTime";
import { shortId } from "../../../../lib/format/shortId";
import { accessReasonText, liveSessionsNoun } from "./access";

export function ConsoleAccessNote({
  access,
  hostName,
  liveSessions,
  onTryAgain,
  tryAgainPending,
}: {
  access: ConsoleAccess;
  hostName: string;
  liveSessions: number | null;
  onTryAgain: () => void;
  tryAgainPending: boolean;
}) {
  if (access.state === "off") {
    return (
      <div className="note" role="status">
        <strong>Console mode is off.</strong> Turned on, {hostName} shows games on its own
        screen. Its recovery actor replaces the node agent with one that can use this machine's
        display, sound and monitor control, which ends the {liveSessionsNoun(liveSessions)}.
      </div>
    );
  }

  if (access.state === "applying") {
    const started = access.started_at ? ` (started ${clockTime(access.started_at, { seconds: false })})` : "";
    return (
      <div className="note" role="status">
        <strong>Turning {access.target ? "on" : "off"} console mode.</strong> The recovery actor
        is replacing the node agent{started}. Console mode reads as on only once the new node
        agent is healthy; if it is not, the previous one is put back. New sessions on this host
        wait until then.
      </div>
    );
  }

  if (access.state === "restored") {
    const reasonText = accessReasonText(access.reason);
    return (
      <div className="note warn" role="alert">
        <div className="row wrap gap5">
          <div className="grow">
            <strong>Console mode did not turn {access.target ? "on" : "off"}.</strong>{" "}
            {reasonText || access.summary} The recovery actor put the previous node agent back,
            so streaming works as before.
            <Diag
              lines={[
                `attempt: ${shortId(access.request_id)}`,
                `outcome: failed, restored`,
                `reason: ${access.reason ?? "unknown"}`,
              ]}
            />
          </div>
          <Button variant="primary" onClick={onTryAgain} disabled={tryAgainPending}>
            <IconRefresh />
            {tryAgainPending ? "Trying again…" : "Try again"}
          </Button>
        </div>
      </div>
    );
  }

  if (access.state === "unsupported") {
    return (
      <div className="note warn" role="status">
        <strong>Console mode is not available on {hostName}.</strong> {access.summary}
      </div>
    );
  }

  // An access.state this build does not recognise reads as off (contract:
  // "a value this list does not name is shown verbatim and treated as off").
  return null;
}
