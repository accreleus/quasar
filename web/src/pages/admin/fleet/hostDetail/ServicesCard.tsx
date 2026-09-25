/**
 * "Services on this machine" for an owned GPU host (design_handoff_v3
 * fleet-rh06-v3.html, `rhServices`; screenshots rh06/inv-gpu, inv-unknown).
 * The rows come from hostServices.ts; this file only draws them.
 */

import type { HostServices, ServiceRow, ServiceState } from "../hostServices";
import { Chip } from "../../../../components/Chip";
import { clockTime } from "../../../../lib/format/clockTime";

/** "13:48", as the mock's "as of" chips and "Last report from" read. */
const reportClock = (at: string): string => clockTime(at, { seconds: false });
import { elapsedWords } from "../../../../lib/format/relativeTime";

export interface ServicesCardProps {
  nodeName: string;
  services: HostServices;
  /** When the agent connected, for the "not reported yet" note. */
  connectedSince: string | null;
  now: number;
}

export function ServicesCard({ nodeName, services, connectedSince, now }: ServicesCardProps) {
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
          <Chip>GPU host</Chip>
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
      {report === "not_answering" && (
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
    </div>
  );
}

function headHint({ report, reportedAt }: HostServices): string {
  if (report === "offline" && reportedAt) return `Last report from ${reportClock(reportedAt)}.`;
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
          <span className="primary">{row.name}</span>
          <span className="sub host-services-desc">{row.description}</span>
        </div>
      </td>
      <td>
        {row.version || row.versionNote ? (
          <div className="stack">
            {row.version ? <span className="num host-services-version">{row.version}</span> : DASH}
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
    default:
      return <Chip>unknown</Chip>;
  }
}
