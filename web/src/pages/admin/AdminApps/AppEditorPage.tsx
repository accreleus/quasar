// The app editor (handoff §A.10): six tabs over one draft and one save.
//
// Every tab edits the same `AppDraft`, and appDraft.ts turns it into one PATCH
// of the keys that moved. The page owns the reads, so the tab bar can carry the
// Access and Library counts before either tab is opened. `enabled` writes
// through immediately instead of joining the draft.

import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type {
  AdminApp,
  CatalogImage,
  Entitlement,
  InstanceSettings,
  LaunchProfile,
  LibraryUnpublishedItem,
  RuntimePreset,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Breadcrumbs } from "../../../components/Breadcrumbs";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import { PageHeader } from "../../../components/PageHeader";
import { ResourceStates } from "../../../components/ResourceStates";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import "../../../styles/admin/editor.css";
import { AccessTab } from "./editor/AccessTab";
import { ArtworkTab } from "./editor/ArtworkTab";
import { EditorRail } from "./editor/EditorRail";
import { IdentityTab } from "./editor/IdentityTab";
import { LibraryTab } from "./editor/LibraryTab";
import { QualityTab } from "./editor/QualityTab";
import { RuntimeTab } from "./editor/RuntimeTab";
import {
  createBody,
  draftFromApp,
  isDirty,
  patchBody,
  tabForError,
  validateDraft,
  type AppDraft,
  type DraftErrors,
} from "./editor/appDraft";
import { activeEditorTab, editorSubtitle, editorTabs, imagePresence } from "./editor/editorTabs";

const TAB_LABEL: Record<string, string> = {
  identity: "Identity",
  quality: "Quality",
  runtime: "Runtime",
};

/** "the Identity and Runtime tabs" — one save, so it must say where to look. */
function errorSummary(errors: DraftErrors): string | null {
  const tabs = [...new Set(Object.keys(errors).map(tabForError))].map((t) => TAB_LABEL[t]);
  if (tabs.length === 0) return null;
  const list =
    tabs.length === 1 ? tabs[0] : `${tabs.slice(0, -1).join(", ")} and ${tabs[tabs.length - 1]}`;
  return `Fix the highlighted fields on the ${list} tab${tabs.length === 1 ? "" : "s"}.`;
}

