/**
 * The four numbers at the top of the console (mock §A.1): live sessions, GPU
 * slots, hosts, users. Each card is a button to the rows behind it.
 *
 * It computes nothing. `deriveKpis` owns every definition (what "active" means
 * for a user, whose bitrate is summed, which hosts' slots count).
 */

import { useNavigate } from "react-router-dom";
import type { Kpis } from "../../../lib/fleet/deriveKpis";
import { seriesFor, type Series } from "./history";
import { Trend } from "./Trend";

/** History keys the page samples for these four cards. */
export const KPI_SERIES = {
  live: "kpi:live",
  slots: "kpi:slots",
  hosts: "kpi:hosts",
  users: "kpi:users",
} as const;

export function KpiRow({ kpis, series }: { kpis: Kpis; series: Series }) {
  const { sessions, slots, hosts, users } = kpis;

  return (
    <div className="grid g4 kpi-strip">
      <KpiCard
        eyebrow="Live sessions"
        value={sessions.live}
        meta={`${sessions.degraded} degraded · ${sessions.mbpsOut} Mb/s out`}
        points={seriesFor(series, KPI_SERIES.live)}
        color="var(--success)"
        to="/admin/sessions"
      />
      <KpiCard
        eyebrow="GPU slots"
        value={slots.used}
        unit={`/ ${slots.total}`}
        meta={`${slots.free} free across ${slots.capacityHosts} ${plural(slots.capacityHosts, "host")}`}
        points={seriesFor(series, KPI_SERIES.slots)}
        color="var(--accent)"
        to="/admin/fleet/hosts"
      />
      <KpiCard
        eyebrow="Hosts"
        value={hosts.online}
        unit={`/ ${hosts.total}`}
        meta={hosts.attention > 0 ? `${hosts.attention} need attention` : "all healthy"}
        // The one meta line that is a fault, so the one that is coloured.
        metaDanger={hosts.attention > 0}
        points={seriesFor(series, KPI_SERIES.hosts)}
        color="var(--danger)"
        to="/admin/fleet/hosts"
      />
      <KpiCard
        eyebrow="Users"
        value={users.active}
        unit="active"
        meta={`${users.streaming} streaming now · ${users.pendingInvites} ${plural(users.pendingInvites, "invite")} pending`}
        points={seriesFor(series, KPI_SERIES.users)}
        color="var(--info)"
        to="/admin/people/users"
      />
    </div>
  );
}

interface KpiCardProps {
  eyebrow: string;
  value: number;
  unit?: string;
  meta: string;
  metaDanger?: boolean;
  points: readonly number[];
  color: string;
  to: string;
}

function KpiCard({ eyebrow, value, unit, meta, metaDanger, points, color, to }: KpiCardProps) {
  const navigate = useNavigate();
  return (
    <button type="button" className="card card-pad kpi" onClick={() => navigate(to)}>
      <div className="eyebrow">{eyebrow}</div>
      <div className="kpi-row">
        <div className="kpi-val">
          {value}
          {unit && <span className="kpi-unit">{unit}</span>}
        </div>
        <div className="kpi-trend">
          <Trend points={points} color={color} />
        </div>
      </div>
      <div className="kpi-meta" style={metaDanger ? { color: "var(--danger-text)" } : undefined}>
        {meta}
      </div>
    </button>
  );
}

function plural(n: number, noun: string): string {
  return n === 1 ? noun : `${noun}s`;
}
