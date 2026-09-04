/**
 * One host in the Hosts table (mock §A.4): the collapsed row and its drawer,
 * as two `<tr>`s so their columns line up. The row opens the host on click,
 * so every control inside it stops propagation.
 */

import type { GPUAvailability, Host } from "../../../api/types";
import { ActionsMenu, type ActionsMenuEntry } from "../../../components/ActionsMenu";
import { Bar } from "../../../components/Bar";
import { Chip } from "../../../components/Chip";
import { IconChevronRight } from "../../../components/icons";
import { bytesFromMb } from "../../../lib/format/bytes";
import { relativeTime, relativeTimeCompact } from "../../../lib/format/relativeTime";
import { shortId } from "../../../lib/format/shortId";
import { primaryGpuLabel } from "../../../lib/gpu";
import { HostExpansion } from "./HostExpansion";
import {
  heartbeatTone,
  hostStateDot,
  hostStateLabel,
  percentOf,
  tone,
  utilisation,
} from "./hostDerived";

export interface HostRowProps {
  host: Host;
  gpus: GPUAvailability[] | null | undefined;
  gpuError: string | null;
  expanded: boolean;
  onToggle: () => void;
  onOpen: () => void;
  onConsole: () => void;
  onSettings: () => void;
  onDrain: () => void;
  onResume: () => void;
  onForget: () => void;
  /** A drain or uncordon on this row is in flight. */
  actionPending: boolean;
  /** The last drain/uncordon on this row failed; shown in the drawer. */
  actionError?: string;
  now: number;
}

const TONE_TEXT: Record<string, string> = {
  success: "var(--success-text)",
  warning: "var(--warning-text)",
  danger: "var(--danger-text)",
};

export function HostRow(props: HostRowProps) {
  const { host, gpus, expanded, now } = props;
  const util = utilisation(host, gpus);
  const state = hostStateLabel(host);
  const offline = host.status === "offline";
  const live = host.capacity?.active_sessions ?? 0;

  return (
    <>
      <tr
        className="clickable"
        style={offline ? { opacity: 0.55 } : undefined}
        onClick={props.onOpen}
      >
        <td className="qtable-expand-cell">
          <button
            type="button"
            className="exp-btn"
            aria-expanded={expanded}
            aria-label={`Show capacity and storage for ${host.node_name}`}
            title="Show capacity and storage"
            onClick={(e) => {
              e.stopPropagation();
              props.onToggle();
            }}
          >
            <IconChevronRight />
          </button>
        </td>

        <td>
          <div className="rowflex">
            <i className={`sdot ${hostStateDot(host)}`} title={state} />
            <span className="primary">{host.node_name}</span>
            {/* #429: an agent that keeps restarting is worth seeing without
                opening the drawer, where the last-restart time lives. */}
            {host.agent_restart_count > 0 && (
              <Chip
                variant="warning"
                className="chip-sm"
                title={
                  host.agent_last_restart_at
                    ? `Agent restarted ${host.agent_restart_count} time(s), last ${relativeTime(
                        host.agent_last_restart_at,
                        now,
                      )}`
                    : `Agent restarted ${host.agent_restart_count} time(s)`
                }
              >
                {host.agent_restart_count} restart{host.agent_restart_count === 1 ? "" : "s"}
              </Chip>
            )}
          </div>
          <div className="sub mono host-row-id" title={host.id}>
            {shortId(host.id)}
            {state !== "online" ? ` · ${state}` : ""}
          </div>
        </td>

        <td>
          <GpuCell gpus={gpus} />
        </td>

        <td className="host-util-cell">
          <div className="u2">
            <Bar
              label="GPU"
              percent={util.gpu ? percentOf(util.gpu.used, util.gpu.total) : 0}
              value={util.gpu ? `${util.gpu.used}/${util.gpu.total}` : "n/a"}
              variant={util.gpu ? tone(percentOf(util.gpu.used, util.gpu.total)) : "default"}
              unknown={!util.gpu}
              hint={util.gpu ? undefined : "No schedulable GPUs reported"}
            />
            <Bar
              label="VRAM"
              percent={util.vram ? percentOf(util.vram.usedMb, util.vram.totalMb) : 0}
              value={
                util.vram
                  ? `${bytesFromMb(util.vram.usedMb)}/${bytesFromMb(util.vram.totalMb)}`
                  : "n/a"
              }
              variant={
                util.vram ? tone(percentOf(util.vram.usedMb, util.vram.totalMb)) : "default"
              }
              unknown={!util.vram}
              hint={util.vram ? undefined : "No VRAM telemetry reported"}
            />
            <Bar
              label="RAM"
              percent={0}
              value="n/a"
              unknown
              hint="The agent does not report host memory use"
            />
            <Bar
              label="DISK"
              percent={util.diskPct ?? 0}
              value={util.diskPct == null ? "n/a" : `${util.diskPct}%`}
              variant={util.diskPct == null ? "default" : tone(util.diskPct)}
              unknown={util.diskPct == null}
              hint={util.diskPct == null ? "No volumes reported" : undefined}
            />
          </div>
        </td>

        <td className="right num">{live}</td>

        <td className="right num" style={{ color: TONE_TEXT[heartbeatTone(host)] }}>
          {host.last_heartbeat_at ? relativeTimeCompact(host.last_heartbeat_at, now) : "Never"}
        </td>

        <td className="cell-actions">
          <ActionsMenu items={menuItems(props)} label={`Actions for ${host.node_name}`} />
        </td>
      </tr>

      {expanded && (
        <tr className="exp-row">
          <td colSpan={7}>
            <HostExpansion
              host={host}
              gpus={gpus}
              gpuError={props.gpuError}
              actionError={props.actionError}
              now={now}
            />
          </td>
        </tr>
      )}
    </>
  );
}

/** The first GPU names the host; the rest are a count, as the mock renders it.
 *  "Reading GPUs" and "no GPUs" are different facts, so they read differently. */
function GpuCell({ gpus }: { gpus: GPUAvailability[] | null | undefined }) {
  if (gpus === undefined) return <span className="sub">…</span>;
  if (gpus === null) return <span className="sub">n/a</span>;
  if (gpus.length === 0) return <span className="sub">No GPUs reported</span>;
  const first = gpus[0];
  return (
    <div className="stack">
      <span>
        {primaryGpuLabel(first.vendor, first.model)}
        {gpus.length > 1 && <span className="sub"> ×{gpus.length}</span>}
      </span>
      <span className="sub">{first.vendor}</span>
    </div>
  );
}

function menuItems(props: HostRowProps): ActionsMenuEntry[] {
  const { host, actionPending } = props;
  const items: ActionsMenuEntry[] = [
    { key: "open", label: "Open host", onClick: props.onOpen },
    { key: "console", label: "Local console", onClick: props.onConsole },
    { key: "settings", label: "Host settings", onClick: props.onSettings },
    { key: "sep", separator: true },
  ];

  if (host.status === "draining") {
    items.push({
      key: "resume",
      label: "Resume scheduling",
      disabled: actionPending,
      onClick: props.onResume,
    });
  } else {
    items.push({
      key: "drain",
      label: "Drain",
      disabled: actionPending || host.status !== "online",
      onClick: props.onDrain,
    });
  }

  // Always enabled: the offline precondition (server 409s otherwise) is
  // explained and actioned in the confirm modal, not gated here (#101).
  items.push({
    key: "forget",
    label: "Remove host",
    variant: "danger",
    onClick: props.onForget,
  });

  return items;
}
