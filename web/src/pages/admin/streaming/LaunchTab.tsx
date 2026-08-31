// Launch profiles tab (v3 handoff §A.16, mockup pageLaunch()): a defaults
// card (global default profile + "let users choose" policy) above a
// `.grid.g2` of one card per launch profile, each holding the inline rung
// editor (existing PATCH-per-action logic — chain mutations save immediately,
// no Save button; Rename is an identity-only modal; only "New launch profile"
// opens a drawer). Authorization is server-enforced; UX only (invariant #6).

import { useState } from "react";
import * as adminApi from "../../../api/admin";
import type { LaunchProfile, ProfilePolicyResponse, StreamProfile } from "../../../api/types";
import { ApiError } from "../../../api/client";
import { useAuth } from "../../../auth/context";
import { ActionsMenu, type ActionsMenuItem, type ActionsMenuSeparator } from "../../../components/ActionsMenu";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { IconPlus } from "../../../components/icons";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { TextareaField, TextField } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { isLaunchProfileInUse } from "./launchProfileHelpers";
import { LaunchProfileDrawer } from "./LaunchProfileDrawer";
import { moveRung, RungEditor } from "./RungEditor";
import { useSectionHead } from "../../../components/shell/sectionHead";

const DEFAULT_POLICY: ProfilePolicyResponse = { global_default_profile_id: null, user_overrides_allowed: true };

// ── Defaults card (mock's "Default profile" + "Let users choose a profile") ─

function DefaultsCard({
  launchProfiles,
  policy,
  busy,
  onChangeDefault,
  onChangeOverridesAllowed,
}: {
  launchProfiles: LaunchProfile[];
  policy: ProfilePolicyResponse;
  busy: boolean;
  onChangeDefault: (id: string) => void;
  onChangeOverridesAllowed: (checked: boolean) => void;
}) {
  // The global default is a user-facing setting (control-api.md: the field a
  // session with no preference falls back to), so the candidates are the same
  // visibility:user set — an internal-only profile is never a valid
  // instance-wide default.
  const userVisible = launchProfiles.filter((p) => p.visibility === "user");
  return (
    <div className="card card-pad" style={{ display: "flex", gap: "var(--s7)", alignItems: "center", flexWrap: "wrap" }}>
      <div>
        <div className="eyebrow">Default profile</div>
        <select
          className="select"
          style={{ marginTop: 7 }}
          value={policy.global_default_profile_id ?? ""}
          disabled={busy}
          aria-label="Default profile"
          onChange={(e) => onChangeDefault(e.target.value)}
        >
          <option value="">Use recommendation</option>
          {userVisible.map((p) => (
            <option key={p.id} value={p.id}>{p.display_name}</option>
          ))}
        </select>
      </div>
      <div style={{ maxWidth: 420 }}>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: "var(--s5)" }}>
          <div>
            <div className="label">Let users choose a profile</div>
            <div className="hint">Otherwise every session uses the default</div>
          </div>
          <label className="switch">
            <input
              type="checkbox"
              checked={policy.user_overrides_allowed}
              disabled={busy}
              aria-label="Let users choose a profile"
              onChange={(e) => onChangeOverridesAllowed(e.target.checked)}
            />
            <span className="track" />
            <span className="thumb" />
          </label>
        </div>
      </div>
    </div>
  );
}

// ── Rename modal (identity-only edit) ───────────────────────────────────────

