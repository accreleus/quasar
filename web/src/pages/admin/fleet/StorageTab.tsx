// Fleet › Storage: managed-home oversight, grouped by user with a per-home
// expand (control-api.md AdminHome / storage.home.tombstone). "Delete" is the
// label; "tombstone" stays the identifier vocabulary (#386) — do not rename
// either to match the other.
//
// Provider is not admin-configurable here: `auto` and `local` resolve
// identically and `volume` was removed outright (#473) — there is nothing
// left to choose, so the toolbar's right slot is a read-only hint instead of
// a control.
//
// "Reclaim pending" does not re-tombstone: TombstoneHome sets
// `gc_after = now()` unconditionally, so calling it again on an
// already-pending home would push its 24h grace period further out instead
// of reclaiming it sooner. It runs the `home.gc` job (host-scoped, adopted —
// see cmd/quasar-control/app.go) now, once per host that has a pending home.

import { useMemo, useState } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AdminHome, StorageProvider } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { ActionsMenu, type ActionsMenuItem } from "../../../components/ActionsMenu";
import { Bar } from "../../../components/Bar";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { IconSearch } from "../../../components/icons";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { Table, type TableColumn } from "../../../components/Table";
import { useToast } from "../../../components/Toast";
import { bytes, bytesFromMb } from "../../../lib/format/bytes";
import { useFleetContext } from "../../../lib/fleet/FleetContext";
import { relativeTime } from "../../../lib/format/relativeTime";
import { useResource } from "../../../lib/resource/react";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { runNowErrorMessage } from "./jobsFormat";
import {
  distinctHostCount,
  distinctProviders,
  groupHomesByUser,
  homeState,
  isAppOrphaned,
  isHostOrphaned,
  NO_USER_KEY,
  type StorageUserGroup,
} from "./storageGroups";

const HOME_GC_JOB_ID = "home.gc";

const PROVIDER_LABELS: Partial<Record<StorageProvider, string>> = {
  auto: "Automatic",
  local: "Local directory",
};

function providerLabel(value: string): string {
  const known = PROVIDER_LABELS[value as StorageProvider];
  if (known) return known;
  return value.length === 0 ? "—" : value[0].toUpperCase() + value.slice(1);
}

