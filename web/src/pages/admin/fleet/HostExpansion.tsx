/**
 * The drawer under a host row (mock §A.4): hardware, the agent build, GPUs and
 * slots, storage, and the three places to go next. Host memory use, CPU load
 * and host uptime are not on the agent report, so they are absent, not guessed.
 */

import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { GPUAvailability, Host, HostStorageVolume } from "../../../api/types";
import { Bar } from "../../../components/Bar";
import { ReadinessCard } from "../../../components/ReadinessCard";
import { LOW_STORAGE_PCT } from "../../../lib/fleet/deriveAlerts";
import { bytesFromMb } from "../../../lib/format/bytes";
import { relativeTime } from "../../../lib/format/relativeTime";
import { primaryGpuLabel } from "../../../lib/gpu";
import { percentOf, storageTotals, tone, uptimeSince, utilisation } from "./hostDerived";
import {
  installModeHint,
  installModeLabel,
  shortCommit,
  updaterHint,
  updaterLabel,
} from "./hostIdentity";

export interface HostExpansionProps {
  host: Host;
  /** That host's GPUs; null when the per-host read failed, undefined until it
   *  has run. Both mean "no per-GPU detail", and neither is an empty list. */
  gpus: GPUAvailability[] | null | undefined;
  gpuError: string | null;
  /** Inline result of the last drain/uncordon on this row. */
  actionError?: string;
  now: number;
}

/** Free share under which the storage column turns red (mock: dp >= 90 used). */
const DISK_DANGER_PCT = 90;