function RenameModal({
  profile,
  token,
  onClose,
  onSaved,
}: {
  profile: LaunchProfile;
  token: string;
  onClose: () => void;
  onSaved: (p: LaunchProfile) => void;
}) {
  const [name, setName] = useState(profile.display_name);
  const [description, setDescription] = useState(profile.description);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async () => {
    if (!name.trim()) {
      setError("Name is required");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const saved = await adminApi.updateLaunchProfile(token, profile.id, {
        display_name: name.trim(),
        description,
        rungs: profile.rungs.map((r) => r.stream_profile.id),
      });
      onSaved(saved);
    } catch (e: unknown) {
      setError(e instanceof ApiError ? e.message : "Could not save.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      onClose={onClose}
      title="Rename launch profile"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={saving} onClick={() => void submit()}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </>
      }
    >
      <div className="col gap4">
        <TextField label="Name" value={name} onChange={(e) => setName(e.target.value)} />
        <TextareaField
          label="Description"
          rows={2}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
        {error && <p className="form-error">{error}</p>}
      </div>
    </Modal>
  );
}

// ── One launch profile card ─────────────────────────────────────────────────

function LaunchProfileCard({
  profile,
  isDefault,
  streamProfiles,
  token,
  onChanged,
  onRename,
  onSetDefault,
  onDeleteClick,
}: {
  profile: LaunchProfile;
  isDefault: boolean;
  streamProfiles: StreamProfile[];
  token: string;
  onChanged: (p: LaunchProfile) => void;
  onRename: () => void;
  onSetDefault: () => void;
  onDeleteClick: () => void;
}) {
  const { addToast } = useToast();
  const [busy, setBusy] = useState(false);

  const rungs = profile.rungs.map((r) => r.stream_profile);
  const availableToAdd = streamProfiles.filter((sp) => !rungs.some((r) => r.id === sp.id));
  const appCount = profile.used_by?.apps.length ?? 0;
  const inUse = isLaunchProfileInUse(profile.used_by);

  const menuItems: (ActionsMenuItem | ActionsMenuSeparator)[] = [
    { key: "rename", label: "Rename profile", onClick: onRename },
    isDefault
      ? { key: "default", label: "Already the default", onClick: () => {}, disabled: true }
      : { key: "default", label: "Set as default", onClick: onSetDefault },
    { key: "sep", separator: true },
    {
      key: "delete",
      label: "Delete profile",
      variant: "danger",
      onClick: onDeleteClick,
      disabled: inUse,
      title: inUse ? "In use. Remove it from every app first." : undefined,
    },
  ];

  const persist = async (nextRungIds: string[]) => {
    setBusy(true);
    try {
      const saved = await adminApi.updateLaunchProfile(token, profile.id, {
        display_name: profile.display_name,
        description: profile.description,
        rungs: nextRungIds,
      });
      onChanged(saved);
      for (const w of saved.warnings ?? []) {
        addToast({ variant: "info", title: w.message });
      }
    } catch (e: unknown) {
      addToast({
        variant: "danger",
        title: e instanceof ApiError ? e.message : "Could not save launch profile.",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <div className="panel-head">
        <span className="panel-title">{profile.display_name}</span>
        <div className="acts">
          {isDefault && <Chip variant="accent">default</Chip>}
          <Chip>{appCount} app{appCount === 1 ? "" : "s"}</Chip>
          <ActionsMenu label={`Actions for ${profile.display_name}`} items={menuItems} />
        </div>
      </div>
      <div style={{ padding: "var(--s3) var(--card-pad) var(--card-pad)" }}>
        {profile.description && <p className="hint" style={{ marginTop: -4, marginBottom: "var(--s3)" }}>{profile.description}</p>}
        <RungEditor
          rungs={rungs}
          availableToAdd={availableToAdd}
          disabled={busy}
          onMove={(from, to) => {
            const next = moveRung(rungs, from, to);
            if (next !== rungs) void persist(next.map((r) => r.id));
          }}
          onRemove={(index) => {
            const next = rungs.filter((_, i) => i !== index);
            void persist(next.map((r) => r.id));
          }}
          onAdd={(profileId) => {
            void persist([...rungs.map((r) => r.id), profileId]);
          }}
        />
      </div>
    </div>
  );
}

// ── Page ─────────────────────────────────────────────────────────────────────

/** Fan-out fetch: launch profiles are the tab's subject, stream profiles are
 *  the candidate catalog for "Add a stream profile", and the policy backs the
 *  defaults card — every rung, not just visibility:user stream profiles,
 *  since a rung is normally internal-visibility and only its wrapping launch
 *  profile is user-facing. Composed inside the one spec's fetch so the page
 *  keeps a single timer and a single error surface. */
interface LaunchTabData {
  launchProfiles: LaunchProfile[];
  streamProfiles: StreamProfile[];
  policy: ProfilePolicyResponse;
}

export function LaunchTab() {
  const { token } = useAuth();
  const { addToast } = useToast();

  const resource = useResource({
    label: "launch profiles",
    initialData: { launchProfiles: [], streamProfiles: [], policy: DEFAULT_POLICY } as LaunchTabData,
    fetch: async (ctx) => {
      const [lps, sps, policy] = await Promise.all([
        adminApi.listLaunchProfiles(ctx.token),
        adminApi.listStreamProfiles(ctx.token),
        adminApi.getProfilePolicy(ctx.token),
      ]);
      return { launchProfiles: lps.items, streamProfiles: sps.items, policy };
    },
  });
  const launchProfiles = resource.data?.launchProfiles ?? [];
  const streamProfiles = resource.data?.streamProfiles ?? [];
  const policy = resource.data?.policy ?? DEFAULT_POLICY;

  const [creating, setCreating] = useState(false);
  const [renameTarget, setRenameTarget] = useState<LaunchProfile | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<LaunchProfile | null>(null);

  const savePolicy = useAdminAction<[Partial<ProfilePolicyResponse>], ProfilePolicyResponse>(
    (patch) =>
      resource.mutate(
        (ctx) => adminApi.updateProfilePolicy(ctx.token, { ...policy, ...patch }),
        (data, saved) => ({ ...data, policy: saved }),
      ),
    { failure: "Could not save the launch policy." },
  );

  const del = useAdminAction<[LaunchProfile], void>(
    (profile) =>
      resource.mutate(
        (ctx) => adminApi.deleteLaunchProfile(ctx.token, profile.id),
        (data) => ({
          ...data,
          launchProfiles: data.launchProfiles.filter((p) => p.id !== profile.id),
        }),
      ),
    {
      success: (_result, profile) => `"${profile.display_name}" deleted`,
      failure: "could not delete launch profile",
      onSuccess: () => setDeleteTarget(null),
    },
  );

  // The head is the Streaming section's (../Streaming.tsx); this tab fills it in.
  useSectionHead({
    sub: "What a user picks, and the quality chain behind it",
    actions: (
      <Button variant="primary" onClick={() => setCreating(true)}>
        <IconPlus />
        New launch profile
      </Button>
    ),
    counts: { launch: launchProfiles.length },
  });

  return (
    <section className="page">
      {token && (
        <DefaultsCard
          launchProfiles={launchProfiles}
          policy={policy}
          busy={savePolicy.pending != null || resource.loading}
          onChangeDefault={(id) => void savePolicy.run({ global_default_profile_id: id || null })}
          onChangeOverridesAllowed={(checked) => void savePolicy.run({ user_overrides_allowed: checked })}
        />
      )}

      <ResourceStates
        loading={resource.loading}
        error={resource.errorMessage}
        isEmpty={launchProfiles.length === 0}
        empty="No launch profiles yet. Create one from your stream profiles."
      />

      {token && launchProfiles.length > 0 && (
        <div className="grid g2">
          {launchProfiles.map((p) => (
            <LaunchProfileCard
              key={p.id}
              profile={p}
              isDefault={policy.global_default_profile_id === p.id}
              streamProfiles={streamProfiles}
              token={token}
              onChanged={(saved) =>
                resource.setData((data) => ({
                  ...data,
                  launchProfiles: data.launchProfiles.map((lp) => (lp.id === saved.id ? saved : lp)),
                }))
              }
              onRename={() => setRenameTarget(p)}
              onSetDefault={() => void savePolicy.run({ global_default_profile_id: p.id })}
              onDeleteClick={() => setDeleteTarget(p)}
            />
          ))}
        </div>
      )}

      {/* Rename */}
      {renameTarget && token && (
        <RenameModal
          profile={renameTarget}
          token={token}
          onClose={() => setRenameTarget(null)}
          onSaved={(saved) => {
            resource.setData((data) => ({
              ...data,
              launchProfiles: data.launchProfiles.map((lp) => (lp.id === saved.id ? saved : lp)),
            }));
            setRenameTarget(null);
            addToast({ variant: "success", title: `"${saved.display_name}" saved` });
          }}
        />
      )}

      {/* Delete confirmation */}
      {deleteTarget && (
        <Modal
          open
          onClose={() => setDeleteTarget(null)}
          title="Delete launch profile"
          footer={
            <>
              <Button variant="ghost" onClick={() => setDeleteTarget(null)}>Cancel</Button>
              <Button variant="danger" disabled={del.pending != null} onClick={() => void del.run(deleteTarget)}>
                {del.pending ? "Deleting…" : "Delete profile"}
              </Button>
            </>
          }
        >
          <p className="sec">
            This permanently removes <strong>{deleteTarget.display_name}</strong>. This cannot be
            undone. Any app, the global policy, or a user preference still pointing at it must be
            repointed first. The server refuses the delete while any do.
          </p>
        </Modal>
      )}

      {/* New launch profile drawer */}
      {creating && token && (
        <LaunchProfileDrawer
          token={token}
          streamProfiles={streamProfiles}
          onClose={() => setCreating(false)}
          onSaved={(saved) => {
            resource.setData((data) => ({
              ...data,
              launchProfiles: [...data.launchProfiles, saved],
            }));
            setCreating(false);
            addToast({ variant: "success", title: `"${saved.display_name}" created` });
          }}
        />
      )}
    </section>
  );
}
