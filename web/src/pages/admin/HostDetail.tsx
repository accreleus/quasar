/**
 * `/admin/fleet/hosts/:id` — one host (spec §5.8, mock §A.5), composed from
 * `GET /v1/hosts/{id}` and `/gpus` in one resource, plus the shared live
 * session poll filtered to this host.
 */

import { useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import type {
  GPUAvailability,
  Host,
  PlatformIdentity,
  PlatformReleaseFault,
} from "../../api/types";
import { useAuth } from "../../auth/context";
import { Breadcrumbs } from "../../components/Breadcrumbs";
import { shortId } from "../../lib/format/shortId";
import { Button } from "../../components/Button";
import { Chip } from "../../components/Chip";
import { Modal } from "../../components/Modal";
import { PageHeader } from "../../components/PageHeader";
import { ReadinessCard } from "../../components/ReadinessCard";
import { ResourceStates } from "../../components/ResourceStates";
import { useFleetContext } from "../../lib/fleet/FleetContext";
import { bytesFromMb } from "../../lib/format/bytes";
import { elapsedWords, relativeTime } from "../../lib/format/relativeTime";
import { useAdminAction } from "../../lib/resource/action";
import { useResource } from "../../lib/resource/react";
import { ImageCleanupModal } from "./library/ImageCleanupModal";
import { CapacityCard } from "./fleet/hostDetail/CapacityCard";
import { ServicesCard } from "./fleet/hostDetail/ServicesCard";
import { SessionsCard } from "./fleet/hostDetail/SessionsCard";
import { agentOlderThanControlPlane, hostServices } from "./fleet/hostServices";
import { AdmissionReasons, admissionActionLabel, canChangeOperatorDrain, hasOperatorDrain } from "./fleet/AdmissionReasons";
import { hostStateChip, hostStateLabel } from "./fleet/hostDerived";
import { faultText } from "./fleet/releasesCopy";
import "../../styles/admin/fleet.css";

const POLL_MS = 5000;

interface HostDetailData {
  host: Host;
  /** Null when the GPU read failed: the host is still worth rendering. */
  gpus: GPUAvailability[] | null;
  /** This host's platform-release faults. Empty when the read failed: a fault
   *  gates nothing, so its absence must never blank the page. */
  faults: PlatformReleaseFault[];
  /** The installed control plane's commit, from the same read; null when unknown. */
  controlPlaneCommit: string | null;
  /** The control plane's own identity, machine fields included; null when unread. */
  controlPlane: PlatformIdentity | null;
}

export function HostDetail() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const { token } = useAuth();
  const fleet = useFleetContext();

  // The two reads are one question — what is this host doing — so they share a
  // timer, a loading state and an error surface.
  const res = useResource<HostDetailData>(
    {
      label: "host",
      pollMs: POLL_MS,
      fetch: async (ctx): Promise<HostDetailData> => {
        const [{ host }, gpus, release] = await Promise.all([
          adminApi.getHost(ctx.token, id),
          adminApi.getHostGPUs(ctx.token, id).then(
            (r) => r.items,
            () => null,
          ),
          // Rides this page's one poll rather than earning a second timer.
          adminApi.getPlatformReleases(ctx.token, ctx.signal).then(
            (v) => ({
              faults: v.faults.filter((f) => f.host_id === id),
              controlPlaneCommit: v.installed?.control_plane.source_commit ?? null,
              controlPlane: v.installed?.control_plane ?? null,
            }),
            () => ({ faults: [], controlPlaneCommit: null, controlPlane: null }),
          ),
        ]);
        return { host, gpus, ...release };
      },
    },
    [id],
  );

  const host = res.data?.host;
  const gpus = res.data?.gpus ?? null;
  const faults = res.data?.faults ?? [];
  const controlPlaneCommit = res.data?.controlPlaneCommit ?? null;
  const now = res.updatedAt ?? Date.now();

  const sessions = useMemo(
    () => fleet.sessions.filter((s) => s.host_id === id),
    [fleet.sessions, id],
  );

  const drain = useAdminAction<[Host], void>(
    async (target) => {
      if (!token) return;
      if (hasOperatorDrain(target)) await adminApi.uncordonHost(token, target.id);
      else await adminApi.drainHost(token, target.id);
      await res.refresh({ silent: true });
      await fleet.reload();
    },
    {
      success: (_r, target) =>
        hasOperatorDrain(target)
          ? `Operator drain released for ${target.node_name}`
          : `Operator drain added for ${target.node_name}`,
      failure: (_e, target) =>
        hasOperatorDrain(target) ? "could not release operator drain" : "could not drain host",
    },
  );

  // One shared pending id: the card disables whichever button (set or clear)
  // matches, and only one override action is in flight for a given check.
  const setOverride = useAdminAction<[string], void>(
    async (checkId) => {
      if (!token || !host) return;
      await adminApi.setReadinessOverride(token, host.id, checkId);
      await res.refresh({ silent: true });
    },
    {
      success: (_r, checkId) => `Launches on this host may proceed despite ${checkId} failing`,
      failure: (e, checkId) =>
        e instanceof ApiError && e.code === "conflict"
          ? { title: "Could not set the override", body: e.message }
          : `Could not set an override for ${checkId}`,
    },
  );

  const clearOverride = useAdminAction<[string], void>(
    async (checkId) => {
      if (!token || !host) return;
      await adminApi.clearReadinessOverride(token, host.id, checkId);
      await res.refresh({ silent: true });
    },
    {
      success: (_r, checkId) => `The override for ${checkId} was withdrawn`,
      failure: (_e, checkId) => `Could not withdraw the override for ${checkId}`,
    },
  );

  const overridePending = setOverride.pending?.[0] ?? clearOverride.pending?.[0] ?? null;

  // Awaiting confirmation in the Modal below; null when no "Launch anyway" is pending.
  const [confirmOverrideCheckId, setConfirmOverrideCheckId] = useState<string | null>(null);
  const [cleanupOpen, setCleanupOpen] = useState(false);

  const crumbs = (
    <Breadcrumbs
      items={[
        { label: "Fleet", to: "/admin/fleet/hosts" },
        { label: shortId(id), title: id, mono: true },
      ]}
    />
  );

  if (!host) {
    return (
      <section className="page host-detail-page">
        {crumbs}
        <ResourceStates loading={res.loading} error={res.errorMessage} />
      </section>
    );
  }

  const state = hostStateLabel(host);
  const services = hostServices(host, {
    machine: res.data?.controlPlane ?? null,
    agentOlder: agentOlderThanControlPlane(host, controlPlaneCommit, faults),
  });

  return (
    <section className="page host-detail-page">
      {crumbs}

      <PageHeader
        title={host.node_name}
        sub={[
          services ? services.shape : null,
          host.cpu_model,
          host.mem_mb != null ? bytesFromMb(host.mem_mb) : null,
        ]
          .filter(Boolean)
          .join(" · ")}
        actions={
          <>
            <Chip variant={hostStateChip(host)} dot={state === "online"}>
              {state}
            </Chip>
            <Button
              variant="ghost"
              onClick={() => navigate(`/admin/fleet/hosts/${host.id}/console`)}
            >
              Local console
            </Button>
            <Button variant="ghost" onClick={() => setCleanupOpen(true)}>
              Manage cached images
            </Button>
            <Button
              variant="ghost"
              disabled={drain.pending != null || !canChangeOperatorDrain(host)}
              onClick={() => void drain.run(host)}
            >
              {admissionActionLabel(host)}
            </Button>
            <Button onClick={() => navigate(`/admin/fleet/hosts/${host.id}/settings`)}>
              Settings
            </Button>
          </>
        }
      />

      <ResourceStates loading={res.loading} error={res.errorMessage} />

      {host.status === "offline" && (
        <p className="note warn host-note">
          <b>
            No heartbeat for{" "}
            {host.last_heartbeat_at ? elapsedWords(host.last_heartbeat_at, now) : "some time"}.
          </b>{" "}
          Scheduling is paused for this host.
          {host.last_heartbeat_at
            ? ` Last successful heartbeat ${relativeTime(host.last_heartbeat_at, now)}.`
            : " It has never sent one."}
        </p>
      )}

      <AdmissionReasons host={host} className="note warn host-note" />

      {faults.map((fault) => (
        <p className="note warn host-note" key={fault.kind}>
          <b>{faultText(fault.kind)}.</b> {fault.detail}
        </p>
      ))}

      {host.capacity_detection !== "ok" && (
        <p className="note warn host-note">
          <b>GPU capacity {host.capacity_detection}.</b> This host is not schedulable until
          fresh hardware capacity is reported.
          {host.capacity_reason ? ` ${host.capacity_reason}` : ""}
        </p>
      )}

      {services && (
        <ServicesCard
          nodeName={host.node_name}
          services={services}
          connectedSince={host.agent_connected_since}
          now={now}
        />
      )}

      <CapacityCard host={host} gpus={gpus} now={now} />

      <SessionsCard sessions={sessions} now={now} />

      {/* Full width, checks in a grid: a dozen checks as a sidebar list ran
          three screens tall next to a two-row table. */}
      <ReadinessCard
        layout="grid"
        checks={host.readiness}
        reportedAt={host.readiness_reported_at}
        gate={host.readiness_gate}
        overrides={host.readiness_overrides}
        onSetOverride={(checkId) => setConfirmOverrideCheckId(checkId)}
        onClearOverride={(checkId) => void clearOverride.run(checkId)}
        overridePending={overridePending}
        footnote={
          <>
            The <strong>Restart agent</strong> action on this host's settings page re-runs
            these checks. Driver fixes (missing EGL or 32-bit libraries, for example) need the
            agent container recreated, not just restarted: redeploy the agent after a host-level
            fix, then restart to refresh this card.
          </>
        }
      />

      {confirmOverrideCheckId && (
        <Modal
          open
          onClose={() => setConfirmOverrideCheckId(null)}
          title="Launch despite a failing check"
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmOverrideCheckId(null)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={setOverride.pending != null}
                onClick={() => {
                  void setOverride.run(confirmOverrideCheckId);
                  setConfirmOverrideCheckId(null);
                }}
              >
                Launch anyway
              </Button>
            </>
          }
        >
          <p className="sec">
            Sessions will be placed on {host.node_name} although the "{confirmOverrideCheckId}"
            check is failing. The check stays visible, and the override ends when it next passes.
          </p>
        </Modal>
      )}
      {cleanupOpen && token && (
        <ImageCleanupModal token={token} hostID={host.id} hostName={host.node_name}
          onClose={() => setCleanupOpen(false)} />
      )}
    </section>
  );
}
