// v3 Library > Apps tab (handoff §A.9). Adds discovered-but-unpublished
// titles and the `?preset=` drill-in on top of P2-08's AdminApps behaviour
// (catalog list, enable/disable, Ignore, Delete, artwork re-fetch).

import { useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AdminApp, LaunchProfile, RuntimePreset } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { ActionsMenu } from "../../../components/ActionsMenu";
import { Button } from "../../../components/Button";
import { IconPlus, IconRefresh } from "../../../components/icons";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { SearchInput } from "../../../components/TextField";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { useToast } from "../../../components/Toast";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { coverClassAt } from "../../app/libraryGrid";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { DeleteAppModal, IgnoreAppModal } from "./AppConfirmModals";
import { AppRow } from "./AppRow";
import { PendingImportRow } from "./PendingImportRow";
import {
  filterApps,
  parseAppSegment,
  parseAppSourceFilter,
  segmentCounts,
  type AppSegment,
  type AppSourceFilter,
  type PendingImportItem,
} from "./appsFilters";

/** `useAdminAction`'s `pending` is the newest unfinished call's args or null;
 *  this keys it by the pending row's identity for "which Import button is busy". */
function isPendingRowBusy(pendingArgs: [PendingImportItem] | null, row: PendingImportItem): boolean {
  if (!pendingArgs) return false;
  const [p] = pendingArgs;
  return p.providerAppId === row.providerAppId && p.item.external_id === row.item.external_id;
}

const SEGMENTS: { value: AppSegment; label: string; countKey?: keyof ReturnType<typeof segmentCounts> }[] = [
  { value: "all", label: "All", countKey: "all" },
  { value: "games", label: "Games" },
  { value: "desktops", label: "Desktops" },
  { value: "disabled", label: "Disabled", countKey: "disabled" },
  { value: "pending", label: "Pending import", countKey: "pending" },
];

