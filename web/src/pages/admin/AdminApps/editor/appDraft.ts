// The app editor's form state, as data: one draft for all six tabs, so a save
// carries every tab's edits (handoff §A.10).
//
// `launchable_profile_ids` is the field to read twice: `[]` is the contract's
// "unrestricted", so an unchanged list must be omitted and a list that emptied
// must be sent (control-api.md; omission is a silent no-op, null is a 400).

import type {
  AdminApp,
  AppKind,
  CreateAppRequest,
  LaunchProfile,
  LibraryProvider,
  ProfilePolicyMode,
  UpdateAppRequest,
} from "../../../../api/types";
import { effectiveLaunchableIds } from "../launchableHelpers";
import { emptySpec, parseSpec, specToRecord } from "../runtimeSpec";

export interface AppDraft {
  name: string;
  description: string;
  kind: AppKind;
  libraryProvider: LibraryProvider;
  /** No control edits these: a launch-profile pick writes the rung's geometry
   *  through, and a create must still send them. */
  width: number;
  height: number;
  fps: number;
  bitrateKbps: number;
  defaultProfileId: string;
  profilePolicy: ProfilePolicyMode;
  /** Control state, not a stored field: the contract models restriction as
   *  list emptiness. */
  restrictLaunchable: boolean;
  launchableIds: string[];
  image: string;
  args: string[];
  env: [string, string][];
  mounts: string[];
  runtimePresetId: string;
  managedHome: boolean;
  containerPath: string;
  /** runtime_spec.gpu is inert but must round-trip; no control offers it. */
  gpu: boolean;
  /** runtime_spec keys the form does not edit. Dropping `no_new_privileges`
   *  once broke every GOW desktop launch. */
  specExtras: Record<string, unknown>;
}

export function draftFromApp(app: AdminApp | null): AppDraft {
  const spec = app ? parseSpec(app.runtime_spec) : emptySpec();
  return {
    name: app?.name ?? "",
    description: app?.description ?? "",
    kind: app?.kind ?? "game",
    libraryProvider: app?.library_provider ?? "",
    width: app?.default_width ?? 1280,
    height: app?.default_height ?? 720,
    fps: app?.default_fps ?? 60,
    bitrateKbps: app?.default_bitrate_kbps ?? 6000,
    defaultProfileId: app?.default_profile_id ?? "",
    profilePolicy: app?.profile_policy ?? "inherit",
    restrictLaunchable: (app?.launchable_profile_ids ?? []).length > 0,
    launchableIds: app?.launchable_profile_ids ?? [],
    image: spec.image,
    args: spec.args,
    env: Object.entries(spec.env),
    mounts: spec.mounts,
    runtimePresetId: app?.runtime_preset_id ?? "",
    managedHome: app?.managed_home ?? false,
    containerPath: app?.home_container_path || "/home/quasar",
    gpu: spec.gpu,
    specExtras: spec.extras,
  };
}

/** Steam suggests Launcher, never overrides an explicit choice: library_provider
 *  does not imply kind, and no server path branches on kind. */
export function withLibraryProvider(draft: AppDraft, provider: LibraryProvider): AppDraft {
  const kind = provider === "steam" && draft.kind === "game" ? "launcher" : draft.kind;
  return { ...draft, libraryProvider: provider, kind };
}

/** Picking a launch profile writes its top rung's geometry through to the
 *  app's own defaults, which is what a launch falls back to. */
export function withLaunchProfile(draft: AppDraft, profile: LaunchProfile | undefined): AppDraft {
  const next = { ...draft, defaultProfileId: profile?.id ?? "" };
  const rung =
    profile?.rungs.find((r) => r.position === 1)?.stream_profile ?? profile?.rungs[0]?.stream_profile;
  if (!rung) return next;
  return {
    ...next,
    width: rung.width,
    height: rung.height,
    fps: rung.fps,
    bitrateKbps: rung.nominal_bitrate_kbps,
  };
}

/** `inherit` pins nothing, so it clears the profile; the other two need one. */
export function withProfilePolicy(
  draft: AppDraft,
  policy: ProfilePolicyMode,
  profiles: LaunchProfile[],
): AppDraft {
  if (policy === "inherit") return { ...draft, profilePolicy: "inherit", defaultProfileId: "" };
  const next = { ...draft, profilePolicy: policy };
  if (next.defaultProfileId || profiles.length === 0) return next;
  return withLaunchProfile(next, profiles[0]);
}

export type DraftErrors = Record<string, string>;

/**
 * `hasParent` is a derived tile: its runtime is merged from the parent at
 * launch and the Runtime tab offers no image control, so requiring one would
 * be an error with nowhere to fix it. A preset supplies the image server-side
 * (mergeRuntimePreset), which is what the Image field's own hint promises.
 */
