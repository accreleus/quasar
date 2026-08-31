/**
 * "Fleet capacity" — where the slots are and how full they are (mock §A.1).
 *
 * Both bars read `Host.capacity`, the read-time roll-up from the v3 amendment
 * (spec §6.2): one request for the fleet, no GPU fan-out per host.
 *
 * A null `capacity` means no schedulable GPUs were reported, not zero
 * capacity, so those rows draw the `unknown` bar (no fill, #383). An empty bar
 * would read as "idle" for the host most worth opening.
 */

import { useNavigate } from "react-router-dom";
import type { Host } from "../../../api/types";
import { Bar } from "../../../components/Bar";
import { Chip } from "../../../components/Chip";
import { IconChevronRight } from "../../../components/icons";
import { ResourceStates } from "../../../components/ResourceStates";
import { bytesFromMb } from "../../../lib/format/bytes";
import { relativeTimeCompact } from "../../../lib/format/relativeTime";
import { hostStateChip, hostStateLabel, tone } from "../fleet/hostDerived";

export interface FleetCapacityCardProps {
  hosts: Host[];
  /** Fleet totals, already summed by `deriveKpis` — the chip must not do its
   *  own arithmetic and disagree with the KPI card above it. */
  slots: { used: number; total: number };
  loading: boolean;
  error: string | null;
  now: number;
}

export function FleetCapacityCard({ hosts, slots, loading, error, now }: FleetCapacityCardProps) {
  const navigate = useNavigate();

  return (
    <div className="card">
      <div className="panel-head">
        <span className="panel-title">Fleet capacity</span>
        <div className="acts">
          <Chip>
            {slots.used}/{slots.total} slots
          </Chip>
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            onClick={() => navigate("/admin/fleet/hosts")}
          >
            Hosts <IconChevronRight />
          </button>
        </div>
      </div>

      <div className="table-wrap">
        <ResourceStates loading={loading} error={error} />
        {!loading && hosts.length === 0 ? (
          <div className="empty">
            <h3>No hosts enrolled</h3>
            <p>Enroll a host to give the fleet somewhere to run sessions.</p>
          </div>
        ) : (
          <table className="qtable">
            <thead>
              <tr>
                <th>Host</th>
                <th>Capacity</th>
                <th className="right">Sessions</th>
                <th className="right">Heartbeat</th>
              </tr>
            </thead>
            <tbody>
              {hosts.map((host) => (
                <HostRow
                  key={host.id}
                  host={host}
                  now={now}
                  onOpen={() => navigate(`/admin/fleet/hosts/${host.id}`)}
                />
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

function HostRow({ host, now, onOpen }: { host: Host; now: number; onOpen: () => void }) {
  const capacity = host.capacity;
  const online = host.status === "online";
  const slots = share(capacity?.slots_used, capacity?.slots_total);
  const vram = share(capacity?.vram_mb_used, capacity?.vram_mb_total);

  return (
    <tr className="clickable" onClick={onOpen}>
      <td>
        <div className="rowflex">
          <span className="primary">{host.node_name}</span>
          <Chip variant={hostStateChip(host)} className="chip-sm">
            {hostStateLabel(host)}
          </Chip>
        </div>
        <div className="sub">{gpuSummary(host)}</div>
      </td>
      <td className="ov-cap-cell">
        <Bar
          label="SLOTS"
          percent={slots}
          value={capacity ? `${capacity.slots_used}/${capacity.slots_total}` : "—"}
          variant={capacity ? tone(slots) : "default"}
          unknown={!capacity}
          hint={capacity ? undefined : NO_CAPACITY}
        />
        <Bar
          label="VRAM"
          percent={vram}
          value={
            capacity
              ? `${bytesFromMb(capacity.vram_mb_used)}/${bytesFromMb(capacity.vram_mb_total)}`
              : "—"
          }
          variant={capacity ? tone(vram) : "default"}
          unknown={!capacity}
          hint={capacity ? undefined : NO_CAPACITY}
        />
      </td>
      <td className="right num">{capacity ? capacity.active_sessions : "—"}</td>
      <td className="right">
        <span className="num" style={{ color: online ? "var(--success-text)" : "var(--text-3)" }}>
          {host.last_heartbeat_at ? relativeTimeCompact(host.last_heartbeat_at, now) : "Never"}
        </span>
      </td>
    </tr>
  );
}

const NO_CAPACITY = "No schedulable GPUs reported";

/** The wire has no GPU model names on `GET /v1/hosts` — only how many were
 *  summed — so the sub-line counts them rather than fetching a name per host. */
function gpuSummary(host: Host): string {
  const count = host.capacity?.gpu_count ?? 0;
  if (count === 0) return "No GPUs reported";
  return `${count} GPU${count === 1 ? "" : "s"}`;
}

function share(used: number | undefined, total: number | undefined): number {
  if (!total || used === undefined) return 0;
  return (used / total) * 100;
}