export function AppsTab() {
  const { token } = useAuth();
  const navigate = useNavigate();
  const { addToast } = useToast();
  const [searchParams, setSearchParams] = useSearchParams();

  const appsRes = useResource({
    label: "apps",
    initialData: [] as AdminApp[],
    fetch: async (ctx) => (await adminApi.listAdminApps(ctx.token)).items,
  });
  const apps = appsRes.data ?? [];

  // A provider app (library_provider !== "") is the one identity the
  // per-provider unpublished endpoint is keyed on — every derived tile it
  // owns carries parent_app_id, never library_provider itself.
  const providerApps = useMemo(() => apps.filter((a) => a.library_provider !== ""), [apps]);
  const providerKey = useMemo(
    () => providerApps.map((a) => a.id).sort().join(","),
    [providerApps],
  );

  // No fleet-wide "everything unpublished" endpoint exists (api-surface-map):
  // GET /v1/admin/apps/{id}/library/unpublished is scoped to one provider
  // app, so this fans that call out across every provider app this instance
  // has and flattens the result. 30s poll matches the pending count's use in
  // the head sub-line staying live without a manual refresh.
  const pendingRes = useResource(
    {
      label: "library-unpublished",
      pollMs: 30_000,
      initialData: [] as PendingImportItem[],
      fetch: async (ctx) => {
        const lists = await Promise.all(
          providerApps.map((p) =>
            adminApi
              .listUnpublishedLibraryItems(ctx.token, p.id)
              .then((res) => res.items.map((item) => ({ providerAppId: p.id, item }))),
          ),
        );
        return lists.flat();
      },
    },
    [providerKey],
  );
  const pending = pendingRes.data ?? [];

  const presetsRes = useResource({
    label: "runtime-presets-lookup",
    initialData: [] as RuntimePreset[],
    fetch: async (ctx) => (await adminApi.listRuntimePresets(ctx.token)).items,
  });
  const profilesRes = useResource({
    label: "launch-profiles-lookup",
    initialData: [] as LaunchProfile[],
    fetch: async (ctx) => (await adminApi.listLaunchProfiles(ctx.token)).items,
  });
  const presetNameById = useMemo(
    () => new Map((presetsRes.data ?? []).map((p) => [p.id, p.name])),
    [presetsRes.data],
  );
  const profileNameById = useMemo(
    () => new Map((profilesRes.data ?? []).map((p) => [p.id, p.display_name])),
    [profilesRes.data],
  );

  const coverClassById = useMemo(() => {
    const byName = [...apps].sort((a, b) => a.name.localeCompare(b.name));
    const m = new Map<string, string>();
    byName.forEach((a, i) => m.set(a.id, coverClassAt(i)));
    return m;
  }, [apps]);

  // Initialised from the URL once (SourcesTab links to `?segment=pending` and
  // `?source=manual`); an unrecognised value falls back to "all" rather than
  // rendering an unfiltered list under a filter label that doesn't apply.
  const [segment, setSegmentState] = useState<AppSegment>(() =>
    parseAppSegment(searchParams.get("segment")),
  );
  const [query, setQuery] = useState("");
  const [source, setSourceState] = useState<AppSourceFilter>(() =>
    parseAppSourceFilter(searchParams.get("source")),
  );
  const presetId = searchParams.get("preset") ?? undefined;
  const presetName = presetId ? (presetNameById.get(presetId) ?? presetId) : null;

  // Keeps the URL in sync with user-driven changes so the toolbar state
  // survives a reload/share; "all" is the default and carries no param.
  const setSegment = (next: AppSegment) => {
    setSegmentState(next);
    const params = new URLSearchParams(searchParams);
    if (next === "all") params.delete("segment");
    else params.set("segment", next);
    setSearchParams(params, { replace: true });
  };
  const setSource = (next: AppSourceFilter) => {
    setSourceState(next);
    const params = new URLSearchParams(searchParams);
    if (next === "all") params.delete("source");
    else params.set("source", next);
    setSearchParams(params, { replace: true });
  };

  const clearPreset = () => {
    const next = new URLSearchParams(searchParams);
    next.delete("preset");
    setSearchParams(next);
  };

  const counts = useMemo(() => segmentCounts(apps, pending), [apps, pending]);
  const filtered = useMemo(
    () => filterApps(apps, pending, { segment, q: query, source, presetId }),
    [apps, pending, segment, query, source, presetId],
  );

  const [deleteTarget, setDeleteTarget] = useState<AdminApp | null>(null);
  const [ignoreTarget, setIgnoreTarget] = useState<AdminApp | null>(null);
  const [reresolveOpen, setReresolveOpen] = useState(false);
  const [reresolveForce, setReresolveForce] = useState(false);
  const [reresolving, setReresolving] = useState(false);

  const toggle = useAdminAction(
    (app: AdminApp) =>
      appsRes.mutate(
        (ctx) => adminApi.updateApp(ctx.token, app.id, { enabled: !app.enabled }),
        (items, result) => items.map((a) => (a.id === result.app.id ? result.app : a)),
      ),
    { failure: "could not update app" },
  );

  // Disabled Delete is UX only; the server's 409 while in use is the
  // enforcement (CLAUDE.md invariant #6).
  const del = useAdminAction<[AdminApp], void>(
    (app) =>
      appsRes.mutate(
        (ctx) => adminApi.deleteApp(ctx.token, app.id),
        (items) => items.filter((a) => a.id !== app.id),
      ),
    {
      success: (_result, app) => `"${app.name}" deleted`,
      failure: "could not delete app",
      onSuccess: () => setDeleteTarget(null),
      onFailure: () => setDeleteTarget(null),
    },
  );

  // rule='ignore' (spec §8.2): the server disables the tile + revokes
  // provider entitlements in one transaction; reflect disabled locally.
  const ignore = useAdminAction(
    (app: AdminApp) => {
      if (!app.parent_app_id) throw new Error("ignore called on a non-derived tile");
      const parentId = app.parent_app_id;
      return appsRes.mutate(
        (ctx) => adminApi.setLibraryRule(ctx.token, parentId, app.external_id, { rule: "ignore" }),
        (items) => items.map((a) => (a.id === app.id ? { ...a, enabled: false } : a)),
      );
    },
    {
      failure: "could not ignore this tile",
      success: (_result, app) => ({
        title: `"${app.name}" ignored`,
        body: "Disabled fleet-wide and will not be re-published by a future scan.",
      }),
      onSuccess: () => setIgnoreTarget(null),
    },
  );

  // Import writes rule='allow' — the same "un-ignore" call the app editor's
  // Library panel already makes (LibraryPanel.tsx), which outranks the
  // built-in denylist and any prior 'ignore' row without deleting it. It only
  // writes the rule; the reconciler publishes the tile on the next scan, so
  // the row stays in this list until then (§8.2) — the toast says so rather
  // than pretending the import is immediate.
  const importPending = useAdminAction<[PendingImportItem], void>(
    async (p) => {
      if (!token) throw new Error("not authenticated");
      await adminApi.setLibraryRule(token, p.providerAppId, p.item.external_id, {
        rule: "allow",
        external_source: p.item.external_source,
      });
    },
    {
      success: (_r, p) => `"${p.item.name || p.item.external_id}" will publish on the next scan`,
      failure: "could not write the allow rule",
      onSuccess: () => void pendingRes.refresh({ silent: true }),
    },
  );

  const ignorePending = useAdminAction<[PendingImportItem], void>(
    async (p) => {
      if (!token) throw new Error("not authenticated");
      await adminApi.setLibraryRule(token, p.providerAppId, p.item.external_id, {
        rule: "ignore",
        external_source: p.item.external_source,
      });
    },
    {
      success: (_r, p) => `"${p.item.name || p.item.external_id}" ignored`,
      failure: "could not write the ignore rule",
      onSuccess: () => void pendingRes.refresh({ silent: true }),
    },
  );

  // Toasts are fired by hand rather than through useAdminAction's `success`
  // path: `inert_reason` is a real 200 (nothing to do, not an error) and
  // needs its own wording, not a fabricated failure.
  const scan = useAdminAction<[], void>(
    async () => {
      if (!token) throw new Error("not authenticated");
      const result = await adminApi.forceLibraryScan(token);
      if (result.inert_reason) {
        addToast({ variant: "info", title: "Scan not started", body: result.inert_reason });
      } else if (result.queued > 0) {
        addToast({
          variant: "success",
          title: `${result.queued} scan${result.queued === 1 ? "" : "s"} queued`,
          body: result.skipped > 0 ? `${result.skipped} already queued` : undefined,
        });
      } else {
        addToast({
          variant: "info",
          title: "Nothing new to queue; scans are already waiting on the agent.",
        });
      }
      void pendingRes.refresh({ silent: true });
    },
    { failure: (e) => (e instanceof ApiError ? e.message : "Could not start a scan.") },
  );

  const totalCount = apps.length;
  const enabledCount = apps.filter((a) => a.enabled).length;

  useSectionHead({
    sub: (
      <>
        {enabledCount} of {totalCount} app{totalCount === 1 ? "" : "s"} enabled
        {pendingRes.errorMessage ? (
          " · pending import count unavailable"
        ) : (
          <>
            {" · "}
            {counts.pending} discovered title{counts.pending === 1 ? "" : "s"} not yet imported
          </>
        )}
      </>
    ),
    actions: (
      <>
        <Button variant="ghost" onClick={() => void scan.run()} disabled={scan.pending != null}>
          <IconRefresh /> {scan.pending ? "Scanning…" : "Scan sources"}
        </Button>
        <Button variant="primary" onClick={() => navigate("/admin/library/apps/new")}>
          <IconPlus /> Add app
        </Button>
        <ActionsMenu
          label="More app actions"
          items={[
            {
              key: "reresolve",
              label: "Re-fetch artwork for every app",
              onClick: () => setReresolveOpen(true),
            },
          ]}
        />
      </>
    ),
    counts: { apps: totalCount },
  });

  // A failed provider fan-out must not silently read as "0 discovered
  // titles" — it blocks the page like the apps load does, rather than
  // rendering a table that quietly undercounts pending imports.
  const errorMessage = appsRes.errorMessage ?? pendingRes.errorMessage;
  const emptyCatalog = !appsRes.loading && totalCount === 0 && pending.length === 0;

  return (
    <section className="page">
      <ResourceStates loading={appsRes.loading} error={errorMessage} />

      {!appsRes.loading && !errorMessage && emptyCatalog && (
        <div className="empty">
          <h3>No apps yet</h3>
          <p>Create one by hand, or scan a source to discover titles automatically.</p>
          <Button variant="primary" onClick={() => navigate("/admin/library/apps/new")}>
            Add app
          </Button>
        </div>
      )}

      {!appsRes.loading && !errorMessage && !emptyCatalog && (
        <>
          <div className="toolbar">
            <SegmentedControl<AppSegment>
              aria-label="Apps filter"
              options={SEGMENTS.map((s) => ({
                value: s.value,
                label: s.countKey ? (
                  <>
                    {s.label} <span className="num" style={{ opacity: 0.7 }}>{counts[s.countKey]}</span>
                  </>
                ) : (
                  s.label
                ),
              }))}
              value={segment}
              onChange={setSegment}
            />
            <SearchInput
              placeholder="Filter apps"
              aria-label="Filter apps"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
            <div className="right">
              <select
                className="select"
                aria-label="Filter by source"
                value={source}
                onChange={(e) => setSource(e.target.value as AppSourceFilter)}
              >
                <option value="all">All sources</option>
                <option value="steam">Steam</option>
                <option value="manual">Manual</option>
              </select>
            </div>
          </div>

          {presetId && (
            <div className="toolbar">
              <span className="chip chip-accent" style={{ height: 26 }}>
                Runtime preset: {presetName}
                <button
                  type="button"
                  onClick={clearPreset}
                  aria-label="Clear runtime preset filter"
                  style={{
                    border: 0,
                    background: "none",
                    color: "inherit",
                    cursor: "pointer",
                    marginLeft: 4,
                    fontSize: 11,
                  }}
                >
                  ✕
                </button>
              </span>
              <span className="hint">
                {filtered.apps.length} of {totalCount} apps
              </span>
            </div>
          )}

          <div className="table-wrap">
            <table className="qtable">
              <thead>
                <tr>
                  <th>App</th>
                  <th>Kind</th>
                  <th>Source</th>
                  <th>Runtime preset</th>
                  <th>Launch profile</th>
                  <th className="right">Sessions · 30d</th>
                  <th className="right">Enabled</th>
                  <th><span className="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                {filtered.pending.map((p) => (
                  <PendingImportRow
                    key={`${p.providerAppId}:${p.item.external_source}:${p.item.external_id}`}
                    row={p}
                    importing={isPendingRowBusy(importPending.pending, p)}
                    onImport={() => void importPending.run(p)}
                    onIgnore={() => void ignorePending.run(p)}
                  />
                ))}
                {filtered.apps.map((app) => (
                  <AppRow
                    key={app.id}
                    app={app}
                    coverClass={coverClassById.get(app.id) ?? "cv-violet"}
                    presetName={app.runtime_preset_id ? (presetNameById.get(app.runtime_preset_id) ?? app.runtime_preset_id) : null}
                    profileName={
                      app.default_profile_id
                        ? (profileNameById.get(app.default_profile_id) ?? "Default")
                        : "Default"
                    }
                    toggling={toggle.pending?.[0].id === app.id}
                    onToggleEnabled={() => void toggle.run(app)}
                    onOpen={() => navigate(`/admin/library/apps/${app.id}`)}
                    onIgnore={app.parent_app_id ? () => setIgnoreTarget(app) : undefined}
                    onDelete={() => setDeleteTarget(app)}
                  />
                ))}
                {filtered.apps.length === 0 && filtered.pending.length === 0 && (
                  <tr>
                    <td colSpan={8} style={{ textAlign: "center", color: "var(--text-3)", padding: "var(--s8)" }}>
                      {query ? `No apps matching "${query}"` : "No apps match these filters."}
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </>
      )}

      {/* Catalogue-wide artwork re-fetch (#385 item 1). Confirmed, never
          automatic: it spends third-party requests. Locked apps are always
          skipped server-side; force is off by default since overwriting a
          correction is normally a per-app decision. */}
      {reresolveOpen && (
        <Modal
          open
          onClose={() => setReresolveOpen(false)}
          title="Re-fetch artwork for every app"
          footer={
            <>
              <Button variant="ghost" onClick={() => setReresolveOpen(false)}>
                Cancel
              </Button>
              <Button
                variant="primary"
                disabled={reresolving}
                onClick={() => {
                  if (!token) return;
                  setReresolving(true);
                  adminApi
                    .reresolveAllArtwork(token, reresolveForce)
                    .then((res) => {
                      setReresolveOpen(false);
                      addToast({
                        variant: "success",
                        title:
                          `Re-fetched artwork for ${res.resolved} of ${res.total} apps · ` +
                          `${res.skipped_locked} left alone (locked)` +
                          (res.failed > 0
                            ? ` · ${res.failed} unmatched · the background sweep will retry`
                            : ""),
                      });
                    })
                    .catch((e: unknown) => {
                      addToast({
                        variant: "danger",
                        title: e instanceof ApiError ? e.message : "Could not re-fetch artwork.",
                      });
                    })
                    .finally(() => setReresolving(false));
                }}
              >
                {reresolving ? "Re-fetching…" : "Re-fetch"}
              </Button>
            </>
          }
        >
          <p>
            Clears and re-fetches the library tile and hero art for every app from the configured
            artwork provider. Locked apps, those with an admin correction or upload, are skipped
            unless force is on.
          </p>
          <label className="row gap3 center" style={{ marginTop: "var(--s3)", cursor: "pointer" }}>
            <input
              type="checkbox"
              checked={reresolveForce}
              onChange={(e) => setReresolveForce(e.target.checked)}
            />
            Overwrite locked artwork too
          </label>
          <p className="muted mt3" style={{ fontSize: "var(--t-sm)" }}>
            Requests are spaced out, so a large catalogue takes a moment.
          </p>
        </Modal>
      )}

      {deleteTarget && (
        <DeleteAppModal
          target={deleteTarget}
          pending={del.pending != null}
          onClose={() => setDeleteTarget(null)}
          onConfirm={() => void del.run(deleteTarget)}
        />
      )}

      {ignoreTarget && (
        <IgnoreAppModal
          target={ignoreTarget}
          pending={ignore.pending != null}
          onClose={() => setIgnoreTarget(null)}
          onConfirm={() => void ignore.run(ignoreTarget)}
        />
      )}
    </section>
  );
}
