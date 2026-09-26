/**
 * "Services on this machine" for an owned host (design_handoff_v3
 * fleet-rh06-v3.html, `rhServices`; screenshots rh06/inv-gpu, inv-unknown, inv-combined).
 * The rows come from hostServices.ts; this file only draws them.
 */

import type { ReactNode } from "react";
import type { HostServices, ServiceRow, ServiceState } from "../hostServices";
import { Chip } from "../../../../components/Chip";
import { clockTime } from "../../../../lib/format/clockTime";
import { Diag } from "../Diag";

/** "13:48", as the mock's "as of" chips and "Last report from" read. */
const reportClock = (at: string): string => clockTime(at, { seconds: false });
import { elapsedWords } from "../../../../lib/format/relativeTime";

export interface ServicesCardProps {
  nodeName: string;
  services: HostServices;
  /** When the agent connected, for the "not reported yet" note. */
  connectedSince: string | null;
  now: number;
  /** A GPU host's footer: Remove host, or a removal in flight (`rhRemoveFoot`). */
  foot?: ReactNode;
  /** The host's short id, for the Details of a recovery actor that stopped answering. */
  hostId?: string;
}

export function ServicesCard({
  nodeName,
  services,
  connectedSince,
  now,
  foot,
  hostId,
}: ServicesCardProps) {
  const { report, rows } = services;
  const since = connectedSince ? elapsedWords(connectedSince, now) : null;

  return (
    <div className="card host-services">
      <div className="panel-head">
        <div>
          <span className="panel-title">Services on this machine</span>
          <div className="hint host-services-hint">{headHint(services)}</div>
        </div>
        <div className="acts">
          <Chip>{services.shape}</Chip>
        </div>
      </div>

      {report === "not_reported" && (
        <div className="host-services-note">
          <p className="note">
            {nodeName} {since ? `connected ${since} ago and ` : ""}has not reported its services
            yet.
          </p>
        </div>
      )}
      {report === "not_answering" && services.lastReportAt != null && (
        <div className="host-services-note">
          <div className="note warn">
            <b>Could not read this machine’s services.</b> Its recovery actor has not answered for{" "}
            {elapsedWords(new Date(services.lastReportAt).toISOString(), now)}, so the list below
            is its last report. The node agent is connected; sessions are unaffected.
            <Diag
              lines={[
                "recovery actor answered: no (the node agent's last register)",
                `last status: ${new Date(services.lastReportAt).toISOString()}`,
                ...(hostId ? [`host: ${hostId}`] : []),
              ]}
            />
          </div>
        </div>
      )}
      {report === "not_answering" && services.lastReportAt == null && (
        <div className="host-services-note">
          <p className="note warn">
            <b>Could not read this machine’s services.</b> Its recovery actor did not answer when
            the node agent {since ? `connected ${since} ago` : "last connected"}. The node agent is
            connected; sessions are unaffected.
          </p>
        </div>
      )}

      <div className="table-wrap">
        <table className="qtable">
          <thead>
            <tr>
              <th>Service</th>
              <th>Version</th>
              <th>Owner</th>
              <th className="right">State</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <Row key={row.key} row={row} />
            ))}
          </tbody>
        </table>
      </div>
      {services.controlPlaneHere ? (
        <div className="card-pad host-services-foot">
          <p className="hint">
            This machine runs the control plane, so it is not removed from here. To uninstall it,
            run the uninstall command on the machine; it keeps the database, machine state and
            homes unless you ask it to purge.
          </p>
        </div>
      ) : (
        foot && <div className="card-pad host-services-foot host-services-foot-row">{foot}</div>
      )}
    </div>
  );
}

function headHint({ report, reportedAt, lastReportAt }: HostServices): string {
  if (report === "offline" && reportedAt) return `Last report from ${reportClock(reportedAt)}.`;
  if (report === "not_answering" && lastReportAt != null)
    return `Last report from ${reportClock(new Date(lastReportAt).toISOString())}. Nothing here is acted on until the recovery actor answers again.`;
  if (report === "reported")
    return "Each Quasar service has one owner. On this machine every service but the seed is owned by its recovery actor.";
  return "Versions and owners appear once this machine’s recovery actor reports.";
}

const DASH = <span className="host-services-none">—</span>;

function Row({ row }: { row: ServiceRow }) {
  return (
    <tr>
      <td>
        <div className="stack">
          <span className={row.warning ? "primary host-services-warning" : "primary"}>
            {row.name}
          </span>
          <span className="sub host-services-desc">{row.description}</span>
        </div>
      </td>
      <td>
        {row.version || row.versionNote ? (
          <div className="stack">
            {row.version ? (
              <span
                className={
                  row.versionPlain ? "host-services-version" : "num host-services-version"
                }
              >
                {row.version}
              </span>
            ) : (
              DASH
            )}
            {row.versionNote && <span className="sub">{row.versionNote}</span>}
          </div>
        ) : (
          DASH
        )}
      </td>
      <td>{row.owner ?? DASH}</td>
      <td className="right">
        <StateCell state={row.state} />
      </td>
    </tr>
  );
}

function StateCell({ state }: { state: ServiceState }) {
  switch (state.kind) {
    case "running":
      return (
        <Chip variant="success" dot>
          running
        </Chip>
      );
    case "as_of":
      return <Chip title={state.at}>as of {reportClock(state.at)}</Chip>;
    case "absent":
      return <span className="hint">{state.text}</span>;
    case "not_found":
      return <Chip variant="warning">not found</Chip>;
    case "reachable":
      return (
        <Chip variant="success" dot>
          reachable
        </Chip>
      );
    case "must_update":
      return <Chip variant="warning">must update</Chip>;
    case "in_the_way":
      return <Chip variant="warning">in the way</Chip>;
    default:
      return <Chip>unknown</Chip>;
  }
}
