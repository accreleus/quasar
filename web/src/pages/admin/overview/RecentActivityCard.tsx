/**
 * "Recent activity" — the last six audit entries (mock §A.1).
 *
 * Shows the raw action key (`host.drain`), not a prose rewrite: that is the
 * string an operator searches for on the Audit page. Colour comes from the
 * server-derived `severity`, so the two pages cannot classify one entry
 * differently.
 */

import { useNavigate } from "react-router-dom";
import type { AdminActivityItem } from "../../../api/admin";
import { ResourceStates } from "../../../components/ResourceStates";

export interface RecentActivityCardProps {
  items: AdminActivityItem[];
  loading: boolean;
  error: string | null;
}

export function RecentActivityCard({ items, loading, error }: RecentActivityCardProps) {
  const navigate = useNavigate();

  return (
    <div className="card">
      <div className="panel-head">
        <span className="panel-title">Recent activity</span>
        <div className="acts">
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            onClick={() => navigate("/admin/audit")}
          >
            View all
          </button>
        </div>
      </div>

      <ResourceStates loading={loading} error={error} />
      {!loading && items.length === 0 ? (
        <div className="empty">
          <h3>Nothing logged yet</h3>
          <p>Operator and system actions appear here as they happen.</p>
        </div>
      ) : (
        <div className="act-list">
          {items.map((item) => (
            <div className="act-row" key={item.id}>
              <span className="act-time num">{clockTime(item.created_at)}</span>
              <div className="act-body">
                <span className="act-actor">{item.actor_username ?? "system"}</span>{" "}
                <span className="mono" style={{ color: severityColor(item.severity) }}>
                  {item.action}
                </span>{" "}
                <span className="act-target">{targetLabel(item)}</span>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/** Local 24-hour clock time, as the mock's audit column renders it. Local, not
 *  UTC: an operator correlating this with what they just did is reading their
 *  own wall clock. */
export function clockTime(at: string): string {
  const ms = Date.parse(at);
  if (!Number.isFinite(ms)) return "—";
  return new Date(ms).toLocaleTimeString(undefined, {
    hour12: false,
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** `target_type` plus a short id — the full uuid would push the row off the
 *  card, and the Audit page carries it in full. */
export function targetLabel(item: AdminActivityItem): string {
  if (!item.target_id) return item.target_type;
  return `${item.target_type} ${item.target_id.slice(0, 8)}`;
}

function severityColor(severity: AdminActivityItem["severity"]): string {
  if (severity === "err") return "var(--danger-text)";
  if (severity === "warn") return "var(--warning-text)";
  return "var(--text-3)";
}
