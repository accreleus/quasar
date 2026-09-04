/**
 * The host's "Capacity" card (mock §A.5): a gauge and two bars per GPU, then
 * memory and storage, then the fact table. Readings the wire does not carry
 * say "n/a" instead of drawing a zero.
 */

import type { CSSProperties, ReactNode } from "react";
import { Link } from "react-router-dom";
import type { GPUAvailability, Host, HostStorageVolume } from "../../../../api/types";
import { Bar, Gauge } from "../../../../components/Bar";
import { Chip } from "../../../../components/Chip";
import { bytesFromMb } from "../../../../lib/format/bytes";
import { relativeTime } from "../../../../lib/format/relativeTime";
import { primaryGpuLabel } from "../../../../lib/gpu";
import {
  percentOf,
  schedulingLabel,
  storageTotals,
  tone,
  toneColor,
  uptimeSince,
} from "../hostDerived";
import {
  installModeHint,
  installModeLabel,
  shortCommit,
  updaterHint,
  updaterLabel,
} from "../hostIdentity";

/** A gauge with no reading: the track, and the word for it. Never a 0 % arc,
 *  which would read as a confirmed zero. */
const unknownGauge = { "--p": 0 } as CSSProperties;

export interface CapacityCardProps {
  host: Host;
  /** Null when the GPU read failed; the rest of the card still renders. */
  gpus: GPUAvailability[] | null;
  /** The poll's clock, so every age on the page agrees. */
  now: number;
}

