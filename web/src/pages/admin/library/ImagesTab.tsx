// Images tab — v3 handoff §A.13 (pageImages()): the catalog table with the
// compact per-row `.gbar` actions, the update-policy card and the catalog
// stats block. P1 (read-only catalog) + P3 (install/uninstall/pin/update,
// per-host state, instance update policy). Authorization is server-enforced;
// this UI is UX only (CLAUDE.md invariant #6).
//
// The gbar has no lazy-install toggle (the mock's compact action bar has no
// room for one) — every gbar install goes eager. Lazy install lives on the
// image detail page instead. Per-row action errors surface as a toast, not
// inline text — there is no room for one in a 26px icon row.

import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { CatalogImage, ImageUpdatePolicy, ManifestProvenance } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Bar } from "../../../components/Bar";
import { Button } from "../../../components/Button";
import { IconChevronRight, IconDownload, IconPin, IconRefresh, IconTrash } from "../../../components/icons";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { SearchInput } from "../../../components/TextField";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Table, type TableColumn } from "../../../components/Table";
import { useToast } from "../../../components/Toast";
import { useFleetContext } from "../../../lib/fleet/FleetContext";
import { useResource } from "../../../lib/resource/react";
import { fmtClockTime, fmtDate } from "../../../lib/formatLegacy";
import { relativeTime } from "../../../lib/format/relativeTime";
import { imgRollout, imgVersion, hostImageState, POLICY_COPY } from "./imageRollout";
import { isImageInFlight } from "./imageStatus";
import { useSectionHead } from "../../../components/shell/sectionHead";

type RowAction = "install" | "uninstall" | "pin" | "update";
type Segment = "all" | "installed" | "updates";

interface RowUiState {
  pending: RowAction | null;
}

const EMPTY_ROW_UI: RowUiState = { pending: null };

/** 64-hex digests are never compared by eye. Show the 12-char short form with
 *  the full value in `title` for copy-out. */
function Digest({ sha }: { sha: string }) {
  return <span className="mono" title={sha}>{sha.slice(0, 12)}</span>;
}

function PolicyCard({
  policy,
  loadError,
  onRetry,
  saving,
  onChange,
  images,
  fetchedAt,
  provenance,
}: {
  policy: ImageUpdatePolicy | null;
  loadError: string | null;
  onRetry: () => void;
  saving: boolean;
  onChange: (next: ImageUpdatePolicy) => void;
  images: CatalogImage[];
  fetchedAt: string | null;
  provenance: ManifestProvenance | null;
}) {
  const installed = images.filter((i) => i.installed).length;
  const updates = images.filter((i) => i.update_available).length;

  return (
    <div className="card card-pad" style={{ display: "flex", gap: "var(--s7)", alignItems: "center", flexWrap: "wrap" }}>
      <div>
        <div className="eyebrow">Update policy</div>
        {loadError ? (
          <div className="col gap2" style={{ marginTop: 8, alignItems: "flex-start" }}>
            <p className="form-error" role="alert" style={{ margin: 0 }}>{loadError}</p>
            <Button variant="secondary" size="sm" onClick={onRetry}>Retry</Button>
          </div>
        ) : policy ? (
          <div style={{ marginTop: 8 }}>
            <SegmentedControl<ImageUpdatePolicy>
              aria-label="Image update policy"
              value={policy}
              onChange={onChange}
              disabled={saving}
              options={(Object.keys(POLICY_COPY) as ImageUpdatePolicy[]).map((p) => ({
                value: p,
                label: POLICY_COPY[p].label,
              }))}
            />
          </div>
        ) : null}
      </div>
      {policy && <p className="hint" style={{ maxWidth: 430 }}>{POLICY_COPY[policy].desc}</p>}
      <div className="right" style={{ marginLeft: "auto", textAlign: "right" }}>
        <div className="eyebrow">Catalog</div>
        <div style={{ fontSize: "var(--t-sm)", color: "var(--text-2)", marginTop: 6 }}>
          {images.length} image{images.length === 1 ? "" : "s"} · {installed} installed
          {updates > 0 && (
            <> · <span style={{ color: "var(--warning-text)" }}>{updates} update{updates === 1 ? "" : "s"} available</span></>
          )}
        </div>
        <p className="hint">Last synced {fmtDate(fetchedAt)}, {fmtClockTime(fetchedAt)}</p>
        {provenance && (
          <>
            <p className="hint" style={{ marginTop: 2 }}>
              Manifest <Digest sha={provenance.sha256} /> at ref <span className="mono">{provenance.ref}</span>
              {provenance.commit_sha ? (
                <> · commit <Digest sha={provenance.commit_sha} /></>
              ) : (
                <span className="muted"> · commit unresolved</span>
              )}
            </p>
            <p className="hint" style={{ fontSize: "var(--t-xs)", wordBreak: "break-all", maxWidth: 360, marginTop: 2 }}>
              {provenance.url}
            </p>
          </>
        )}
      </div>
    </div>
  );
}

