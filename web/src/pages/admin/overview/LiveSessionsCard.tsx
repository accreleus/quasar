/**
 * "Live sessions" — what is streaming right now (mock §A.1).
 *
 * Rows are the shared live poll (`state=active`), so this table and the rail's
 * live badge cannot disagree. Each figure is the latest sample, not an
 * average; where the wire says nothing the cell says "—", because a session
 * that has not reported yet is not a session at zero fps.
 */

import { useNavigate } from "react-router-dom";
import type { AdminSession } from "../../../api/types";
import { IconChevronRight } from "../../../components/icons";
import { ResourceStates } from "../../../components/ResourceStates";
import { isDegraded } from "../../../lib/fleet/deriveAlerts";
import { agentMetrics, browserMetrics } from "../../../lib/fleet/sessionMetrics";
import { bitrate } from "../../../lib/format/bitrate";
import { durationBetween } from "../../../lib/format/duration";
import { seriesFor, type Series } from "./history";
import { Trend } from "./Trend";

/** Above this a stream is not comfortable, so the cell says so in red. */
const LATENCY_WARN_MS = 50;

/** History key for one session's fps series. Exported so the page can sample
 *  the same key it later reads. */
export function fpsSeriesKey(sessionId: string): string {
  return `fps:${sessionId}`;
}

export interface LiveSessionsCardProps {
  sessions: AdminSession[];
  series: Series;
  loading: boolean;
  error: string | null;
  /** The poll's clock, so every duration on the page agrees. */
  now: number;
}

export function LiveSessionsCard({ sessions, series, loading, error, now }: LiveSessionsCardProps) {
  const navigate = useNavigate();

  return (
    <div className="card">
      <div className="panel-head">
        <span className="live-dot" aria-hidden="true" />
        <span className="panel-title">Live sessions</span>
        <div className="acts">
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            onClick={() => navigate("/admin/sessions")}
          >
            All sessions <IconChevronRight />
          </button>
        </div>
      </div>

      <div className="table-wrap">
        <ResourceStates loading={loading} error={error} />
        {!loading && sessions.length === 0 ? (
          <div className="empty">
            <h3>No live sessions</h3>
            <p>Sessions users start appear here while they run.</p>
          </div>
        ) : (
          <table className="qtable">
            <thead>
              <tr>
                <th>Session</th>
                <th>Host</th>
                <th className="right">FPS</th>
                <th>Trend</th>
                <th className="right">Latency</th>
                <th className="right">Bitrate</th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((session) => (
                <SessionRow
                  key={session.id}
                  session={session}
                  points={seriesFor(series, fpsSeriesKey(session.id))}
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
  points,
  now,
  onOpen,
}: {
  session: AdminSession;
  points: readonly number[];
  now: number;
  onOpen: () => void;
}) {
  const degraded = isDegraded(session);
  const fps = browserMetrics(session)?.fps;
  const rtt = browserMetrics(session)?.rtt_ms;
  const kbps = agentMetrics(session)?.bitrate_kbps;
  const ran = durationBetween(session.started_at, null, now);

  return (
    <tr className="clickable" onClick={onOpen}>
      <td>
        <div className="stack">
          <span className="primary">{session.app_name ?? "Unnamed app"}</span>
          <span className="sub">
            {session.username ?? "unknown user"}
            {ran && ` · ${ran}`}
          </span>
        </div>
      </td>
      <td>{shortHost(session.host_name)}</td>
      <td className="right num" style={degraded ? { color: "var(--warning-text)" } : undefined}>
        {fps === undefined ? "—" : Math.round(fps)}
      </td>
      <td className="ov-trend-cell">
        <Trend points={points} color={degraded ? "var(--warning)" : "var(--success)"} />
      </td>
      <td
        className="right num"
        style={rtt !== undefined && rtt > LATENCY_WARN_MS ? { color: "var(--danger-text)" } : undefined}
      >
        {rtt === undefined ? "—" : `${Math.round(rtt)} ms`}
      </td>
      <td className="right num">{bitrate(kbps)}</td>
    </tr>
  );
}

/** Hosts are conventionally named `quasar-node-N`; the prefix is the same on
 *  every row, so it is noise in a narrow column (mock §A.1). */
export function shortHost(name: string | undefined): string {
  if (!name) return "unassigned";
  return name.replace(/^quasar-/, "");
}