export function AppEditorPage() {
  const { token } = useAuth();
  const { id, tab } = useParams();
  const navigate = useNavigate();
  const isNew = id === "new";
  const appId = isNew ? null : (id ?? null);

  const appRes = useResource<AdminApp | null>(
    {
      label: "app",
      fetch: async (ctx) => (appId ? (await adminApi.getApp(ctx.token, appId)).app : null),
    },
    [appId],
  );
  const app = appRes.data ?? null;
  const isProvider = app?.library_provider === "steam";
  const parentId = app?.parent_app_id ?? null;

  const profiles = useResource<LaunchProfile[]>(
    {
      label: "launch profiles",
      initialData: [],
      // UI-P4: the picker lists launch profiles (a chain of rungs); every app
      // resolves through one.
      fetch: async (ctx) =>
        (await adminApi.listLaunchProfiles(ctx.token)).items.filter((p) => p.visibility === "user"),
    },
    [],
  );
  const presets = useResource<RuntimePreset[]>(
    {
      label: "runtime presets",
      initialData: [],
      fetch: async (ctx) => (await adminApi.listRuntimePresets(ctx.token)).items,
    },
    [],
  );
  const settings = useResource<InstanceSettings>(
    { label: "settings", fetch: async (ctx) => (await adminApi.getSettings(ctx.token)).settings },
    [],
  );
  const entitlements = useResource<Entitlement[]>(
    {
      label: "access",
      initialData: [],
      fetch: async (ctx) => (appId ? (await adminApi.listAppEntitlements(ctx.token, appId)).items : []),
    },
    [appId],
  );
  const unpublished = useResource<LibraryUnpublishedItem[]>(
    {
      label: "unpublished appids",
      initialData: [],
      fetch: async (ctx) =>
        appId && isProvider
          ? (await adminApi.listUnpublishedLibraryItems(ctx.token, appId)).items
          : [],
    },
    [appId, isProvider],
  );
  const images = useResource<CatalogImage[]>(
    {
      label: "images",
      initialData: [],
      fetch: async (ctx) => (appId ? (await adminApi.listImages(ctx.token)).images : []),
    },
    [appId],
  );
  const parent = useResource<AdminApp | null>(
    {
      label: "parent tile",
      fetch: async (ctx) => (parentId ? (await adminApi.getApp(ctx.token, parentId)).app : null),
    },
    [parentId],
  );

  const [draft, setDraft] = useState<AppDraft>(() => draftFromApp(null));
  const [baseline, setBaseline] = useState<AdminApp | null>(null);
  const [errors, setErrors] = useState<DraftErrors>({});
  const [deleteOpen, setDeleteOpen] = useState(false);

  // Re-seeded on a change of app, never on a refresh of the same one — a
  // reload after a write would discard what is being typed.
  const loadedId = app?.id ?? null;
  useEffect(() => {
    setBaseline(app ?? null);
    setDraft(draftFromApp(app ?? null));
    setErrors({});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [loadedId]);

  const grants = (entitlements.data ?? []).filter((e) => e.subject_type === "user");
  const tabs = editorTabs({
    appId: baseline?.id ?? null,
    isProvider,
    grants: grants.length,
    suppressed: (unpublished.data ?? []).length,
  });
  const active = activeEditorTab(tabs, tab);
  const dirty = isDirty(baseline, draft);

  const save = useAdminAction(
    async () => {
      if (baseline) {
        const body = patchBody(baseline, draft);
        const { app: updated } = await adminApi.updateApp(token!, baseline.id, body);
        appRes.setData(() => updated);
        setBaseline(updated);
        setDraft(draftFromApp(updated));
        return updated;
      }
      const { app: created } = await adminApi.createApp(token!, createBody(draft));
      navigate(`/admin/library/apps/${created.id}`);
      return created;
    },
    {
      success: (result: AdminApp) => (baseline ? "Changes saved." : `"${result.name}" created.`),
      failure: (e) => (e instanceof ApiError ? e.message : "Could not save this app."),
    },
  );

  const toggleEnabled = useAdminAction(
    async (next: boolean) => {
      const { app: updated } = await adminApi.updateApp(token!, baseline!.id, { enabled: next });
      appRes.setData(() => updated);
      setBaseline((prev) => (prev ? { ...prev, enabled: updated.enabled } : prev));
    },
    {
      success: (_r, next) => (next ? "Enabled for users." : "Hidden from the library."),
      failure: "could not update this app",
    },
  );

  const del = useAdminAction(
    async () => {
      await adminApi.deleteApp(token!, baseline!.id);
      navigate("/admin/library/apps");
    },
    {
      success: `"${baseline?.name ?? "App"}" deleted`,
      failure: "could not delete app",
      onFailure: () => setDeleteOpen(false),
    },
  );

  const onSave = () => {
    const errs = validateDraft(draft, { hasParent: !!baseline?.parent_app_id });
    setErrors(errs);
    if (Object.keys(errs).length > 0) return;
    void save.run();
  };

  // Tracks the draft, so renaming an app retitles the page before it is saved.
  const title = draft.name.trim() || (isNew ? "New app" : (app?.name ?? "App"));
  const summary = errorSummary(errors);

  return (
    <section className="page">
      <Breadcrumbs items={[{ label: "Library", to: "/admin/library/apps" }, { label: title }]} />
      <PageHeader
        title={title}
        sub={app ? editorSubtitle(app) : "Not in the library yet. It appears once you save it."}
        actions={
          <>
            <Button
              variant="ghost"
              disabled={!dirty || save.pending != null}
              onClick={() => {
                setDraft(draftFromApp(baseline));
                setErrors({});
              }}
            >
              Discard
            </Button>
            <Button variant="primary" disabled={!dirty || save.pending != null} onClick={onSave}>
              {save.pending ? "Saving…" : "Save changes"}
            </Button>
          </>
        }
      />

      <ResourceStates loading={appRes.loading} error={appRes.errorMessage} />
      {summary && (
        <p className="form-error" role="alert">
          {summary}
        </p>
      )}

      {!appRes.loading && !appRes.errorMessage && (
        <div className="editor">
          <div className="card ae-panel">
            <nav className="tabs" role="tablist" aria-label="App editor sections">
              {tabs.map((t) => (
                <Link
                  key={t.id}
                  to={t.to}
                  role="tab"
                  aria-selected={t.id === active}
                  className={t.id === active ? "tab active" : "tab"}
                >
                  {t.label}
                  {t.count !== undefined && <span className="cnt">{t.count}</span>}
                </Link>
              ))}
            </nav>

            {active === "identity" && (
              <IdentityTab
                draft={draft}
                onChange={setDraft}
                errors={errors}
                app={app}
                parent={parent.data ?? null}
              />
            )}
            {active === "artwork" && app && (
              <ArtworkTab appId={app.id} appName={app.name} token={token!} kind={draft.kind} />
            )}
            {active === "access" && app && (
              <AccessTab
                appId={app.id}
                token={token!}
                items={entitlements.data ?? []}
                loading={entitlements.loading}
                error={entitlements.errorMessage}
                reload={() => void entitlements.refresh()}
                libraryLink={
                  parentId
                    ? `/admin/library/apps/${parentId}/library`
                    : isProvider
                      ? `/admin/library/apps/${app.id}/library`
                      : null
                }
              />
            )}
            {active === "quality" && (
              <QualityTab
                draft={draft}
                onChange={setDraft}
                errors={errors}
                profiles={profiles.data ?? []}
              />
            )}
            {active === "runtime" && (
              <RuntimeTab
                draft={draft}
                onChange={setDraft}
                errors={errors}
                app={app}
                parent={parent.data ?? null}
                presets={presets.data ?? []}
                storageProvider={settings.data?.storage_provider ?? null}
              />
            )}
            {active === "library" && app && (
              <LibraryTab
                appId={app.id}
                token={token!}
                items={unpublished.data ?? []}
                loading={unpublished.loading}
                error={unpublished.errorMessage}
                reload={() => void unpublished.refresh()}
              />
            )}
          </div>

          {app && (
            <EditorRail
              app={app}
              presetName={
                (presets.data ?? []).find((p) => p.id === app.runtime_preset_id)?.name ?? null
              }
              launchProfileName={
                (profiles.data ?? []).find((p) => p.id === app.default_profile_id)?.display_name ??
                "Account or global default"
              }
              imageHosts={imagePresence(images.data ?? [], {
                image: draft.image,
                runtimePresetId: draft.runtimePresetId,
              })}
              onToggleEnabled={(next) => void toggleEnabled.run(next)}
              togglePending={toggleEnabled.pending != null}
              onDelete={() => setDeleteOpen(true)}
            />
          )}
        </div>
      )}

      {deleteOpen && baseline && (
        <Modal
          open
          onClose={() => setDeleteOpen(false)}
          title="Delete app"
          footer={
            <>
              <Button variant="ghost" onClick={() => setDeleteOpen(false)}>
                Cancel
              </Button>
              <Button variant="danger" disabled={del.pending != null} onClick={() => void del.run()}>
                {del.pending ? "Deleting…" : "Delete app"}
              </Button>
            </>
          }
        >
          <p className="sec">
            This permanently removes <strong>{baseline.name}</strong> from the catalog. Its session
            history is purged. Users with an active session on this app must stop it first. This
            cannot be undone.
          </p>
          {/* Spec §17: warn before the delete-a-junk-tile reflex. */}
          {baseline.parent_app_id && (
            <div className="note warn mt4">
              <div>
                <b>This is a discovered tile, so Delete is probably not what you want.</b> Deleting
                it destroys every user&rsquo;s favourite of it and its artwork, and cannot be
                undone. The game is still installed, so the next scan recreates a bare,
                un-favourited, art-less tile in its place. For a junk tile you never want back, use{" "}
                <strong>Ignore</strong> instead: it stays gone across every future scan.
              </div>
            </div>
          )}
        </Modal>
      )}
    </section>
  );
}