export function CapacityCard({ host, gpus, now }: CapacityCardProps) {
  const volumes = host.storage ?? [];
  const storage = storageTotals(host.storage);
  const usedPct = storage ? percentOf(storage.usedMb, storage.totalMb) : null;
  // The gauge reads the fullest volume, not the average: a host with one full
  // disk and one empty one cannot provision a home, and an average hides that.
  const gaugePct = volumes.length
    ? Math.max(...volumes.map((v) => percentOf(v.total_mb - v.available_mb, v.total_mb)))
    : null;
  const gpuCount = gpus?.length ?? host.capacity?.gpu_count ?? 0;
  const sessionCount = host.capacity?.active_sessions ?? 0;

  return (
    <div className="card host-capacity">
      <div className="panel-head">
        <span className="panel-title">Capacity</span>
        <div className="acts">
          <Chip>
            {gpuCount} GPU{gpuCount === 1 ? "" : "s"}
          </Chip>
          <Chip>
            {sessionCount} session{sessionCount === 1 ? "" : "s"}
          </Chip>
        </div>
      </div>

      <div className="cap-rows">
        {gpus === null && <p className="form-error">Could not load this host's GPUs.</p>}
        {gpus?.length === 0 && <p className="sub">No GPUs reported.</p>}
        {gpus?.map((gpu) => (
          <GpuRow key={gpu.gpu_id} gpu={gpu} />
        ))}

        <div className="cap-row" data-testid="cap-row-memory">
          <span className="gauge" style={unknownGauge}>
            <span>n/a</span>
          </span>
          <div className="cap-detail">
            <div className="rowflex">
              <span className="cap-name">Memory</span>
              <span className="cell-id">
                {host.mem_mb != null ? bytesFromMb(host.mem_mb) : "n/a"}
              </span>
            </div>
            <Bar
              label="USED"
              percent={0}
              value={null}
              unknown
              hint="The agent does not report host memory use"
            />
            <Bar
              label="CPU"
              percent={0}
              value={null}
              unknown
              hint="The agent does not report CPU load"
            />
          </div>
        </div>

        <div className="cap-row" data-testid="cap-row-storage">
          {gaugePct == null ? (
            <span className="gauge" style={unknownGauge}>
              <span>n/a</span>
            </span>
          ) : (
            <Gauge percent={gaugePct} color={toneColor(gaugePct)} />
          )}
          <div className="cap-detail">
            <div className="rowflex">
              <span className="cap-name">Storage</span>
              <span className="cell-id">{volumes[0]?.label ?? "agent-data"}</span>
              <Link className="cap-link" to="/admin/fleet/storage">
                Managed homes
              </Link>
            </div>
            {storage ? (
              <>
                <Bar
                  label="USED"
                  percent={usedPct ?? 0}
                  value={`${bytesFromMb(storage.usedMb)} / ${bytesFromMb(storage.totalMb)}`}
                  variant={tone(usedPct ?? 0)}
                />
                <Bar
                  label="FREE"
                  percent={100 - (usedPct ?? 0)}
                  value={bytesFromMb(storage.totalMb - storage.usedMb)}
                  variant="success"
                />
                {volumes.length > 1 &&
                  volumes.map((volume) => <VolumeBar key={volume.path} volume={volume} />)}
              </>
            ) : (
              <Bar label="USED" percent={0} value={null} unknown hint="No volumes reported" />
            )}
          </div>
        </div>
      </div>

      <div className="table-wrap host-facts">
        <table className="qtable">
          <tbody>
            <Fact
              label="Node ID"
              value={
                <span className="cell-id" data-testid="fact-node-id">
                  {host.id}
                </span>
              }
            />
            <Fact label="CPU" value={cpuLabel(host)} />
            <Fact label="Agent" value={host.agent_version ?? "n/a"} />
            {/* The agent build's identity. Every row reads "Unknown" until an
                amendment-aware agent has registered; for the updater that is a
                different fact from "None" (openapi.yaml Host.updater_present). */}
            <Fact
              label="Commit"
              value={
                host.source_commit ? (
                  <span className="mono" data-testid="fact-source-commit" title={host.source_commit}>
                    {shortCommit(host.source_commit)}
                  </span>
                ) : (
                  "Unknown"
                )
              }
            />
            <Fact
              label="Built"
              value={
                host.built_at ? (
                  <span title={host.built_at}>{relativeTime(host.built_at, now)}</span>
                ) : (
                  "Unknown"
                )
              }
            />
            <Fact
              label="Install"
              value={
                <span title={installModeHint(host.install_mode)}>
                  {installModeLabel(host.install_mode)}
                </span>
              }
            />
            <Fact
              label="Updater"
              value={
                <span title={updaterHint(host.updater_present)}>
                  {updaterLabel(host.updater_present)}
                </span>
              }
            />
            <Fact label="Agent uptime" value={uptimeSince(host.agent_connected_since, now)} />
            <Fact
              label="Heartbeat"
              value={host.last_heartbeat_at ? relativeTime(host.last_heartbeat_at, now) : "Never"}
            />
            <Fact label="Uptime" value="n/a" />
            <Fact label="Scheduling" value={schedulingLabel(host)} />
          </tbody>
        </table>
      </div>
    </div>
  );
}

function GpuRow({ gpu }: { gpu: GPUAvailability }) {
  const vramKnown = gpu.vram_mb_used != null;
  const vramPct = vramKnown ? percentOf(gpu.vram_mb_used ?? 0, gpu.vram_mb_total) : 0;
  const slotPct = percentOf(gpu.slots_reserved, gpu.slots_total);

  return (
    <div className="cap-row" data-testid={`cap-row-gpu-${gpu.gpu_id}`}>
      {vramKnown ? (
        <Gauge percent={vramPct} color={toneColor(vramPct)} />
      ) : (
        <span className="gauge" style={unknownGauge}>
          <span>n/a</span>
        </span>
      )}
      <div className="cap-detail">
        <div className="rowflex">
          <span className="cap-name">{primaryGpuLabel(gpu.vendor, gpu.model)}</span>
          <span className="cell-id">{gpu.vendor}</span>
          <Chip variant={gpu.active_sessions > 0 ? "success" : undefined} className="cap-chip">
            {gpu.active_sessions} active
          </Chip>
        </div>
        <Bar
          label="VRAM"
          percent={vramPct}
          value={
            vramKnown
              ? `${bytesFromMb(gpu.vram_mb_used ?? 0)} / ${bytesFromMb(gpu.vram_mb_total)}`
              : null
          }
          variant={vramKnown ? tone(vramPct) : "default"}
          unknown={!vramKnown}
          hint={vramKnown ? undefined : "No VRAM telemetry reported yet"}
        />
        <Bar
          label="SLOTS"
          percent={slotPct}
          value={`${gpu.slots_reserved} / ${gpu.slots_total}`}
          variant={tone(slotPct)}
        />
      </div>
    </div>
  );
}

/** One volume's own fill, listed under the totals when a host has more than
 *  one: the totals cannot say which disk is full. */
function VolumeBar({ volume }: { volume: HostStorageVolume }) {
  const pct = percentOf(volume.total_mb - volume.available_mb, volume.total_mb);
  return (
    <Bar
      label={volume.label}
      percent={pct}
      value={`${bytesFromMb(volume.total_mb - volume.available_mb)} / ${bytesFromMb(volume.total_mb)}`}
      variant={tone(pct)}
      hint={volume.path}
    />
  );
}

function Fact({ label, value }: { label: string; value: ReactNode }) {
  return (
    <tr>
      <td className="fact-key">{label}</td>
      <td className="right primary">{value}</td>
    </tr>
  );
}

function cpuLabel(host: Host): string {
  const cores = host.cpu_cores != null ? `${host.cpu_cores} cores` : null;
  if (host.cpu_model && cores) return `${host.cpu_model} · ${cores}`;
  return host.cpu_model ?? cores ?? "n/a";
}
