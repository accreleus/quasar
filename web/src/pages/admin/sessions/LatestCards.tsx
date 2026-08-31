/**
 * "Agent latest" / "Browser latest" (handoff §A.3): the newest sample from each
 * side with its own age. The mock's rows (encoder, capture, decoder, nack/pli)
 * have no `latest_metrics` field — encoder identity lives in the diagnostic
 * bundle's effective-media snapshot, shown under Diagnostics.
 */

import type { AgentMetrics, BrowserMetrics, MetricPoint } from "../../../api/types";
import { relativeTimeCompact } from "../../../lib/format/relativeTime";

type Row = [label: string, value: string];

function num(n: number | undefined, unit = "", digits = 1): string {
  if (n === undefined || !Number.isFinite(n)) return "—";
  return `${n.toFixed(digits)}${unit}`;
}

function LatestCard({ title, at, now, rows }: { title: string; at: number | null; now: number; rows: Row[] }) {
  return (
    <div className="card">
      <div className="panel-head">
        <span className="panel-title">{title}</span>
        <span className="hint" style={{ marginLeft: "auto" }}>
          {at === null ? "no sample yet" : `${relativeTimeCompact(at, now)} ago`}
        </span>
      </div>
      <div className="table-wrap">
        <table className="qtable">
          <tbody>
            {rows.map(([label, value]) => (
              <tr key={label}>
                <td style={{ color: "var(--text-3)" }}>{label}</td>
                <td className="right num primary">{value}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export function LatestCards({
  agent,
  browser,
  now,
}: {
  agent: MetricPoint | undefined;
  browser: MetricPoint | undefined;
  now: number;
}) {
  const a = agent?.metrics as AgentMetrics | undefined;
  const b = browser?.metrics as BrowserMetrics | undefined;

  return (
    <div className="grid g2">
      <LatestCard
        title="Agent latest"
        at={agent?.ts_unix_ms ?? null}
        now={now}
        rows={[
          ["frame rate", num(a?.fps, " fps", 0)],
          ["bitrate", num(a?.bitrate_kbps === undefined ? undefined : a.bitrate_kbps / 1000, " Mb/s")],
          ["encode p50", num(a?.encode_ms_p50 ?? a?.encode_ms, " ms")],
          ["encode p95", num(a?.encode_ms_p95, " ms")],
          ["dropped frames", num(a?.frames_dropped, "", 0)],
        ]}
      />
      <LatestCard
        title="Browser latest"
        at={browser?.ts_unix_ms ?? null}
        now={now}
        rows={[
          ["frame rate", num(b?.fps, " fps", 0)],
          ["round-trip", num(b?.rtt_ms, " ms", 0)],
          ["jitter buffer", num(b?.jitter_buffer_ms, " ms")],
          ["decode time", num(b?.decode_ms, " ms")],
          ["freezes", num(b?.freeze_count, "", 0)],
        ]}
      />
    </div>
  );
}