export function StorageTab() {
  const { token } = useAuth();
  const { addToast } = useToast();

  const res = useResource({
    label: "storage",
    initialData: [] as AdminHome[],
    fetch: async (ctx): Promise<AdminHome[]> => {
      try {
        return (await adminApi.listAdminHomes(ctx.token)).items;
      } catch (e: unknown) {
        // P5-05 not yet merged on an older control plane — show empty, not an error.
        if (e instanceof ApiError && e.status === 404) return [];
        throw e;
      }
    },
  });
  const homes = res.data ?? [];
  // Feeds the "allocated" KPI sub-line only, off the fleet poll above this page
  // (lib/fleet) rather than a second read that could disagree with it.
  const { hosts } = useFleetContext();

  const [query, setQuery] = useState("");
  const [providerFilter, setProviderFilter] = useState<string>("all");
  const [expandedKeys, setExpandedKeys] = useState<Set<string>>(new Set());
  const [tombstoning, setTombstoning] = useState<AdminHome | null>(null);
  const [tombstoneError, setTombstoneError] = useState<string | null>(null);
  const [tombstoneInFlight, setTombstoneInFlight] = useState(false);
  const [reclaimOpen, setReclaimOpen] = useState(false);
  const [reclaiming, setReclaiming] = useState(false);

  // ── KPI strip (fleet-wide — unaffected by the toolbar filters below) ─────
  const totalBytes = useMemo(() => homes.reduce((s, h) => s + h.bytes_used, 0), [homes]);
  const hostsWithHomes = useMemo(() => distinctHostCount(homes), [homes]);
  const activeCount = useMemo(
    () => homes.filter((h) => h.user_id != null && !h.gc_after).length,
    [homes],
  );
  const pendingHomes = useMemo(() => homes.filter((h) => !!h.gc_after), [homes]);
  const pendingBytes = useMemo(() => pendingHomes.reduce((s, h) => s + h.bytes_used, 0), [pendingHomes]);
  const pendingHostIds = useMemo(
    () => Array.from(new Set(pendingHomes.filter((h) => h.host_id != null).map((h) => h.host_id!))),
    [pendingHomes],
  );
  const allocatedMb = useMemo(
    () => hosts.reduce((s, h) => s + (h.storage ?? []).reduce((ss, v) => ss + v.total_mb, 0), 0),
    [hosts],
  );

  // ── Toolbar filters ───────────────────────────────────────────────────
  const providerOptions = useMemo(() => distinctProviders(homes), [homes]);
  const filteredHomes = useMemo(() => {
    const q = query.trim().toLowerCase();
    return homes.filter((h) => {
      if (providerFilter !== "all" && h.provider !== providerFilter) return false;
      if (!q) return true;
      return (h.username ?? "").toLowerCase().includes(q) || (h.host_name ?? "").toLowerCase().includes(q);
    });
  }, [homes, query, providerFilter]);
  const groups = useMemo(() => groupHomesByUser(filteredHomes), [filteredHomes]);

  const toggleExpand = (key: string) => {
    setExpandedKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  // ── Delete (tombstone) — single home ──────────────────────────────────
  const confirmTombstone = async () => {
    if (!token || !tombstoning) return;
    setTombstoneInFlight(true);
    setTombstoneError(null);
    try {
      await res.mutate((ctx) => adminApi.tombstoneHome(ctx.token, tombstoning.id));
      setTombstoning(null);
      await res.refresh({ silent: true });
    } catch (e: unknown) {
      if (e instanceof ApiError && e.code === "home_in_use") {
        setTombstoneError("Cannot delete. A live session is currently using this home.");
      } else {
        setTombstoneError(e instanceof ApiError ? e.message : "Delete failed.");
      }
    } finally {
      setTombstoneInFlight(false);
    }
  };

  // ── Reclaim pending — runs the home.gc job now, once per host that has a
  //    pending home (see the file banner for why this isn't tombstoneHome).
  const confirmReclaim = async () => {
    if (!token || pendingHostIds.length === 0) return;
    setReclaiming(true);
    const results = await Promise.allSettled(
      pendingHostIds.map((hostId) => adminApi.runJobNow(token, HOME_GC_JOB_ID, { host_id: hostId })),
    );
    const failed = results.filter((r): r is PromiseRejectedResult => r.status === "rejected");
    setReclaiming(false);
    setReclaimOpen(false);
    if (failed.length > 0) {
      const reason = failed[0].reason;
      const body =
        reason instanceof ApiError ? runNowErrorMessage(reason.code, reason.message) : "Could not queue the run.";
      addToast({
        variant: "danger",
        title: `Queued cleanup on ${results.length - failed.length} of ${results.length} host${results.length === 1 ? "" : "s"}`,
        body,
      });
    } else {
      addToast({ variant: "success", title: `Queued cleanup on ${results.length} host${results.length === 1 ? "" : "s"}` });
    }
  };

  const actionsForHome = (h: AdminHome): ActionsMenuItem[] => [
    {
      key: "delete",
      label: "Delete",
      variant: "danger",
      disabled: !!h.gc_after,
      onClick: () => { setTombstoneError(null); setTombstoning(h); },
    },
  ];

  const homeColumns: TableColumn<AdminHome>[] = [
    {
      key: "host",
      header: "Host",
      render: (h) => (
        <div className="qtable-stack">
          {isHostOrphaned(h) ? (
            <span className="sub" style={{ color: "var(--warning-text)" }}>Host deleted</span>
          ) : (
            <span>{h.host_name}</span>
          )}
          {isAppOrphaned(h) ? (
            <span className="sub">App deleted</span>
          ) : (
            <span className="sub">{h.app_name}</span>
          )}
          <span className="sub" title={h.id}>
            Last used {relativeTime(h.last_used_at)} · {h.id.slice(0, 8)}
          </span>
        </div>
      ),
    },
    {
      key: "provider",
      header: "Provider",
      render: (h) => <span className="sub">{providerLabel(h.provider)}</span>,
    },
    {
      key: "usage",
      header: "Usage",
      render: (h) => {
        const pct = totalBytes > 0 ? Math.round((h.bytes_used / totalBytes) * 100) : 0;
        return <Bar percent={pct} variant="grad" value={`${pct}%`} />;
      },
    },
    {
      key: "size",
      header: <span style={{ display: "block", textAlign: "right" }}>Size</span>,
      mobileLabel: "Size",
      render: (h) => <span style={{ display: "block", textAlign: "right" }}>{bytes(h.bytes_used)}</span>,
    },
    {
      key: "state",
      header: "State",
      render: (h) => {
        const s = homeState(h);
        return <Chip variant={s.variant}>{s.label}</Chip>;
      },
    },
    {
      key: "actions",
      header: "",
      mobileLabel: "",
      render: (h) => (
        <div className="cell-actions">
          <ActionsMenu items={actionsForHome(h)} label={`Actions for ${h.username ?? "this"} home on ${h.host_name ?? "an unknown host"}`} />
        </div>
      ),
    },
  ];

  const groupColumns: TableColumn<StorageUserGroup>[] = [
    {
      key: "user",
      header: "User",
      render: (g) =>
        g.key === NO_USER_KEY ? (
          <div className="qtable-stack">
            <span>No linked user</span>
            <span className="sub">Owning account deleted, or never linked</span>
          </div>
        ) : (
          <span className="primary" title={g.userId ?? undefined}>{g.username ?? "—"}</span>
        ),
    },
    {
      key: "host",
      header: "Host",
      render: (g) => {
        const n = distinctHostCount(g.homes);
        if (n === 1) {
          const only = g.homes.find((h) => h.host_id != null);
          return <span className="sub">{only ? only.host_name : "Host deleted"}</span>;
        }
        return <span className="sub">{n} host{n === 1 ? "" : "s"}</span>;
      },
    },
    {
      key: "provider",
      header: "Provider",
      render: (g) => {
        const providers = distinctProviders(g.homes);
        return <span className="sub">{providers.length === 1 ? providerLabel(providers[0]) : "Mixed"}</span>;
      },
    },
    {
      key: "usage",
      header: "Usage",
      render: (g) => {
        const pct = totalBytes > 0 ? Math.round((g.totalBytes / totalBytes) * 100) : 0;
        return <Bar percent={pct} variant="grad" value={`${pct}%`} />;
      },
    },
    {
      key: "size",
      header: <span style={{ display: "block", textAlign: "right" }}>Size</span>,
      mobileLabel: "Size",
      render: (g) => <span style={{ display: "block", textAlign: "right" }}>{bytes(g.totalBytes)}</span>,
    },
    {
      key: "state",
      header: "State",
      render: (g) => {
        const pending = g.homes.filter((h) => !!h.gc_after).length;
        if (pending > 0) return <Chip variant="warning">{pending} pending</Chip>;
        if (g.key === NO_USER_KEY) return <Chip variant="neutral">No linked user</Chip>;
        return <Chip variant="success">Active</Chip>;
      },
    },
    {
      key: "actions",
      header: "",
      mobileLabel: "",
      render: () => null,
    },
  ];

  const renderExpandedGroup = (g: StorageUserGroup) => (
    <Table columns={homeColumns} rows={g.homes} rowKey={(h) => h.id} />
  );

  // The head is the Fleet section's (../Fleet.tsx); this tab fills it in.
  useSectionHead({
    sub: `${homes.length} managed home${homes.length === 1 ? "" : "s"} · ${bytes(totalBytes)} provisioned`,
    actions: (
      <>
        <Button variant="ghost" onClick={() => void res.refresh()}>Refresh</Button>
        <Button
          onClick={() => setReclaimOpen(true)}
          disabled={pendingHostIds.length === 0}
          title={
            pendingHostIds.length === 0
              ? pendingHomes.length === 0
                ? "No homes are pending cleanup."
                : "No known host for the homes pending cleanup."
              : undefined
          }
        >
          Reclaim pending
        </Button>
      </>
    ),
    counts: { storage: homes.length },
  });

  return (
    <section className="page">
      <ResourceStates loading={res.loading} error={res.errorMessage} />

      {!res.loading && (
        <>
          <div className="grid g4" style={{ marginBottom: "var(--s5)" }}>
            <div className="card card-pad">
              <div className="eyebrow">Managed homes</div>
              <div className="kpi-val" style={{ marginTop: 8 }}>{homes.length}</div>
              <div className="kpi-meta">across {hostsWithHomes} host{hostsWithHomes === 1 ? "" : "s"}</div>
            </div>
            <div className="card card-pad">
              <div className="eyebrow">Total size</div>
              <div className="kpi-val" style={{ marginTop: 8 }}>{bytes(totalBytes)}</div>
              {allocatedMb > 0 && (
                <div className="kpi-meta">of {bytesFromMb(allocatedMb)} allocated</div>
              )}
            </div>
            <div className="card card-pad">
              <div className="eyebrow">Active</div>
              <div className="kpi-val" style={{ marginTop: 8 }}>{activeCount}</div>
              <div className="kpi-meta">attached to a user</div>
            </div>
            <div className="card card-pad">
              <div className="eyebrow">Pending cleanup</div>
              <div className="kpi-val" style={{ marginTop: 8 }}>{pendingHomes.length}</div>
              {pendingHomes.length > 0 && (
                <div className="kpi-meta">{bytes(pendingBytes)} reclaimable</div>
              )}
            </div>
          </div>

          <div className="toolbar">
            <div className="search">
              <IconSearch />
              <input
                placeholder="Filter by user or host"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                aria-label="Filter by user or host"
              />
            </div>
            <select
              className="select"
              aria-label="Filter by provider"
              value={providerFilter}
              onChange={(e) => setProviderFilter(e.target.value)}
            >
              <option value="all">All providers</option>
              {providerOptions.map((p) => (
                <option key={p} value={p}>{providerLabel(p)}</option>
              ))}
            </select>
            <div className="right">
              <span className="hint">
                New homes are created as local directories under each host&rsquo;s storage root.
                Set the root under Fleet &rsaquo; Hosts &rsaquo; Settings.
              </span>
            </div>
          </div>

          <Table
            columns={groupColumns}
            rows={groups}
            rowKey={(g) => g.key}
            empty={
              query || providerFilter !== "all"
                ? "No homes match this filter."
                : "No managed homes yet. Enable “Managed home” on an app to start provisioning."
            }
            renderExpanded={renderExpandedGroup}
            isExpanded={(g) => expandedKeys.has(g.key)}
            onToggleExpand={(g) => toggleExpand(g.key)}
          />
        </>
      )}

      <Modal
        open={!!tombstoning}
        onClose={() => { setTombstoning(null); setTombstoneError(null); }}
        title="Delete managed home"
        footer={
          <>
            <Button
              variant="ghost"
              onClick={() => { setTombstoning(null); setTombstoneError(null); }}
              disabled={tombstoneInFlight}
            >
              Cancel
            </Button>
            <Button
              variant="danger"
              onClick={() => void confirmTombstone()}
              disabled={tombstoneInFlight}
            >
              {tombstoneInFlight ? "Deleting…" : "Delete"}
            </Button>
          </>
        }
      >
        {tombstoning && (
          <div style={{ display: "flex", flexDirection: "column", gap: "var(--s4)" }}>
            <p style={{ color: "var(--text-2)" }}>
              Mark this home for deletion. The GC janitor will reap the backing store.
            </p>
            <div
              className="panel"
              style={{ padding: "var(--s4)", display: "flex", flexDirection: "column", gap: "var(--s2)" }}
            >
              <div className="row gap3">
                <span className="muted" style={{ fontSize: "var(--t-sm)", minWidth: 64 }}>User</span>
                <span className="mono" style={{ fontSize: "var(--t-sm)" }} title={tombstoning.user_id ?? undefined}>
                  {tombstoning.username ?? "No linked user"}
                </span>
              </div>
              <div className="row gap3">
                <span className="muted" style={{ fontSize: "var(--t-sm)", minWidth: 64 }}>App</span>
                <span className="mono" style={{ fontSize: "var(--t-sm)" }} title={tombstoning.app_id ?? undefined}>
                  {tombstoning.app_name ?? "App deleted"}
                </span>
              </div>
              <div className="row gap3">
                <span className="muted" style={{ fontSize: "var(--t-sm)", minWidth: 64 }}>Size</span>
                <span className="mono" style={{ fontSize: "var(--t-sm)" }}>
                  {bytes(tombstoning.bytes_used)}
                </span>
              </div>
              {/* Full home id: names are neither unique nor rename-stable, so
                  the operator sees the exact row being destroyed. */}
              <div className="row gap3">
                <span className="muted" style={{ fontSize: "var(--t-sm)", minWidth: 64 }}>Home</span>
                <span className="mono" style={{ fontSize: "var(--t-sm)", overflowWrap: "anywhere" }}>
                  {tombstoning.id}
                </span>
              </div>
            </div>
            {tombstoneError && (
              <p style={{ color: "var(--danger-text)", fontSize: "var(--t-sm)" }}>
                {tombstoneError}
              </p>
            )}
          </div>
        )}
      </Modal>

      <Modal
        open={reclaimOpen}
        onClose={() => setReclaimOpen(false)}
        title="Reclaim pending homes"
        footer={
          <>
            <Button variant="ghost" onClick={() => setReclaimOpen(false)} disabled={reclaiming}>
              Cancel
            </Button>
            <Button variant="danger" onClick={() => void confirmReclaim()} disabled={reclaiming}>
              {reclaiming ? "Queuing…" : "Reclaim"}
            </Button>
          </>
        }
      >
        <p style={{ color: "var(--text-2)" }}>
          Runs the cleanup job now on {pendingHostIds.length} host{pendingHostIds.length === 1 ? "" : "s"}.
          Homes tombstoned less than 24 hours ago stay until their grace period ends.
        </p>
      </Modal>
    </section>
  );
}
