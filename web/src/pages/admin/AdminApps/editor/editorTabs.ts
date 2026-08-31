// The editor's tab list and the head/rail derivations that go with it
// (handoff §A.10). Pure: the page routes on `activeEditorTab` and renders one
// body, so what a tab shows is never what decides whether it exists.

import type { AdminApp, AppKind, CatalogImage } from "../../../../api/types";

export type EditorTabId = "identity" | "artwork" | "access" | "quality" | "runtime" | "library";

export interface EditorTab {
  id: EditorTabId;
  label: string;
  to: string;
  /** Rendered as `.cnt` beside the label. Absent = no badge. */
  count?: number;
}

export interface EditorTabInput {
  /** null for /new: Artwork, Access and Library are all keyed on an app id. */
  appId: string | null;
  /** Only a provider app owns a Library tab. A derived tile can never be one. */
  isProvider: boolean;
  grants: number;
  suppressed: number;
}

export function editorTabs({ appId, isProvider, grants, suppressed }: EditorTabInput): EditorTab[] {
  const base = `/admin/library/apps/${appId ?? "new"}`;
  const tabs: EditorTab[] = [{ id: "identity", label: "Identity", to: base }];
  if (appId) tabs.push({ id: "artwork", label: "Artwork", to: `${base}/artwork` });
  if (appId) {
    tabs.push({
      id: "access",
      label: "Access",
      to: `${base}/access`,
      ...(grants > 0 ? { count: grants } : {}),
    });
  }
  tabs.push({ id: "quality", label: "Quality", to: `${base}/quality` });
  tabs.push({ id: "runtime", label: "Runtime", to: `${base}/runtime` });
  if (appId && isProvider) {
    tabs.push({ id: "library", label: "Library", to: `${base}/library`, count: suppressed });
  }
  return tabs;
}

/** Identity is the default, and is also where an unknown or now-absent tab
 *  lands (a provider app un-marked while /library is open). */
export function activeEditorTab(tabs: EditorTab[], param: string | undefined): EditorTabId {
  return tabs.find((t) => t.id === param)?.id ?? "identity";
}

const KIND_LABEL: Record<AppKind, string> = {
  game: "Game",
  desktop: "Desktop",
  launcher: "Launcher",
};

export function appKindLabel(kind: AppKind): string {
  return KIND_LABEL[kind] ?? "Game";
}

/** Where the tile came from, as the Apps table and the head both say it. */
export function appSourceLabel(app: AdminApp): string {
  return app.external_source === "steam" ? "Steam" : "Manual";
}

export function editorSubtitle(app: AdminApp): string {
  const sessions = app.sessions_30d;
  return `${appKindLabel(app.kind)} · ${appSourceLabel(app)} · ${sessions} session${
    sessions === 1 ? "" : "s"
  } in the last 30 days`;
}

/**
 * How many hosts hold this app's image, from the image catalog. Matched by
 * registry ref first, then by the runtime preset the catalog image installs
 * for. Null when nothing in the catalog is this app's image, which is the
 * normal case for a hand-written image string: the rail omits the fact rather
 * than reporting a count for somebody else's image.
 */
export function imagePresence(
  images: CatalogImage[],
  app: { image: string; runtimePresetId: string },
): { ready: number; total: number } | null {
  const image = app.image.trim();
  const match =
    (image ? images.find((i) => i.registry_ref === image) : undefined) ??
    (app.runtimePresetId
      ? images.find((i) => i.runtime_preset_id === app.runtimePresetId)
      : undefined);
  const hosts = match?.hosts;
  if (!hosts || hosts.length === 0) return null;
  return { ready: hosts.filter((h) => h.state === "ready").length, total: hosts.length };
}
