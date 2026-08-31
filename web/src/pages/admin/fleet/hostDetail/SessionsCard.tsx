/**
 * "Sessions on this host" (mock §A.5): the shared live poll, filtered to one
 * host. Each figure is the latest sample; where the wire says nothing the cell
 * says "—", because a session that has not reported yet is not one at zero fps.
 */

import { useNavigate } from "react-router-dom";
import type { AdminSession } from "../../../../api/types";
import { Chip } from "../../../../components/Chip";
import { browserMetrics } from "../../../../lib/fleet/sessionMetrics";
import { durationBetween } from "../../../../lib/format/duration";

/** Above this a stream is not comfortable, so the cell says so in red. */
const LATENCY_WARN_MS = 50;

export function SessionsCard({ sessions, now }: { sessions: AdminSession[]; now: number }) {
  const navigate = useNavigate();

  return (
    <div className="card">
      <div className="panel-head">
        <span className="panel-title">Sessions on this host</span>
        <div className="acts">
          <Chip>{sessions.length}</Chip>
        </div>
      </div>
      <div className="table-wrap">
        {sessions.length === 0 ? (
          <div className="empty">
            <h3>No sessions</h3>
            <p>This host is not running anything right now.</p>
          </div>
        ) : (
          <table className="qtable">
            <thead>
              <tr>
                <th>App</th>
                <th>User</th>
                <th>Codec</th>
                <th>State</th>
                <th className="right">FPS</th>
                <th className="right">Latency</th>
                <th className="right">Duration</th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((session) => (
                <SessionRow
                  key={session.id}
                  session={session}
                  now={now}
                  onOpen={() => navigate(`/admin/sessions/${session.id}`)}
                />
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

function SessionRow({
  session,
  now,
  onOpen,
}: {
  session: AdminSession;
  now: number;
  onOpen: () => void;
}) {
  const fps = browserMetrics(session)?.fps;
  const rtt = browserMetrics(session)?.rtt_ms;

  return (
    <tr className="clickable" onClick={onOpen}>
      <td className="primary">{session.app_name ?? "Unnamed app"}</td>
      <td>{session.username ?? "unknown user"}</td>
      <td>
        <span className="cell-id">{session.stream?.codec ?? "—"}</span>
      </td>
      <td>
        <Chip variant={session.state === "running" ? "success" : "neutral"} className="chip-sm">
          {session.state}
        </Chip>
      </td>
      <td className="right num">{fps === undefined ? "—" : Math.round(fps)}</td>
      <td
        className="right num"
        style={
          rtt !== undefined && rtt > LATENCY_WARN_MS ? { color: "var(--danger-text)" } : undefined
        }
      >
        {rtt === undefined ? "—" : `${Math.round(rtt)} ms`}
      </td>
      <td className="right num">{durationBetween(session.started_at, null, now) || "—"}</td>
    </tr>
  );
}
