// ScanHealth — the census, recent-scans table and provider-app link under
// the Steam row's "Show scan health" toggle.

import { Link } from "react-router-dom";
import type { AdminApp, LibraryRecentScan, LibraryStatus, RuntimePreset } from "../../../api/types";
import { Chip } from "../../../components/Chip";
import { EmptyState, LoadingState } from "../../../components/LoadingState";
import { Table, type TableColumn } from "../../../components/Table";
import { elapsedWords, relativeTime } from "../../../lib/format/relativeTime";

const OUTCOME_COUNT_KEYS = [
  "observed",
  "suppressed",
  "created",
  "disabled",
  "granted",
  "revoked",
  "rejected",
  "backfilled",
] as const;

/** Counts are stored at reconcile (migration 0048): a scan reported before it
 *  reads all-zero, which must render as "not recorded", not "nothing happened". */
export function allCountsZero(row: LibraryRecentScan): boolean {
  return OUTCOME_COUNT_KEYS.every((k) => row[k] === 0);
}

function recentScanTitle(row: LibraryRecentScan): string {
  return (
    `granted ${row.granted} · revoked ${row.revoked} · rejected ${row.rejected}` +
    (row.state === "failed" && row.error ? ` — ${row.error}` : "")
  );
}

export const recentScanColumns: TableColumn<LibraryRecentScan>[] = [
  {
    key: "completed_at",
    header: "Time",
    render: (r) => (
      <span title={r.completed_at} style={{ fontSize: "var(--t-sm)" }}>
        {relativeTime(r.completed_at)}
      </span>
    ),
  },
  {
    key: "user",
    header: "User",
    render: (r) => <span style={{ fontSize: "var(--t-sm)" }}>{r.user}</span>,
  },
  {
    key: "host",
    header: "Host",
    render: (r) => <span className="mono" style={{ fontSize: "var(--t-sm)" }}>{r.host}</span>,
  },
  {
    key: "state",
    header: "Result",
    render: (r) => (
      <Chip variant={r.state === "reported" ? "success" : "danger"} title={recentScanTitle(r)}>
        {r.state === "reported" ? "Reported" : "Failed"}
      </Chip>
    ),
  },
  {
    key: "counts",
    header: "Observed · suppressed · created · disabled",
    render: (r) => (
      <span className="muted mono" style={{ fontSize: "var(--t-sm)" }} title={recentScanTitle(r)}>
        {r.observed} · {r.suppressed} · {r.created} · {r.disabled}
      </span>
    ),
  },
  {
    key: "backfilled",
    header: "Backfilled",
    render: (r) => <span className="mono" style={{ fontSize: "var(--t-sm)" }}>{r.backfilled}</span>,
  },
];

export interface ScanHealthProps {
  status: LibraryStatus;
  steamApp: AdminApp | null;
  preset: RuntimePreset | null;
  presetLoading: boolean;
}

export function ScanHealth({ status, steamApp, preset, presetLoading }: ScanHealthProps) {
  return (
    <div className="col gap5" style={{ marginTop: "var(--s4)" }}>
      <div className="row gap6 wrap">
        <div>
          <span className="hint">Last scan completed</span>
          <div className="muted" style={{ fontSize: "var(--t-sm)" }}>
            {status.last_scan_completed_at ? (
              <span title={status.last_scan_completed_at}>
                {elapsedWords(status.last_scan_completed_at)} ago (
                {new Date(status.last_scan_completed_at).toLocaleString()})
              </span>
            ) : (
              "Never"
            )}
          </div>
        </div>
        {(["pending", "claimed", "reported", "failed"] as const).map((k) => (
          <div key={k}>
            <span className="hint" style={{ textTransform: "capitalize" }}>{k}</span>
            <div className="muted mono" style={{ fontSize: "var(--t-sm)" }}>{status.scans[k]}</div>
          </div>
        ))}
      </div>

      <div>
        <span className="hint">Recent scans</span>
        <Table<LibraryRecentScan>
          columns={recentScanColumns}
          rows={status.recent_scans}
          rowKey={(r) => `${r.completed_at}-${r.user}-${r.host}`}
          empty={<span className="muted">No scans have run yet.</span>}
        />
        {status.recent_scans.some((r) => r.state === "reported" && allCountsZero(r)) && (
          <p className="hint mt2">Scans from before this upgrade show no recorded counts.</p>
        )}
      </div>

      <div>
        <span className="hint">Provider app</span>
        <div style={{ fontSize: "var(--t-sm)", marginTop: 4 }}>
          {steamApp === null ? (
            <EmptyState>
              No app is marked as the Steam library provider yet. Set{" "}
              <Link to="/admin/library/apps">Library provider</Link> to Steam on an app's editor to
              give it one.
            </EmptyState>
          ) : presetLoading ? (
            <LoadingState />
          ) : steamApp.runtime_preset_id ? (
            <p>
              Uses the runtime preset{" "}
              <Link to="/admin/library/presets">{preset?.name ?? steamApp.runtime_preset_id}</Link>.
            </p>
          ) : (
            <p>
              Uses an inline runtime spec, not a shared preset.{" "}
              <Link to={`/admin/library/apps/${steamApp.id}`}>Open the app editor</Link> to extract it
              to a reusable preset.
            </p>
          )}
        </div>
      </div>

      <p className="hint">
        Scan cadence and app-details lookup are set in <Link to="/admin/settings">Settings › Libraries</Link>.
      </p>
    </div>
  );
}
