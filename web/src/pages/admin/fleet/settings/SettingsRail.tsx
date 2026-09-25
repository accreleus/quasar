// Right rail (handoff-v3-spec §A.7): Host card (identity, state, agent facts,
// restart action) + Overrides card (count).

import type { Host } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { Chip } from "../../../../components/Chip";
import { relativeTimeCompact } from "../../../../lib/format/relativeTime";

function hostStatusVariant(status: Host["status"]): "success" | "warning" | "danger" {
  return status === "online" ? "success" : status === "draining" ? "warning" : "danger";
}

export function SettingsRail({
  host,
  changedCount,
  liveSessionsCount,
  pendingRestart,
  restarting,
  onRestart,
}: {
  host: Host | null;
  changedCount: number;
  liveSessionsCount: number;
  pendingRestart: boolean;
  restarting: boolean;
  onRestart: () => void;
}) {
  return (
    <div className="col gap4">
      <div className="card card-pad">
        <div className="eyebrow">Host</div>
        <h3 className="t-h3 mt2">{host?.node_name ?? "Unknown host"}</h3>
        <div className="mono muted t-xs mt1">
          {host?.id}
        </div>
        {host && (
          <div className="mt3">
            <Chip variant={hostStatusVariant(host.status)} dot>{host.status}</Chip>
          </div>
        )}
        <div className="col gap2 mt5 t-sm rail-facts">
          <div className="row between">
            <span className="hint">Agent</span>
            <span className="mono">{host?.agent_version ?? "—"}</span>
          </div>
          <div className="row between">
            <span className="hint">Heartbeat</span>
            <span className="mono">
              {host?.last_heartbeat_at ? relativeTimeCompact(host.last_heartbeat_at) : "—"}
            </span>
          </div>
          <div className="row between">
            <span className="hint">Live sessions</span>
            <span className="mono">{liveSessionsCount}</span>
          </div>
          <div className="row between">
            <span className="hint">Pending restart</span>
            {pendingRestart ? <Chip variant="warning">yes</Chip> : <span className="mono">no</span>}
          </div>
        </div>
        <Button
          variant="secondary"
          size="sm"
          disabled={restarting}
          onClick={onRestart}
          className="mt4"
          style={{ width: "100%" }}
        >
          {restarting ? "Restarting…" : "Restart agent now"}
        </Button>
      </div>

      <div className="card card-pad">
        <div className="eyebrow">Overrides</div>
        <div className="rail-stat">
          {changedCount}
        </div>
        <div className="hint">Set on this host. Everything else follows the instance default.</div>
      </div>
    </div>
  );
}
