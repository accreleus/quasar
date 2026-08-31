/**
 * One row of the Sessions table (handoff §A.2). Figures are the latest
 * `latest_metrics` sample, read through the fleet accessors so this row and the
 * Overview's Live sessions card cannot disagree. A missing reading renders "—",
 * never 0: a session that has not reported is not a session at zero fps.
 */

import type { AdminSession } from "../../../api/types";
import { ActionsMenu } from "../../../components/ActionsMenu";
import { codecDisplayName, normaliseCodec } from "../../../lib/codecDisplay";
import { isDegraded } from "../../../lib/fleet/deriveAlerts";
import { agentMetrics, browserMetrics } from "../../../lib/fleet/sessionMetrics";
import { bitrate } from "../../../lib/format/bitrate";
import { durationBetween } from "../../../lib/format/duration";
import { shortHost } from "../overview/LiveSessionsCard";
import { Trend } from "../overview/Trend";

/** Above this a stream is not comfortable, so the cell says so in red (§A.2). */
const LATENCY_WARN_MS = 50;

/** `.sdot` tone for a session. Health outranks lifecycle: a running session
 *  the health evaluator has flagged is a warning dot, not a green one. */
export function sessionDotClass(session: AdminSession): string {
  if (session.state === "failed") return "bad";
  if (isDegraded(session)) return "warn";
  if (session.state === "running") return "ok";
  if (session.state === "stopped") return "off";
  return "info";
}

export interface SessionRowProps {
  session: AdminSession;
  /** fps history collected across polls; fewer than two points draws no line. */
  points: readonly number[];
  /** The poll's clock, so every duration in the table agrees. */
  now: number;
  onOpen: () => void;
  onTerminate: () => void;
  /** False for a terminal session — there is nothing left to stop. */
  terminable: boolean;
  terminating: boolean;
}

export function SessionRow({
  session,
  points,
  now,
  onOpen,
  onTerminate,
  terminable,
  terminating,
}: SessionRowProps) {
  const degraded = isDegraded(session);
  const fps = browserMetrics(session)?.fps;
  const rtt = browserMetrics(session)?.rtt_ms;
  const kbps = agentMetrics(session)?.bitrate_kbps;
  const codec = codecDisplayName(normaliseCodec(session.negotiated_codec) ?? session.stream?.codec);
  const ran = durationBetween(session.started_at, session.ended_at, now);
  const trendColor = degraded ? "var(--warning)" : "var(--success)";

  return (
    <tr
      className="clickable"
      onClick={onOpen}
      style={session.state === "stopped" || session.state === "failed" ? { opacity: 0.62 } : undefined}
    >
      {/* Every identity cell falls back to a truncated id and carries the full
          uuid as its title: a session whose app or user row was deleted stays
          identifiable, and any cell can be read back for log correlation. */}
      <td title={session.id}>
        <div className="rowflex">
          <i className={`sdot ${sessionDotClass(session)}`} title={session.state} />
          <div className="stack">
            <span className="primary">{session.app_name ?? session.app_id.slice(0, 8)}</span>
            <span className="sub mono">
              {session.id.slice(0, 8)}
              {session.state !== "running" && ` · ${session.state}`}
            </span>
          </div>
        </div>
      </td>

      <td title={session.user_id}>
        {session.username ? (
          <div className="stack">
            <span className="primary">{session.username}</span>
            <span className="sub mono">{session.user_id.slice(0, 8)}</span>
          </div>
        ) : (
          <span className="cell-id">{session.user_id.slice(0, 8)}</span>
        )}
      </td>

      {/* The mock stacks a GPU name under the host. The wire carries no GPU on a
          session (control-api.md §Sessions), so the sub-line is omitted rather
          than filled with a guess. */}
      <td title={session.host_id ?? undefined}>{shortHost(session.host_name)}</td>

      <td className="right num" style={degraded ? { color: "var(--warning-text)" } : undefined}>
        {fps === undefined ? "—" : Math.round(fps)}
      </td>

      <td style={{ width: 84 }}>
        {/* No line for a session that is not producing frames — an empty box is
            honest, a flat line at zero is not. */}
        {fps !== undefined && fps > 0 ? <Trend points={points} color={trendColor} /> : null}
      </td>

      <td
        className="right num"
        style={rtt !== undefined && rtt > LATENCY_WARN_MS ? { color: "var(--danger-text)" } : undefined}
      >
        {rtt === undefined ? "—" : `${Math.round(rtt)} ms`}
      </td>

      <td className="right num">{bitrate(kbps)}</td>

      <td>{codec ? <span className="cell-id">{codec}</span> : <span className="sub">—</span>}</td>

      <td className="right num">{ran || "—"}</td>

      {/* .cell-actions is display:flex, which takes a <td> out of the table box
          model — it goes on a wrapper, never on the cell. */}
      <td className="right" onClick={(e) => e.stopPropagation()}>
        <div className="cell-actions">
          <ActionsMenu
            label={`Actions for ${session.app_name ?? session.id.slice(0, 8)}`}
            items={[
              { key: "open", label: "Open", onClick: onOpen },
              ...(terminable
                ? [
                    {
                      key: "terminate",
                      label: terminating ? "Terminating…" : "Terminate",
                      variant: "danger" as const,
                      disabled: terminating,
                      onClick: onTerminate,
                    },
                  ]
                : []),
            ]}
          />
        </div>
      </td>
    </tr>
  );
}