export function HostExpansion({ host, gpus, gpuError, actionError, now }: HostExpansionProps) {
  const util = utilisation(host, gpus);
  const storage = storageTotals(host.storage);
  const volumes = host.storage ?? [];
  const activeSessions = host.capacity?.active_sessions ?? 0;

  return (
    <>
      {actionError && (
        <p className="form-error" role="alert">
          {actionError}
        </p>
      )}

      {host.capacity_detection !== "ok" && (
        <p className="note warn">
          <b>GPU capacity {host.capacity_detection}.</b> This host is not schedulable until
          fresh hardware capacity is reported.
          {host.capacity_reason ? ` ${host.capacity_reason}` : ""}
        </p>
      )}

      {host.status === "draining" && (
        <p className="note warn">
          <b>Draining.</b> No new sessions are placed here. {activeSessions} running{" "}
          {activeSessions === 1 ? "session finishes" : "sessions finish"}, then the host parks.
        </p>
      )}

      <div className="exp-in">
        <div>
          <div className="eyebrow">Hardware</div>
          <Fact label="CPU" value={cpuLabel(host)} />
          <Fact label="Memory" value={host.mem_mb != null ? bytesFromMb(host.mem_mb) : "n/a"} />
          <Fact label="Agent uptime" value={uptimeSince(host.agent_connected_since, now)} />
          <Fact label="Agent" value={<span className="mono">{host.agent_version ?? "n/a"}</span>} />
          <Fact label="Node ID" value={<span className="cell-id">{host.id}</span>} />
          {host.agent_restart_count > 0 && (
            <Fact
              label="Agent restarts"
              value={
                <span style={{ color: "var(--warning-text)" }}>
                  {host.agent_restart_count}
                  {host.agent_last_restart_at
                    ? ` · last ${relativeTime(host.agent_last_restart_at, now)}`
                    : ""}
                </span>
              }
            />
          )}
        </div>

        <div>
          <div className="eyebrow">Build</div>
          <Fact
            label="Commit"
            value={
              host.source_commit ? (
                <span className="mono" title={host.source_commit}>
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
        </div>

        <div>
          <div className="eyebrow">GPUs and slots</div>
          {gpuError && <p className="form-error">{gpuError}</p>}
          {gpus?.map((gpu) => (
            <Fact
              key={gpu.gpu_id}
              label={`${primaryGpuLabel(gpu.vendor, gpu.model)} #${gpu.gpu_index}`}
              value={
                <span className="num">
                  {gpu.slots_reserved}/{gpu.slots_total} slots · {vramText(gpu)}
                </span>
              }
            />
          ))}
          {gpus && gpus.length === 0 && !gpuError && <p className="sub">No GPUs reported.</p>}
          {util.gpu && (
            <Fact
              label="Total"
              value={
                <span className="num">
                  {util.gpu.used}/{util.gpu.total} slots
                  {util.vram
                    ? ` · ${bytesFromMb(util.vram.usedMb)}/${bytesFromMb(util.vram.totalMb)}`
                    : ""}
                </span>
              }
            />
          )}
        </div>

        <div>
          <div className="eyebrow">Storage</div>
          {storage ? (
            <>
              {volumes.map((volume) => (
                <VolumeBar key={volume.path} volume={volume} />
              ))}
              <Fact
                label="USED"
                value={
                  <span className="num">
                    {bytesFromMb(storage.usedMb)} / {bytesFromMb(storage.totalMb)}
                  </span>
                }
              />
              <Fact
                label="Free"
                value={
                  <span
                    className="num"
                    style={
                      (util.diskPct ?? 0) >= DISK_DANGER_PCT
                        ? { color: "var(--danger-text)" }
                        : undefined
                    }
                  >
                    {bytesFromMb(storage.totalMb - storage.usedMb)}
                  </span>
                }
              />
              {volumes.map((volume) => (
                <Fact
                  key={volume.path}
                  label={volumes.length === 1 ? "Root" : volume.label}
                  value={<span className="mono">{volume.path}</span>}
                />
              ))}
            </>
          ) : (
            <p className="sub">No volumes reported.</p>
          )}
          <div style={{ marginTop: 9 }}>
            <Link to="/admin/fleet/storage" onClick={(e) => e.stopPropagation()}>
              Storage detail
            </Link>
          </div>
        </div>

        <div>
          <div className="eyebrow">Actions</div>
          <div className="exp-actions">
            <Link className="btn btn-sm btn-ghost" to={`/admin/fleet/hosts/${host.id}`}>
              Open host
            </Link>
            <Link className="btn btn-sm btn-ghost" to={`/admin/fleet/hosts/${host.id}/console`}>
              Local console
            </Link>
            <Link className="btn btn-sm btn-ghost" to={`/admin/fleet/hosts/${host.id}/settings`}>
              Host settings
            </Link>
          </div>
        </div>
      </div>

      {(host.readiness?.length ?? 0) > 0 && (
        <div className="exp-readiness">
          <ReadinessCard checks={host.readiness} reportedAt={host.readiness_reported_at} />
        </div>
      )}
    </>
  );
}

/** One volume's own fill, so a full disk cannot hide inside a host-wide
 *  average. The warning threshold is the alert list's (lib/fleet/deriveAlerts):
 *  a row must not sit amber while Needs attention says the host is fine. */
function VolumeBar({ volume }: { volume: HostStorageVolume }) {
  const usedPct = percentOf(volume.total_mb - volume.available_mb, volume.total_mb);
  const freePct = volume.total_mb === 0 ? 0 : (volume.available_mb / volume.total_mb) * 100;
  return (
    <Bar
      label={volume.label}
      percent={usedPct}
      value={`${bytesFromMb(volume.total_mb - volume.available_mb)} / ${bytesFromMb(volume.total_mb)}`}
      variant={freePct < LOW_STORAGE_PCT ? "warning" : tone(usedPct)}
      hint={volume.path}
    />
  );
}

function Fact({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="exp-fact">
      <span>{label}</span>
      <span>{value}</span>
    </div>
  );
}

function cpuLabel(host: Host): string {
  const cores = host.cpu_cores != null ? `${host.cpu_cores} cores` : null;
  if (host.cpu_model && cores) return `${host.cpu_model} · ${cores}`;
  return host.cpu_model ?? cores ?? "n/a";
}

/** Live VRAM for one GPU, or "vram n/a" when it has never been sampled — an
 *  unsampled GPU is not a GPU at zero. */
function vramText(gpu: GPUAvailability): string {
  if (gpu.vram_mb_used == null) return "vram n/a";
  return `${bytesFromMb(gpu.vram_mb_used)}/${bytesFromMb(gpu.vram_mb_total)}`;
}
