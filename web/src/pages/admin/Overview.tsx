/**
 * `/admin` — the console's landing page (spec §5.6, mock §A.1).
 *
 * Opens no hosts/sessions poll: both come from `FleetContext`, everything else
 * from one 30 s resource (./overview/useOverviewData). No 1h/24h/7d range —
 * nothing serves one (spec §9).
 */
import { useEffect, useMemo, useState } from "react";
import { PageHeader } from "../../components/PageHeader";
import { IconRefresh } from "../../components/icons";
import { deriveAlerts } from "../../lib/fleet/deriveAlerts";
import { deriveKpis } from "../../lib/fleet/deriveKpis";
import { useFleetContext } from "../../lib/fleet/FleetContext";
import { browserMetrics } from "../../lib/fleet/sessionMetrics";
import { relativeTime } from "../../lib/format/relativeTime";
import { AttentionCard } from "./overview/AttentionCard";
import { FleetCapacityCard } from "./overview/FleetCapacityCard";
import { KPI_SERIES, KpiRow } from "./overview/KpiRow";
import { LiveSessionsCard, fpsSeriesKey } from "./overview/LiveSessionsCard";
import { RecentActivityCard } from "./overview/RecentActivityCard";
import { useSeries, type Series } from "./overview/history";
import { useOverviewData } from "./overview/useOverviewData";

/** Re-render cadence for the head's "N seconds ago"; the data has its own timers. */
const CLOCK_MS = 1000;
const MINUTE_MS = 60_000;

export function Overview() {
  const fleet = useFleetContext();
  const overview = useOverviewData();
  const now = useNow();
  // The derivations read ages in minutes (a 24 h user window, an alert's "14m"),
  // so keying them on the second would recompute every card once a second to
  // redraw the same string.
  const coarseNow = Math.floor(now / MINUTE_MS) * MINUTE_MS;

  const kpis = useMemo(
    () =>
      deriveKpis({
        hosts: fleet.hosts,
        sessions: fleet.sessions,
        users: overview.data.users,
        pendingInvites: overview.data.pendingInvites,
        now: coarseNow,
      }),
    [fleet.hosts, fleet.sessions, overview.data.users, overview.data.pendingInvites, coarseNow],
  );

  // The shared poll carries only non-terminal sessions, so the "failed
  // recently" rule needs the failures handed to it explicitly.
  const alerts = useMemo(
    () => deriveAlerts(fleet.hosts, [...fleet.sessions, ...overview.data.recentFailed], coarseNow),
    [fleet.hosts, fleet.sessions, overview.data.recentFailed, coarseNow],
  );

  // Two buffers, because the two sources tick at different rates: sampling the
  // user count on the 5 s fleet stamp would draw six copies of every 30 s read.
  const fleetSeries = useSeries(
    {
      [KPI_SERIES.live]: kpis.sessions.live,
      [KPI_SERIES.slots]: kpis.slots.used,
      [KPI_SERIES.hosts]: kpis.hosts.online,
      ...Object.fromEntries(fleet.sessions.map((s) => [fpsSeriesKey(s.id), browserMetrics(s)?.fps])),
    },
    fleet.lastFetchedAt,
  );
  const userSeries = useSeries(
    { [KPI_SERIES.users]: kpis.users.active },
    overview.lastFetchedAt,
  );
  const series: Series = { ...fleetSeries, ...userSeries };

  const refresh = () => {
    void Promise.all([fleet.reload(), overview.reload()]);
  };

  return (
    <section className="page overview-page">
      <PageHeader
        title="Overview"
        sub={
          fleet.lastFetchedAt
            ? `Live fleet state · updated ${relativeTime(fleet.lastFetchedAt, now)}`
            : "Live fleet state"
        }
        actions={
          <button type="button" className="btn btn-ghost" onClick={refresh}>
            <IconRefresh />
            Refresh
          </button>
        }
      />

      <KpiRow kpis={kpis} series={series} />

      <div className="split even ov-row">
        <LiveSessionsCard
          sessions={fleet.sessions}
          series={series}
          loading={fleet.loading}
          error={fleet.errors.sessions}
          now={now}
        />
        <AttentionCard
          alerts={alerts}
          loading={fleet.loading || overview.loading}
          error={fleet.errors.hosts ?? fleet.errors.sessions ?? overview.error}
        />
      </div>

      <div className="split even">
        <FleetCapacityCard
          hosts={fleet.hosts}
          slots={kpis.slots}
          loading={fleet.loading}
          error={fleet.errors.hosts}
          now={now}
        />
        <RecentActivityCard
          items={overview.data.activity}
          loading={overview.loading}
          error={overview.error}
        />
      </div>
    </section>
  );
}

/** Wall clock, ticking, so ages age. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), CLOCK_MS);
    return () => clearInterval(id);
  }, []);
  return now;
}