export function ImagesTab() {
  const { token } = useAuth();
  const { addToast } = useToast();
  const navigate = useNavigate();

  const catalog = useResource({
    label: "image catalog",
    initialData: null as {
      images: CatalogImage[];
      sync_error: string | null;
      fetched_at: string | null;
      provenance: ManifestProvenance | null;
    } | null,
    fetch: async (ctx) => {
      const envelope = await adminApi.listImages(ctx.token);
      return {
        images: envelope.images,
        sync_error: envelope.sync_error ?? null,
        fetched_at: envelope.fetched_at ?? null,
        provenance: envelope.manifest_provenance ?? null,
      };
    },
    // Poll while anything is mid-install so a background pull (e.g. a
    // provider auto-ensure, #455) corrects the page; stops once every row is
    // terminal.
    pollMs: (data) => (data?.images.some(isImageInFlight) ? 4000 : null),
  });
  const images = catalog.data?.images ?? [];

  // The fleet poll above this page already holds the hosts (lib/fleet).
  const { hosts } = useFleetContext();

  const [policySaving, setPolicySaving] = useState(false);
  const settingsRes = useResource({
    label: "instance settings",
    initialData: null as ImageUpdatePolicy | null,
    fetch: async (ctx) => {
      const { settings } = await adminApi.getSettings(ctx.token);
      // instance_settings.image_update_policy default 'notify' (migration 0054).
      return settings.image_update_policy ?? "notify";
    },
  });
  const policy = settingsRes.data ?? null;

  const [query, setQuery] = useState("");
  const [segment, setSegment] = useState<Segment>("all");
  const [hostFilter, setHostFilter] = useState("");
  const [rowUi, setRowUi] = useState<Record<string, RowUiState>>({});
  const [uninstallTarget, setUninstallTarget] = useState<CatalogImage | null>(null);
  const [syncing, setSyncing] = useState(false);

  const uiFor = (id: string): RowUiState => rowUi[id] ?? EMPTY_ROW_UI;
  const patchUi = (id: string, patch: Partial<RowUiState>) => {
    setRowUi((prev) => ({ ...prev, [id]: { ...(prev[id] ?? EMPTY_ROW_UI), ...patch } }));
  };

  const installedCount = images.filter((i) => i.installed).length;
  const updatesCount = images.filter((i) => i.update_available).length;

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return images.filter((img) => {
      if (segment === "installed" && !img.installed) return false;
      if (segment === "updates" && !img.update_available) return false;
      if (hostFilter && hostImageState(img, hostFilter) === "absent") return false;
      if (q && !img.display_name.toLowerCase().includes(q) && !(img.registry_ref ?? "").toLowerCase().includes(q)) return false;
      return true;
    });
  }, [images, segment, hostFilter, query]);

  async function handleSyncPolicyChange(next: ImageUpdatePolicy) {
    if (!token || policy === next || policySaving) return;
    setPolicySaving(true);
    try {
      await settingsRes.mutate(
        (ctx) => adminApi.updateSettings(ctx.token, { image_update_policy: next }),
        (_data, result) => result.settings.image_update_policy ?? next,
      );
      addToast({ variant: "success", title: `Update policy set to "${POLICY_COPY[next].label}"` });
    } catch (e) {
      addToast({
        variant: "danger",
        title: "Could not update the image update policy",
        body: e instanceof ApiError ? e.message : undefined,
      });
    } finally {
      setPolicySaving(false);
    }
  }

  async function handleSync() {
    if (!token) return;
    setSyncing(true);
    try {
      // sync_error arrives inside a 200 (a manifest sync failure never blocks
      // the cached catalog); the catch below is for the request itself failing.
      const result = await catalog.mutate(
        (ctx) => adminApi.syncImages(ctx.token),
        (_data, envelope) => ({
          images: envelope.images,
          sync_error: envelope.sync_error ?? null,
          fetched_at: envelope.fetched_at ?? null,
          provenance: envelope.manifest_provenance ?? null,
        }),
      );
      if (result.sync_error) {
        addToast({ variant: "danger", title: "Last sync failed", body: result.sync_error });
      } else {
        addToast({ variant: "success", title: "Catalog synced" });
      }
    } catch (e) {
      addToast({ variant: "danger", title: e instanceof ApiError ? e.message : "Could not reach the control plane to sync." });
    } finally {
      setSyncing(false);
    }
  }

  // 409 digest_unresolved: install refused until a fresh sync resolves the
  // content digest (image-management P3 spec §Digest pinning).
  function digestHint(err: ApiError): string {
    if (err.code !== "digest_unresolved") return "";
    const sep = err.message.endsWith(".") ? "" : ".";
    return `${sep} Try syncing the catalog again. A fresh sync resolves the registry digest.`;
  }

  function installErrorMessage(img: CatalogImage, err: ApiError): string {
    if (err.code === "already_installed") {
      return `${img.display_name} was just installed, likely by another admin action (e.g. the Steam integration's auto-install). Refreshing…`;
    }
    return err.message + digestHint(err);
  }

  async function handleInstall(img: CatalogImage) {
    if (!token) return;
    patchUi(img.id, { pending: "install" });
    try {
      const installed = await catalog.mutate(
        (ctx) => adminApi.installImage(ctx.token, img.id, { lazy: false }),
        (data, result) => ({ ...data!, images: data!.images.map((i) => (i.id === result.id ? result : i)) }),
      );
      addToast({
        variant: "success",
        title: `${installed.display_name} installed`,
        body: installed.library_provider
          ? `Enable ${installed.library_provider} library discovery in Settings for it to appear in the library.`
          : undefined,
      });
    } catch (e) {
      const msg = e instanceof ApiError ? installErrorMessage(img, e) : "Could not install this image.";
      addToast({ variant: "danger", title: msg });
      // already_installed = our catalog is stale; refetch so the row corrects
      // itself instead of waiting for the next poll tick.
      if (e instanceof ApiError && e.code === "already_installed") catalog.refresh({ silent: true });
    } finally {
      patchUi(img.id, { pending: null });
    }
  }

  async function handleUninstall(img: CatalogImage) {
    if (!token) return;
    patchUi(img.id, { pending: "uninstall" });
    try {
      await catalog.mutate((ctx) => adminApi.uninstallImage(ctx.token, img.id));
      addToast({ variant: "success", title: `${img.display_name} uninstalled` });
      setUninstallTarget(null);
      catalog.refresh({ silent: true });
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : "Could not uninstall this image.";
      addToast({ variant: "danger", title: msg });
      setUninstallTarget(null);
    } finally {
      patchUi(img.id, { pending: null });
    }
  }

  async function handlePinToggle(img: CatalogImage, nextPinned: boolean) {
    if (!token) return;
    patchUi(img.id, { pending: "pin" });
    try {
      if (nextPinned) await catalog.mutate((ctx) => adminApi.pinImage(ctx.token, img.id));
      else await catalog.mutate((ctx) => adminApi.unpinImage(ctx.token, img.id));
      addToast({ variant: "success", title: `${img.display_name} ${nextPinned ? "pinned" : "unpinned"}` });
      catalog.refresh({ silent: true });
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : "Could not change the pin state.";
      addToast({ variant: "danger", title: msg });
    } finally {
      patchUi(img.id, { pending: null });
    }
  }

  async function handleUpdate(img: CatalogImage) {
    if (!token) return;
    patchUi(img.id, { pending: "update" });
    try {
      // `applied:false` on a 200 (still success) is a no-op — the installed
      // version already equalled the catalog version.
      const result = await catalog.mutate(
        (ctx) => adminApi.updateImage(ctx.token, img.id),
        (data, r) => ({ ...data!, images: data!.images.map((i) => (i.id === r.image.id ? r.image : i)) }),
      );
      addToast({
        variant: "success",
        title: result.applied ? `${img.display_name} updated` : `${img.display_name} is already current`,
      });
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : "Could not update this image.";
      addToast({ variant: "danger", title: msg });
    } finally {
      patchUi(img.id, { pending: null });
    }
  }

  const columns: TableColumn<CatalogImage>[] = [
    {
      key: "name",
      header: "Image",
      render: (img) => (
        <div>
          <div className="primary">{img.display_name}</div>
          <div className="sub mono" style={{ marginTop: 2 }}>{img.registry_ref ?? "—"}</div>
        </div>
      ),
    },
    {
      key: "version",
      header: "Version",
      render: (img) => {
        const v = imgVersion(img);
        return (
          <div>
            <div className="num">{v.version}</div>
            <div className="sub" style={{ color: v.tone === "warning" ? "var(--warning-text)" : v.tone === "info" ? "var(--info-text)" : undefined }}>
              {v.sub}
            </div>
          </div>
        );
      },
    },
    {
      key: "rollout",
      header: "Rollout",
      render: (img) => {
        if (!img.hosts || img.hosts.length === 0) return <span className="muted">—</span>;
        const roll = imgRollout(img, hosts);
        return (
          <div style={{ maxWidth: 132 }}>
            <Bar percent={roll.total > 0 ? (roll.ready / roll.total) * 100 : 0} label={`${roll.ready}/${roll.total}`} variant={roll.tone} />
            <div className="sub" style={{ marginTop: 4 }}>{roll.note}</div>
          </div>
        );
      },
    },
    {
      key: "actions",
      header: "",
      render: (img) => {
        const ui = uiFor(img.id);
        const pending = ui.pending !== null;
        const v = imgVersion(img);
        // A pinned image 409s "image is pinned" on update — the refresh gbtn
        // falls back to a plain re-ensure rather than offering an update that
        // can only fail.
        const isUpdate = img.installed && img.update_available && !img.pinned;
        // The resolver skips non-prebuilt entries, so Install on a template
        // could only ever 409 digest_unresolved. Dim the buttons with an
        // explanatory title rather than wiring a control that always fails.
        const installable = img.installed || img.kind === "prebuilt";
        const notInstallableTitle = "Not installable yet. Template builds ship in a later phase.";
        return (
          <div className="gbar" onClick={(e) => e.stopPropagation()}>
            <button
              type="button"
              className={`gbtn${isUpdate ? " todo" : ""}${installable ? "" : " dim"}`}
              title={
                !installable
                  ? notInstallableTitle
                  : isUpdate
                    ? `Update to ${v.version}`
                    : "Re-ensure on every host"
              }
              aria-label={isUpdate ? `Update to ${v.version}` : "Re-ensure on every host"}
              disabled={pending || !installable}
              onClick={() => void (isUpdate ? handleUpdate(img) : handleInstall(img))}
            >
              <IconRefresh />
            </button>
            {img.installed || ui.pending === "install" || isImageInFlight(img) ? (
              <button
                type="button"
                className="gbtn rm"
                title="Uninstall everywhere"
                aria-label="Uninstall everywhere"
                disabled={pending}
                onClick={() => setUninstallTarget(img)}
              >
                <IconTrash />
              </button>
            ) : (
              <button
                type="button"
                className={`gbtn${installable ? " todo" : " dim"}`}
                title={installable ? "Install on every host" : notInstallableTitle}
                aria-label="Install on every host"
                disabled={pending || !installable}
                onClick={() => void handleInstall(img)}
              >
                <IconDownload />
              </button>
            )}
            <button
              type="button"
              className={`gbtn${img.pinned ? " on" : ""}`}
              title={img.pinned ? "Unpin version" : `Pin to ${img.installed_version || img.version}`}
              aria-label={img.pinned ? "Unpin version" : `Pin to ${img.installed_version || img.version}`}
              disabled={pending || !img.installed}
              onClick={() => void handlePinToggle(img, !img.pinned)}
            >
              <IconPin />
            </button>
            <button
              type="button"
              className="gbtn"
              title="Open image"
              aria-label="Open image"
              onClick={() => navigate(`/admin/library/images/${img.id}`)}
            >
              <IconChevronRight />
            </button>
          </div>
        );
      },
    },
  ];

  // The head is the Library section's (../Library.tsx); this tab fills it in.
  useSectionHead({
    sub: "Container images mirrored from the quasar-images catalog. Installs, updates and uninstalls apply to every connected host.",
    actions: (
      <Button variant="primary" disabled={syncing || !token} onClick={() => void handleSync()}>
        <IconRefresh />
        {syncing ? "Syncing…" : "Sync catalog"}
      </Button>
    ),
    counts: { images: images.length },
  });

  return (
    <section className="page">
      <ResourceStates loading={catalog.loading} error={catalog.errorMessage} />

      {!catalog.loading && catalog.data && (
        <>
          {catalog.data.sync_error && (
            <p className="note warn" role="alert" style={{ marginBottom: "var(--s4)" }}>
              <strong>Last sync failed.</strong> {catalog.data.sync_error} The catalog below is the last
              successfully cached version, not necessarily current.
            </p>
          )}

          {/* #548: the manifest is fetched unauthenticated at a MUTABLE ref, so an
              upstream force-push silently changes what every host installs. Signing
              was ruled out (operator decision 2026-08-28); this alert is the whole
              mitigation, so a changed digest must be impossible to miss. */}
          {catalog.data.provenance?.changed && (
            <p className="note warn" role="alert" style={{ marginBottom: "var(--s4)" }}>
              <strong>The manifest changed at the last sync.</strong>{" "}
              {catalog.data.provenance.previous_sha256 ? (
                <Digest sha={catalog.data.provenance.previous_sha256} />
              ) : (
                "unknown"
              )}{" "}
              → <Digest sha={catalog.data.provenance.sha256} />
              {catalog.data.provenance.changed_at ? ` (${relativeTime(catalog.data.provenance.changed_at)})` : ""}.
              The catalog is fetched unauthenticated from{" "}
              <span className="mono">{catalog.data.provenance.ref}</span>, a ref that can move. Check that
              this change was expected before installing or updating anything below.
            </p>
          )}

          <PolicyCard
            policy={policy}
            loadError={settingsRes.errorMessage}
            onRetry={() => settingsRes.refresh()}
            saving={policySaving}
            onChange={(next) => void handleSyncPolicyChange(next)}
            images={images}
            fetchedAt={catalog.data.fetched_at}
            provenance={catalog.data.provenance}
          />

          <div className="toolbar">
            <SearchInput
              placeholder="Filter catalog"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              aria-label="Filter catalog"
            />
            <SegmentedControl<Segment>
              options={[
                { value: "all", label: <>All <span className="num" style={{ opacity: 0.7 }}>{images.length}</span></> },
                { value: "installed", label: <>Installed <span className="num" style={{ opacity: 0.7 }}>{installedCount}</span></> },
                { value: "updates", label: <>Updates <span className="num" style={{ opacity: 0.7 }}>{updatesCount}</span></> },
              ]}
              value={segment}
              onChange={setSegment}
              aria-label="Filter by install state"
            />
            <div className="right">
              <select className="select" value={hostFilter} onChange={(e) => setHostFilter(e.target.value)} aria-label="Filter by host">
                <option value="">All hosts</option>
                {hosts.map((h) => (
                  <option key={h.id} value={h.id}>{h.node_name}</option>
                ))}
              </select>
            </div>
          </div>

          <Table
            columns={columns}
            rows={filtered}
            rowKey={(img) => img.id}
            empty={query ? `No images matching "${query}"` : "No images in the catalog yet. Try Sync catalog."}
            onRowClick={(img) => navigate(`/admin/library/images/${img.id}`)}
          />
        </>
      )}

      {uninstallTarget && (
        <Modal
          open
          onClose={() => setUninstallTarget(null)}
          title="Uninstall image"
          footer={
            <>
              <Button variant="ghost" onClick={() => setUninstallTarget(null)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={uiFor(uninstallTarget.id).pending === "uninstall"}
                onClick={() => void handleUninstall(uninstallTarget)}
              >
                {uiFor(uninstallTarget.id).pending === "uninstall" ? "Uninstalling…" : "Uninstall"}
              </Button>
            </>
          }
        >
          <p className="sec">
            This removes <strong>{uninstallTarget.display_name}</strong> from every connected host
            that has it and drops the adoption record. It is best effort. A host that never confirms
            the removal keeps the image on disk. Any app relying on this image will fail to launch
            until it is reinstalled.
          </p>
          <p className="sec muted" style={{ fontSize: "var(--t-xs)" }}>
            Reinstalling later re-fetches whatever digest the catalog currently has pinned for this
            image. It is not a fresh build. If you're uninstalling to force a corrected image to be
            pulled again, sync the catalog first so a newer digest is what gets re-adopted.
          </p>
        </Modal>
      )}
    </section>
  );
}
