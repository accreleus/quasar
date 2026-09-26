/**
 * The seed-missing and owner-conflict notes above "Services on this machine"
 * (design_handoff_v3 fleet-rh06-v3.html `pageRhWarning`; screenshots rh06/seed-missing,
 * conflict, conflict-error). What decides them is hostWarnings.ts.
 */

import type { Host } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { IconRefresh } from "../../../../components/icons";
import { clockTime } from "../../../../lib/format/clockTime";
import { Diag } from "../Diag";
import { OWNER_CONFLICT, ownerConflict, seedMissing } from "../hostWarnings";

export interface HostWarningsProps {
  host: Host;
  /** When the admin last pressed Check again (ms), while the conflict stands. */
  checkedAt: number | null;
  onCheckAgain: () => void;
  now: number;
}

/** The agent re-checks every 15 s but reports readiness only when it changes, so a
 *  press with no newer report after this long reads as "found again". */
const CHECK_AGAIN_WAIT_MS = 30_000;

export function HostWarnings({ host, checkedAt, onCheckAgain, now }: HostWarningsProps) {
  const node = host.node_name;
  const conflict = ownerConflict(host);
  const reportedAt = host.readiness_reported_at ? Date.parse(host.readiness_reported_at) : null;
  // A report newer than the press is the answer to it; until one arrives, it is pending.
  const answered =
    checkedAt != null &&
    ((reportedAt != null && reportedAt >= checkedAt) || now - checkedAt > CHECK_AGAIN_WAIT_MS);
  const checking = checkedAt != null && !answered;

  return (
    <>
      {seedMissing(host) && (
        <p className="note warn host-note" role="status">
          <strong>No seed found on {node}.</strong> Quasar is running normally, but if this
          machine’s recovery actor is ever deleted, nothing will re-create it. Start the seed
          again the way you first started it — in your external manager, or with the command from
          Add host. The machine keeps its identity and nothing else changes.
        </p>
      )}
      {conflict && (
        <div className="note warn host-note" role="alert">
          {answered ? (
            <>
              <strong>Still in the way.</strong> It was found again at{" "}
              {clockTime(new Date(Math.max(reportedAt ?? 0, checkedAt ?? 0)).toISOString(), {
                seconds: false,
              })}
              , so nothing has
              changed. If you removed a stack, check that its containers are gone: stopped
              containers count too.
            </>
          ) : (
            <>
              <strong>Another owner’s container is in the way on {node}.</strong>{" "}
              {conflict.summary}. Quasar never stops, renames or removes a container it did not
              create, so it will not update this machine while that container exists. Remove it on{" "}
              {node}, then check again.
            </>
          )}
          <div className="rh-note-actions">
            <Button size="sm" disabled={checking} onClick={onCheckAgain}>
              <IconRefresh />
              {checking ? "Checking…" : "Check again"}
            </Button>
          </div>
          {!answered && (
            <Diag
              lines={[
                `readiness check: ${OWNER_CONFLICT}`,
                `found: ${conflict.summary}`,
                ...(conflict.remediation ? [`fix: ${conflict.remediation}`] : []),
                ...(conflict.observed_at ?? host.readiness_reported_at
                  ? [`checked: ${conflict.observed_at ?? host.readiness_reported_at}`]
                  : []),
                "blocks: platform updates on this machine",
              ]}
            />
          )}
        </div>
      )}
    </>
  );
}
