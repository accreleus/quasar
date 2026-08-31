/**
 * "Needs attention" — the fleet's faults, off the same two polls the rest of
 * the page draws (mock §A.1).
 *
 * No alerts entity backs this: `deriveAlerts` reads the current hosts and
 * sessions, so a row cannot outlive the condition it describes or need
 * acknowledging.
 */

import { useNavigate } from "react-router-dom";
import { IconChevronRight } from "../../../components/icons";
import { ResourceStates } from "../../../components/ResourceStates";
import type { Alert } from "../../../lib/fleet/deriveAlerts";

export interface AttentionCardProps {
  alerts: Alert[];
  /** True until every source behind an alert rule has loaded. */
  loading: boolean;
  /** First failure across those sources. */
  error: string | null;
}

export function AttentionCard({ alerts, loading, error }: AttentionCardProps) {
  const navigate = useNavigate();
  const critical = alerts.filter((a) => a.severity === "critical").length;
  const warning = alerts.length - critical;
  // "Nothing needs attention" is a claim about the fleet. An unresolved or
  // failed read cannot make it, and making it anyway hides the outage.
  const resolved = !loading && !error;

  return (
    <div className="card">
      <div className="panel-head">
        <span className="panel-title">Needs attention</span>
        <div className="acts">
          <span className="chip chip-danger">{critical} critical</span>
          <span className="chip chip-warning">{warning} warning</span>
        </div>
      </div>

      <ResourceStates loading={loading} error={error} />
      {alerts.length === 0 && resolved ? (
        <div className="empty">
          <h3>Nothing needs attention</h3>
          <p>Hosts, sessions and storage are healthy.</p>
        </div>
      ) : (
        <div>
          {alerts.map((alert) => (
            <div className="alert-row" key={alert.id}>
              <span
                className={`alert-ico ${alert.severity === "critical" ? "danger" : "warning"}`}
                aria-hidden="true"
              >
                <AlertGlyph />
              </span>
              <div className="alert-body">
                <div className="alert-title">{alert.title}</div>
                <div className="alert-detail">{alert.body}</div>
              </div>
              <div className="alert-act">
                <span className="alert-age">{alert.age}</span>
                <button type="button" className="btn btn-sm" onClick={() => navigate(alert.to)}>
                  {alert.cta}
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      <div className="alert-foot">
        <button
          type="button"
          className="btn btn-sm btn-ghost"
          onClick={() => navigate("/admin/audit")}
        >
          Open audit log <IconChevronRight />
        </button>
      </div>
    </div>
  );
}

/** The mock's `icon('alert')`: a warning triangle. */
function AlertGlyph() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <path d="M8 2.6l6 10.8H2z" strokeLinejoin="round" />
      <path d="M8 6.6v3M8 11.4h.01" strokeLinecap="round" />
    </svg>
  );
}
