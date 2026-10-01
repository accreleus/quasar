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
 *
 * RH07-15 (#407): a restored attempt's `access.summary` now carries the
 * console preflight's own named cause (who holds the display, or that the
 * host was never prepared with `--console`) in front of the generic
 * put-back sentence — see `consoleFailureCategory` in `./access`. That named
 * cause used to be hidden behind the reason-mapped text; it now leads.
 */

import { Diag } from "../Diag";
import { Snippet } from "../Snippet";
import { Button } from "../../../../components/Button";
import { IconRefresh } from "../../../../components/icons";
import type { ConsoleAccess, Host } from "../../../../api/types";
import { clockTime } from "../../../../lib/format/clockTime";
import { shortId } from "../../../../lib/format/shortId";
import { accessReasonText, consoleFailureCategory, liveSessionsNoun, CONSOLE_RULES_COMMAND } from "./access";

export function ConsoleAccessNote({
  access,
  host,
  liveSessions,
  onTryAgain,
  tryAgainPending,
}: {
  access: ConsoleAccess;
  host: Pick<Host, "node_name" | "engine" | "engine_mode">;
  liveSessions: number | null;
  onTryAgain: () => void;
  tryAgainPending: boolean;
}) {
  const hostName = host.node_name;
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
    const category = consoleFailureCategory(access);
    const diagLines = [
      `attempt: ${shortId(access.request_id)}`,
      `outcome: failed, restored`,
      `reason: ${access.reason ?? "unknown"}`,
    ];
    const tryAgainButton = (
      <Button variant="primary" onClick={onTryAgain} disabled={tryAgainPending}>
        <IconRefresh />
        {tryAgainPending ? "Trying again…" : "Try again"}
      </Button>
    );

    // The display was held by another process: the preflight's own detail
    // (who holds it, when logind names them) leads `access.summary`, so lead
    // with it here too, then the mock's guidance (design_handoff_v3/screens/
    // rh07/README.md, specimen "d" failed).
    if (category === "held") {
      return (
        <div className="note warn" role="alert">
          <div className="row wrap gap5">
            <div className="grow">
              <strong>Console mode did not turn {access.target ? "on" : "off"}.</strong>{" "}
              {access.summary} Stop the login screen on this display, or choose another output,
              then try again.
              <Diag lines={diagLines} />
            </div>
            {tryAgainButton}
          </div>
        </div>
      );
    }

    // The host lacks the console device rules: same lead, plus the mock's
    // copyable fix.
    if (category === "unprepared") {
      return (
        <div className="note warn" role="alert">
          <div className="row wrap gap5">
            <div className="grow">
              <strong>
                Console mode did not turn {access.target ? "on" : "off"}: {hostName} is not
                prepared for it.
              </strong>{" "}
              {access.summary} Install the console device rules, then try again.
              <Diag lines={diagLines} />
            </div>
            {tryAgainButton}
          </div>
          <Snippet
            caption={`Run on ${hostName} as root`}
            text={CONSOLE_RULES_COMMAND}
            testId="console-access-prepare-snippet"
            label="Copy the console device-rules commands"
          />
        </div>
      );
    }

    // Generic: a plain replacement failure with no preflight detail —
    // access.summary leads (the fix here: it no longer hides behind the
    // reason-mapped text), and the mapped reason still follows it, since it
    // reads friendlier than the raw identifier access.summary carries inline.
    const tail = reasonText && reasonText !== access.summary ? ` ${reasonText}` : "";
    return (
      <div className="note warn" role="alert">
        <div className="row wrap gap5">
          <div className="grow">
            <strong>Console mode did not turn {access.target ? "on" : "off"}.</strong>{" "}
            {access.summary}
            {tail} The recovery actor put the previous node agent back, so streaming works as
            before.
            <Diag lines={diagLines} />
          </div>
          {tryAgainButton}
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