export function validateDraft(draft: AppDraft, opts: { hasParent?: boolean } = {}): DraftErrors {
  const errs: DraftErrors = {};
  if (!draft.name.trim()) errs.name = "Name is required";
  if (!draft.image.trim() && !draft.runtimePresetId && !opts.hasParent) {
    errs.image = "Image is required";
  }

  draft.env.forEach(([k], i) => {
    if (!k.trim()) {
      errs[`env_key_${i}`] = "Key cannot be empty";
    } else if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(k.trim())) {
      errs[`env_key_${i}`] =
        "Invalid key (use letters, digits, underscores; start with letter or _)";
    }
  });
  draft.args.forEach((a, i) => {
    if (!a.trim()) errs[`arg_${i}`] = "Argument cannot be empty";
  });
  draft.mounts.forEach((m, i) => {
    if (!m.trim()) errs[`mount_${i}`] = "Mount cannot be empty";
  });

  if ((draft.profilePolicy === "prefer" || draft.profilePolicy === "force") && !draft.defaultProfileId) {
    errs.defaultProfileId = "Choose a launch profile";
  }
  // "Only these" with nothing ticked serialises to [], which the contract reads
  // as unrestricted: the inverse of the operator's intent.
  if (
    draft.profilePolicy !== "force" &&
    draft.restrictLaunchable &&
    launchableToSend(draft).length === 0
  ) {
    errs.launchable = "Pick at least one launch profile, or switch back to Any eligible profile";
  }
  return errs;
}

/** Which tab an error key belongs to, so one save can say where to look. */
export function tabForError(key: string): "identity" | "quality" | "runtime" {
  if (key === "name") return "identity";
  if (key === "defaultProfileId" || key === "launchable") return "quality";
  return "runtime";
}

function launchableToSend(draft: AppDraft): string[] {
  return effectiveLaunchableIds(
    draft.profilePolicy,
    draft.defaultProfileId,
    draft.restrictLaunchable,
    draft.launchableIds,
  );
}

function specOf(draft: AppDraft): Record<string, unknown> {
  return specToRecord({
    image: draft.image.trim(),
    args: draft.args.map((a) => a.trim()).filter(Boolean),
    env: Object.fromEntries(draft.env.map(([k, v]) => [k.trim(), v])),
    mounts: draft.mounts.map((m) => m.trim()).filter(Boolean),
    gpu: draft.gpu,
    extras: draft.specExtras,
  });
}

/** Key-order-independent equality for the two record-shaped fields. */
function sameJson(a: unknown, b: unknown): boolean {
  const norm = (v: unknown): unknown => {
    if (Array.isArray(v)) return v.map(norm);
    if (v && typeof v === "object") {
      return Object.fromEntries(
        Object.entries(v as Record<string, unknown>)
          .sort(([x], [y]) => (x < y ? -1 : 1))
          .map(([k, val]) => [k, norm(val)]),
      );
    }
    return v;
  };
  return JSON.stringify(norm(a)) === JSON.stringify(norm(b));
}

/**
 * The PATCH for `app` -> `draft`: changed keys only, `{}` when nothing moved.
 *
 * Compared against the draft the app loads as, never the row: the draft
 * normalises (empty `runtime_spec` to five empty fields, blank
 * `home_container_path` to /home/quasar, the pinned default into the
 * allow-list), so a row diff reports those as edits and the editor opens dirty.
 */
export function patchBody(app: AdminApp, draft: AppDraft): UpdateAppRequest {
  const from = draftFromApp(app);
  const body: UpdateAppRequest = {};
  if (draft.name.trim() !== from.name.trim()) body.name = draft.name.trim();
  if (draft.description !== from.description) body.description = draft.description;
  if (draft.kind !== from.kind) body.kind = draft.kind;
  if (draft.libraryProvider !== from.libraryProvider) body.library_provider = draft.libraryProvider;
  if (draft.width !== from.width) body.default_width = draft.width;
  if (draft.height !== from.height) body.default_height = draft.height;
  if (draft.fps !== from.fps) body.default_fps = draft.fps;
  if (draft.bitrateKbps !== from.bitrateKbps) body.default_bitrate_kbps = draft.bitrateKbps;
  if (draft.defaultProfileId !== from.defaultProfileId) {
    body.default_profile_id = draft.defaultProfileId || null;
  }
  if (draft.profilePolicy !== from.profilePolicy) body.profile_policy = draft.profilePolicy;

  const spec = specOf(draft);
  if (!sameJson(spec, specOf(from))) body.runtime_spec = spec;

  if (draft.managedHome !== from.managedHome) body.managed_home = draft.managedHome;
  if (draft.containerPath !== from.containerPath) body.home_container_path = draft.containerPath;
  if (draft.runtimePresetId !== from.runtimePresetId) {
    body.runtime_preset_id = draft.runtimePresetId || null;
  }

  const launchable = launchableToSend(draft);
  if (!sameJson(launchable, launchableToSend(from))) body.launchable_profile_ids = launchable;
  return body;
}

export function isDirty(app: AdminApp | null, draft: AppDraft): boolean {
  if (!app) return !sameJson(draft, draftFromApp(null));
  return Object.keys(patchBody(app, draft)).length > 0;
}

export function createBody(draft: AppDraft): CreateAppRequest {
  return {
    name: draft.name.trim(),
    description: draft.description,
    kind: draft.kind,
    library_provider: draft.libraryProvider,
    default_width: draft.width,
    default_height: draft.height,
    default_fps: draft.fps,
    default_bitrate_kbps: draft.bitrateKbps,
    default_profile_id: draft.defaultProfileId || null,
    profile_policy: draft.profilePolicy,
    runtime_spec: specOf(draft),
    managed_home: draft.managedHome,
    home_container_path: draft.containerPath,
    runtime_preset_id: draft.runtimePresetId || null,
    launchable_profile_ids: launchableToSend(draft),
  };
}
