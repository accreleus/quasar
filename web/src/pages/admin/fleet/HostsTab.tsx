/**
 * `/admin/fleet/hosts` — the Hosts tab of the Fleet section (spec §5.8, §A.4).
 * Rows come from `FleetContext`, so this opens no hosts poll of its own.
 */

import { Fragment, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { GPUAvailability, Host } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { IconPlus, IconRefresh, IconSearch, IconSliders } from "../../../components/icons";
import { needsAttention } from "../../../lib/fleet/deriveAlerts";
import { useFleetContext } from "../../../lib/fleet/FleetContext";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { EnrollHostModal } from "./EnrollHostModal";
import { HostRow } from "./HostRow";
import "../../../styles/admin/fleet.css";

// `Host.capacity` carries the roll-up but no GPU model names, so one resource
// fans `GET /v1/hosts/{id}/gpus` over the hosts at 30 s rather than the fleet's
// 5 s: a model does not change, and N requests every five seconds is the 1+N
// fan-out the roll-up exists to remove. A per-host failure degrades that row's
// GPU cell, never the table.

/** A GPU list per host id; null for a host whose read failed. */
type GpuMap = Record<string, GPUAvailability[] | null>;

const GPU_POLL_MS = 30_000;

type Segment = "all" | "online" | "attention";

export function HostsTab() {
  const navigate = useNavigate();
  const { token } = useAuth();
  const fleet = useFleetContext();

  const [segment, setSegment] = useState<Segment>("all");
  const [query, setQuery] = useState("");
  const [groupByVendor, setGroupByVendor] = useState(false);
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  const [enrollOpen, setEnrollOpen] = useState(false);
  const [forgetTarget, setForgetTarget] = useState<Host | null>(null);
  const [forgetError, setForgetError] = useState<string | null>(null);
  const [actionPendingId, setActionPendingId] = useState<string | null>(null);
  const [actionErrors, setActionErrors] = useState<Record<string, string>>({});

  const hosts = fleet.hosts;
  const hostIds = hosts.map((h) => h.id);
  const gpuKey = hostIds.join(",");
  const gpuRes = useResource<GpuMap>(
    {
      label: "host-gpus",
      pollMs: GPU_POLL_MS,
      initialData: {},
      fetch: async (ctx) => {
        const pairs = await Promise.all(
          hostIds.map(async (id): Promise<[string, GPUAvailability[] | null]> => {
            try {
              return [id, (await adminApi.getHostGPUs(ctx.token, id)).items];
            } catch {
              return [id, null];
            }
          }),
        );
        return Object.fromEntries(pairs);
      },
    },
    [gpuKey],
  );
  const gpus = gpuRes.data ?? {};

  const now = fleet.lastFetchedAt ?? Date.now();
  const online = hosts.filter((h) => h.status === "online").length;
  const attention = hosts.filter(needsAttention).length;

  const visible = useMemo(() => {
    const text = query.trim().toLowerCase();
    return hosts.filter((host) => {
      if (segment === "online" && host.status !== "online") return false;
      if (segment === "attention" && !needsAttention(host)) return false;
      if (!text) return true;
      return (
        host.node_name.toLowerCase().includes(text) || host.id.toLowerCase().includes(text)
      );
    });
  }, [hosts, segment, query]);

  const groups = useMemo(
    () => (groupByVendor ? groupByGpuVendor(visible, gpus) : [{ label: null, hosts: visible }]),
    [groupByVendor, visible, gpus],
  );

  const toggle = (id: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  /** Drain and uncordon report in the row's own drawer rather than as a toast:
   *  the answer is the row's new state, and it is right there. */
  const runRowAction = async (
    host: Host,
    call: (token: string) => Promise<unknown>,
    failure: string,
  ) => {
    if (!token) return;
    setActionPendingId(host.id);
    setActionErrors(({ [host.id]: _cleared, ...rest }) => rest);
    try {
      await call(token);
      await fleet.reload();
    } catch (e: unknown) {
      setActionErrors((prev) => ({
        ...prev,
        [host.id]: e instanceof ApiError ? e.message : failure,
      }));
      setExpanded((prev) => new Set(prev).add(host.id));
    } finally {
      setActionPendingId(null);
    }
  };

  /** Shared with the row's own Drain menu item — the confirm modal offers the
   *  same action for a connected host rather than a second code path. */
  const drainRow = (host: Host) =>
    void runRowAction(host, (t) => adminApi.drainHost(t, host.id), "drain failed");

  const forget = useAdminAction<[Host], void>(
    async (host) => {
      if (!token) return;
      await adminApi.deleteHost(token, host.id);
      await fleet.reload();
    },
    {
      success: (_result, host) => `Host "${host.node_name}" forgotten`,
      failure: "could not forget host",
      onSuccess: () => setForgetTarget(null),
      // Keep the modal open on failure (e.g. 409: sessions still active, or
      // the host reconnected) and show the server's message inline (#101).
      onFailure: (error) =>
        setForgetError(error instanceof ApiError ? error.message : "could not remove host"),
    },
  );

  useSectionHead({
    sub: `${online} of ${hosts.length} host${hosts.length === 1 ? "" : "s"} online · ${
      fleet.sessions.length
    } session${fleet.sessions.length === 1 ? "" : "s"} running`,
    actions: (
      <>
        <Button
          variant="ghost"
          onClick={() => {
            void fleet.reload();
            void gpuRes.refresh();
          }}
        >
          <IconRefresh />
          Refresh
        </Button>
        <Button variant="primary" onClick={() => setEnrollOpen(true)}>
          <IconPlus />
          Enroll host
        </Button>
      </>
    ),
    counts: { hosts: hosts.length },
  });

  return (
    <section className="page hosts-page">
      <div className="toolbar">
        <SegmentedControl<Segment>
          aria-label="Filter hosts by state"
          value={segment}
          onChange={setSegment}
          options={[
            { value: "all", label: "All" },
            { value: "online", label: "Online" },
            {
              value: "attention",
              label: (
                <>
                  Needs attention <span className="num">{attention}</span>
                </>
              ),
              ariaLabel: `Needs attention, ${attention}`,
            },
          ]}
        />
        <div className="search">
          <IconSearch />
          <input
            aria-label="Filter hosts"
            placeholder="Filter hosts"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
        <div className="right">
          <Button
            variant="ghost"
            size="sm"
            aria-pressed={groupByVendor}
            onClick={() => setGroupByVendor((v) => !v)}
          >
            <IconSliders />
            Group by GPU vendor
          </Button>
        </div>
      </div>

      <div className="card table-wrap">
        <ResourceStates loading={fleet.loading} error={fleet.errors.hosts} />

        {!fleet.loading && hosts.length === 0 ? (
          <div className="empty">
            <h3>No hosts enrolled</h3>
            <p>A GPU machine running the node agent registers itself and appears here.</p>
            <Button variant="primary" onClick={() => setEnrollOpen(true)}>
              <IconPlus />
              Enroll host
            </Button>
          </div>
        ) : !fleet.loading && visible.length === 0 ? (
          <div className="empty">
            <h3>No hosts match</h3>
            <p>No host matches this filter. Clear it to see the whole fleet.</p>
          </div>
        ) : (
          <table className="qtable">
            <thead>
              <tr>
                <th className="qtable-expand-th" />
                <th>Host</th>
                <th>GPU</th>
                <th>Utilisation</th>
                <th className="right">Live</th>
                <th className="right">Seen</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {groups.map((group) => (
                <Fragment key={group.label ?? "all"}>
                  {group.label && (
                    <tr className="group-row">
                      <td colSpan={7}>
                        <span className="eyebrow">{group.label}</span>
                        <span className="num">{group.hosts.length}</span>
                      </td>
                    </tr>
                  )}
                  {group.hosts.map((host) => (
                    <HostRow
                      key={host.id}
                      host={host}
                      gpus={host.id in gpus ? gpus[host.id] : undefined}
                      gpuError={gpus[host.id] === null ? "Could not load GPUs" : null}
                      expanded={expanded.has(host.id)}
                      onToggle={() => toggle(host.id)}
                      onOpen={() => navigate(`/admin/fleet/hosts/${host.id}`)}
                      onConsole={() => navigate(`/admin/fleet/hosts/${host.id}/console`)}
                      onSettings={() => navigate(`/admin/fleet/hosts/${host.id}/settings`)}
                      onDrain={() => drainRow(host)}
                      onResume={() =>
                        void runRowAction(
                          host,
                          (t) => adminApi.uncordonHost(t, host.id),
                          "uncordon failed",
                        )
                      }
                      onForget={() => {
                        setForgetError(null);
                        setForgetTarget(host);
                      }}
                      actionPending={actionPendingId === host.id}
                      actionError={actionErrors[host.id]}
                      now={now}
                    />
                  ))}
                </Fragment>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <EnrollHostModal open={enrollOpen} onClose={() => setEnrollOpen(false)} />

      {forgetTarget &&
        (() => {
          // The server refuses (409) while the agent is reachable or has
          // active sessions, checked live rather than off `status` — so an
          // offline row still gets the real confirm, everything else gets
          // the explanation + a way to get there (#101).
          // Read the LIVE row, not the snapshot taken when the dialog opened:
          // Drain from inside the dialog changes the status it is explaining.
          const live = hosts.find((h) => h.id === forgetTarget.id) ?? forgetTarget;
          const removable = live.status === "offline";
          const draining = live.status === "draining" || actionPendingId === live.id;
          return (
            <Modal
              open
              onClose={() => setForgetTarget(null)}
              title="Remove host"
              footer={
                removable ? (
                  <>
                    <Button variant="ghost" onClick={() => setForgetTarget(null)}>
                      Cancel
                    </Button>
                    <Button
                      variant="danger"
                      disabled={forget.pending != null}
                      onClick={() => void forget.run(forgetTarget)}
                    >
                      {forget.pending ? "Removing…" : "Remove host"}
                    </Button>
                  </>
                ) : (
                  <>
                    <Button variant="ghost" onClick={() => setForgetTarget(null)}>
                      Close
                    </Button>
                    <Button variant="primary" disabled={draining} onClick={() => drainRow(live)}>
                      {draining ? "Draining…" : "Drain"}
                    </Button>
                  </>
                )
              }
            >
              {removable ? (
                <>
                  <p className="sec">
                    This permanently removes <strong>{forgetTarget.node_name}</strong> from the
                    host registry. Its GPU records and session history are deleted. If the host
                    comes back online it re-enrolls automatically. This cannot be undone.
                  </p>
                  {forgetError && (
                    <p className="note warn" role="alert">
                      {forgetError}
                    </p>
                  )}
                </>
              ) : (
                <>
                  <p className="sec">
                    <strong>{forgetTarget.node_name}</strong> is still connected, so it can't be
                    removed yet.
                  </p>
                  <p className="note">
                    Drain it, then stop the agent on that machine — for a host enrolled with the
                    installer, <span className="mono">docker compose --project-directory
                    /opt/quasar-agent down</span>. The row turns offline within a heartbeat
                    window; remove it from here once it does.
                  </p>
                </>
              )}
            </Modal>
          );
        })()}
    </section>
  );
}

interface HostGroup {
  label: string | null;
  hosts: Host[];
}

/** Group headers by the first GPU's vendor. A host whose GPUs have not loaded
 *  (or failed) groups under "Unknown vendor" rather than vanishing. */
function groupByGpuVendor(hosts: Host[], gpus: GpuMap): HostGroup[] {
  const byVendor = new Map<string, Host[]>();
  for (const host of hosts) {
    const vendor = gpus[host.id]?.[0]?.vendor ?? "Unknown vendor";
    const bucket = byVendor.get(vendor);
    if (bucket) bucket.push(host);
    else byVendor.set(vendor, [host]);
  }
  return [...byVendor.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([label, list]) => ({ label, hosts: list }));
}
