// Image detail page — v3 handoff §A.14 (pageImageDetail()). Sits outside the
// Library section container: its own crumbs and head, like the app editor
// (library/apps/:id).
//
// "Last pulled" has no backing field on CatalogImage (openapi.yaml) — shown
// as "not tracked" rather than a fabricated date.

import { useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import type { AdminApp, CatalogImage, Host, ImageUpdatePolicy, RuntimePreset } from "../../api/types";
import { useAuth } from "../../auth/context";
import { Bar } from "../../components/Bar";
import { Breadcrumbs } from "../../components/Breadcrumbs";
import { Button } from "../../components/Button";
import { IconDownload, IconPin, IconRefresh, IconTrash } from "../../components/icons";
import { Modal } from "../../components/Modal";
import { PageHeader } from "../../components/PageHeader";
import { ResourceStates } from "../../components/ResourceStates";
import { Switch } from "../../components/TextField";
import { Table, type TableColumn } from "../../components/Table";
import { useToast } from "../../components/Toast";
import { useFleetContext } from "../../lib/fleet/FleetContext";
import { useResource } from "../../lib/resource/react";
import { hostImageState, imgRollout, POLICY_COPY } from "./library/imageRollout";
import { dominantInFlightState, HOST_STATE_COPY, isImageInFlight } from "./library/imageStatus";

interface DetailData {
  images: CatalogImage[];
  presets: RuntimePreset[];
  apps: AdminApp[];
  policy: ImageUpdatePolicy;
}

/** `AdminApp.runtime_spec` is an opaque bag (openapi.yaml) — only ever read
 *  here as an optional `image` string, never assumed to have one. */
function appImageRef(app: AdminApp): string | undefined {
  const v = (app.runtime_spec as Record<string, unknown> | null)?.image;
  return typeof v === "string" ? v : undefined;
}

const HOST_STATE_LABEL: Record<ReturnType<typeof hostImageState>, string> = {
  ready: "ready",
  stale: "stale",
  pulling: "pulling",
  building: "building",
  failed: "failed",
  absent: "not installed",
};

function kindLabel(kind: CatalogImage["kind"]): string {
  return kind === "prebuilt" ? "Prebuilt" : "Template";
}

export function ImageDetail() {
  const { id } = useParams();
  const { token } = useAuth();
  const { addToast } = useToast();
  const navigate = useNavigate();

  const resource = useResource(
    {
      label: "image detail",
      initialData: null as DetailData | null,
      fetch: async (ctx) => {
        const [imagesEnv, presetsEnv, appsEnv, settingsEnv] = await Promise.all([
          adminApi.listImages(ctx.token),
          adminApi.listRuntimePresets(ctx.token),
          adminApi.listAdminApps(ctx.token),
          adminApi.getSettings(ctx.token),
        ]);
        return {
          images: imagesEnv.images,
          presets: presetsEnv.items,
          apps: appsEnv.items,
          // instance_settings.image_update_policy default 'notify' (migration 0054).
          policy: settingsEnv.settings.image_update_policy ?? "notify",
        };
      },
      // Poll only while something is mid-install, same as ImagesTab.
      pollMs: (d) => (d?.images.some(isImageInFlight) ? 4000 : null),
    },
    [id],
  );

  const [pending, setPending] = useState<"install" | "update" | "uninstall" | "pin" | null>(null);
  const [uninstallOpen, setUninstallOpen] = useState(false);
  // "Pull on first launch" beside the lead Install action — eager (false) by
  // default, matching ImagesTab's gbar install. Re-ensuring an already
  // installed image always goes eager; the switch only governs a fresh install.
  const [lazy, setLazy] = useState(false);

  const data = resource.data;
  const img = useMemo(() => data?.images.find((i) => i.id === id), [data, id]);
  // Off the fleet poll above this page (lib/fleet), not a read of its own.
  const { hosts } = useFleetContext();

  const roll = img ? imgRollout(img, hosts) : null;

  const usedByPresets = useMemo(
    () => (img ? (data?.presets ?? []).filter((p) => p.image === img.registry_ref) : []),
    [data, img],
  );
  const usedByPresetIds = useMemo(() => new Set(usedByPresets.map((p) => p.id)), [usedByPresets]);
  const usedByApps = useMemo(
    () =>
      img
        ? (data?.apps ?? []).filter(
            (a) => appImageRef(a) === img.registry_ref || (a.runtime_preset_id && usedByPresetIds.has(a.runtime_preset_id)),
          )
        : [],
    [data, img, usedByPresetIds],
  );

  async function withPending<T>(kind: typeof pending, fn: () => Promise<T>): Promise<T | undefined> {
    setPending(kind);
    try {
      return await fn();
    } catch (e) {
      addToast({ variant: "danger", title: e instanceof ApiError ? e.message : "Something went wrong." });
      return undefined;
    } finally {
      setPending(null);
    }
  }

  async function handleInstall(installLazy: boolean) {
    if (!token || !img) return;
    await withPending("install", async () => {
      const installed = await resource.mutate(
        (ctx) => adminApi.installImage(ctx.token, img.id, { lazy: installLazy }),
        (d, result) => (d ? { ...d, images: d.images.map((i) => (i.id === result.id ? result : i)) } : d),
      );
      addToast({
        variant: "success",
        title: `${installed.display_name} ${installLazy ? "queued for lazy install" : "installed"}`,
        body: installed.library_provider
          ? `Enable ${installed.library_provider} library discovery in Settings for it to appear in the library.`
          : undefined,
      });
    });
  }

  async function handleReEnsure() {
    if (!token || !img) return;
    await handleInstall(false);
  }

  async function handleUpdate() {
    if (!token || !img) return;
    await withPending("update", async () => {
      const result = await resource.mutate(
        (ctx) => adminApi.updateImage(ctx.token, img.id),
        (d, r) => (d ? { ...d, images: d.images.map((i) => (i.id === r.image.id ? r.image : i)) } : d),
      );
      addToast({ variant: "success", title: result.applied ? `${img.display_name} updated` : `${img.display_name} is already current` });
    });
  }

  async function handlePinToggle() {
    if (!token || !img) return;
    const nextPinned = !img.pinned;
    await withPending("pin", async () => {
      if (nextPinned) await resource.mutate((ctx) => adminApi.pinImage(ctx.token, img.id));
      else await resource.mutate((ctx) => adminApi.unpinImage(ctx.token, img.id));
      resource.refresh({ silent: true });
      addToast({ variant: "success", title: `${img.display_name} ${nextPinned ? "pinned" : "unpinned"}` });
    });
  }

  async function handleUninstall() {
    if (!token || !img) return;
    await withPending("uninstall", async () => {
      await resource.mutate((ctx) => adminApi.uninstallImage(ctx.token, img.id));
      setUninstallOpen(false);
      resource.refresh({ silent: true });
      addToast({ variant: "success", title: `${img.display_name} uninstalled` });
    });
  }

  const hostColumns: TableColumn<Host>[] = img
    ? [
        {
          key: "host",
          header: "Host",
          render: (h) => {
            const st = hostImageState(img, h.id);
            const dotClass = st === "ready" ? "ok" : st === "stale" ? "warn" : st === "failed" ? "bad" : st === "absent" ? "off" : "info";
            return (
              <div className="rowflex">
                <i className={`sdot ${dotClass}`} title={HOST_STATE_LABEL[st]} />
                <Link to={`/admin/fleet/hosts/${h.id}`} className="primary">{h.node_name}</Link>
              </div>
            );
          },
        },
        {
          key: "state",
          header: "State",
          render: (h) => {
            const st = hostImageState(img, h.id);
            const color = st === "stale" ? "var(--warning-text)" : st === "absent" ? "var(--text-4)" : undefined;
            return <span style={{ color }}>{HOST_STATE_LABEL[st]}</span>;
          },
        },
        {
          key: "version",
          header: "Version",
          align: "right",
          render: (h) => {
            const st = hostImageState(img, h.id);
            if (st === "absent" || st === "pulling") return <span className="num">—</span>;
            // A stale host is running its own reported version, not the
            // image's installed_version (that is the version everyone else
            // converged to, which is exactly what "stale" means it isn't).
            if (st === "stale") {
              const hostVersion = img.hosts?.find((hs) => hs.host_id === h.id)?.version;
              return <span className="num">{hostVersion}</span>;
            }
            return <span className="num">{img.installed_version || img.version}</span>;
          },
        },
        {
          key: "sessions",
          header: "Sessions",
          align: "right",
          render: (h) => <span className="num">{h.capacity?.active_sessions ?? "—"}</span>,
        },
      ]
    : [];

  const installable = img ? img.installed || img.kind === "prebuilt" : true;
  const notInstallableTitle = "Not installable yet. Template builds ship in a later phase.";
  // A pinned image 409s "image is pinned" on update — fall back to Re-ensure
  // rather than a lead action that can only fail.
  const isUpdate = img ? img.installed && img.update_available && !img.pinned : false;
  // Wire-derived, not local `pending` — a background pull (e.g. a provider
  // auto-ensure) is in flight whether or not this tab is the one that started it.
  const inFlight = img ? dominantInFlightState(img) : null;

  return (
    <section className="page">
      <Breadcrumbs
        items={[
          { label: "Library", to: "/admin/library/apps" },
          { label: "Images", to: "/admin/library/images" },
          { label: img?.display_name ?? "…" },
        ]}
      />

      <ResourceStates loading={resource.loading} error={resource.errorMessage} />

      {!resource.loading && !resource.errorMessage && !img && (
        <p className="note">This image no longer exists in the catalog.</p>
      )}

      {img && roll && (
        <>
          <PageHeader
            title={img.display_name}
            sub={`${kindLabel(img.kind)} · from ${img.library_provider ? img.library_provider[0].toUpperCase() + img.library_provider.slice(1) : "Manual"} · ${roll.ready} of ${roll.total} hosts ready`}
            actions={
              <>
                {isUpdate ? (
                  <Button variant="primary" disabled={pending !== null} onClick={() => void handleUpdate()}>
                    <IconRefresh />
                    Update to {img.version}
                  </Button>
                ) : inFlight ? (
                  <Button variant="ghost" disabled>{HOST_STATE_COPY[inFlight].label}</Button>
                ) : !img.installed ? (
                  <>
                    <Button
                      variant="primary"
                      disabled={pending !== null || !installable}
                      title={!installable ? notInstallableTitle : undefined}
                      onClick={() => void handleInstall(lazy)}
                    >
                      <IconDownload />
                      Install on every host
                    </Button>
                    <Switch checked={lazy} onChange={setLazy} label="Pull on first launch" id="image-detail-lazy" />
                  </>
                ) : (
                  <Button variant="ghost" disabled={pending !== null} onClick={() => void handleReEnsure()}>
                    <IconRefresh />
                    Re-ensure
                  </Button>
                )}
                <Button variant="ghost" disabled={pending !== null || !img.installed} onClick={() => void handlePinToggle()}>
                  <IconPin />
                  {img.pinned ? "Unpin" : "Pin version"}
                </Button>
              </>
            }
          />

          <div className="editor">
            <div style={{ display: "flex", flexDirection: "column", gap: "var(--s4)" }}>
              <div className="card card-pad">
                <p style={{ fontSize: "var(--t-sm)", color: "var(--text-2)", lineHeight: 1.55, margin: "0 0 var(--s4)", maxWidth: "70ch" }}>
                  {img.description}
                </p>
                <div className="ae-facts">
                  <div className="ae-fact"><span>Reference</span><span className="mono">{img.registry_ref ?? "—"}</span></div>
                  <div className="ae-fact"><span>Digest</span><span className="mono">{img.registry_digest ?? "—"}</span></div>
                  <div className="ae-fact"><span>Catalog version</span><span className="num">{img.version}</span></div>
                  <div className="ae-fact"><span>Installed version</span><span>{img.installed_version ? <span className="num">{img.installed_version}</span> : <span style={{ color: "var(--text-4)" }}>not installed</span>}</span></div>
                  <div className="ae-fact"><span>Install mode</span><span>{img.installed ? (img.lazy ? "on first launch" : "eager") : "—"}</span></div>
                  <div className="ae-fact"><span>Last pulled</span><span>not tracked</span></div>
                </div>
              </div>

              <div className="card">
                <div className="panel-head">
                  <span className="panel-title">Per host</span>
                  <div className="acts hint">{roll.ready} of {roll.total} ready</div>
                </div>
                <Table
                  columns={hostColumns}
                  rows={hosts}
                  rowKey={(h) => h.id}
                  onRowClick={(h) => navigate(`/admin/fleet/hosts/${h.id}`)}
                />
              </div>

              <div className="card">
                <div className="panel-head">
                  <span className="panel-title">Used by</span>
                  <div className="acts hint">{usedByPresets.length} presets · {usedByApps.length} apps</div>
                </div>
                {usedByPresets.length || usedByApps.length ? (
                  <div className="card-pad" style={{ display: "flex", gap: "var(--s7)", flexWrap: "wrap" }}>
                    <div style={{ flex: "1 1 220px" }}>
                      <div className="eyebrow" style={{ marginBottom: 9 }}>Runtime presets</div>
                      {usedByPresets.length ? (
                        usedByPresets.map((p) => (
                          <div className="ae-fact" key={p.id}>
                            <Link to="/admin/library/presets">{p.name}</Link>
                            <span className="sub">preset</span>
                          </div>
                        ))
                      ) : (
                        <div className="sub">None</div>
                      )}
                    </div>
                    <div style={{ flex: "1 1 220px" }}>
                      <div className="eyebrow" style={{ marginBottom: 9 }}>Apps</div>
                      {usedByApps.length ? (
                        usedByApps.map((a) => (
                          <div className="ae-fact" key={a.id}>
                            <Link to={`/admin/library/apps/${a.id}`}>{a.name}</Link>
                            <span className="sub">app</span>
                          </div>
                        ))
                      ) : (
                        <div className="sub">None</div>
                      )}
                    </div>
                  </div>
                ) : (
                  <div className="card-pad">
                    <div className="note">
                      Nothing points at this image.{img.installed ? " Uninstalling it reclaims the space on every host." : ""}
                    </div>
                  </div>
                )}
              </div>
            </div>

            <div className="ae-rail">
              <div className="card card-pad">
                <div className="eyebrow">Rollout</div>
                <div style={{ marginTop: 10 }}>
                  <Bar percent={roll.total > 0 ? (roll.ready / roll.total) * 100 : 0} label={`${roll.ready}/${roll.total}`} variant={roll.tone} />
                </div>
                <div className="ae-facts" style={{ marginTop: "var(--s4)" }}>
                  <div className="ae-fact">
                    <span>State</span>
                    <span style={{ color: inFlight ? "var(--info-text)" : img.update_available ? "var(--warning-text)" : undefined }}>
                      {inFlight
                        ? HOST_STATE_COPY[inFlight].label
                        : img.update_available
                          ? "update available"
                          : img.installed
                            ? "up to date"
                            : "not installed"}
                    </span>
                  </div>
                  <div className="ae-fact"><span>Version pinned</span><span>{img.pinned ? "yes" : "no"}</span></div>
                  <div className="ae-fact">
                    <span>Update policy</span>
                    <Link to="/admin/library/images">{data ? POLICY_COPY[data.policy].label : "…"}</Link>
                  </div>
                </div>
              </div>
              {(img.installed || inFlight !== null) && (
                <>
                  <Button variant="danger" style={{ width: "100%", justifyContent: "center" }} onClick={() => setUninstallOpen(true)}>
                    <IconTrash />
                    Uninstall everywhere
                  </Button>
                  <p className="hint">
                    Removes the image from every connected host. Presets and apps that point at it stop launching until it is reinstalled.
                  </p>
                </>
              )}
            </div>
          </div>
        </>
      )}

      {uninstallOpen && img && (
        <Modal
          open
          onClose={() => setUninstallOpen(false)}
          title="Uninstall image"
          footer={
            <>
              <Button variant="ghost" onClick={() => setUninstallOpen(false)}>Cancel</Button>
              <Button variant="danger" disabled={pending === "uninstall"} onClick={() => void handleUninstall()}>
                {pending === "uninstall" ? "Uninstalling…" : "Uninstall"}
              </Button>
            </>
          }
        >
          <p className="sec">
            This removes <strong>{img.display_name}</strong> from every connected host
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
