// Placement tab (RH05 #342, control-api.md "App placement, homes and explicit
// image cleanup"): which hosts may start this app. Not part of the app draft —
// placement is its own resource with its own revision, so it saves on its own
// and the page's Save changes never touches it.
//
// No design_handoff_v3 mock covers this tab; it is built only from the
// editor's existing pieces (Section, SegmentedControl, .check, .ae-list, Chip).
//
// Selected, prepared and ready are three separate observations the server
// makes per host. They are never folded into one verdict here: a selected host
// can be unprepared, and a ready host can be unselected.

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import { useAuth } from "../../../../auth/context";
import type {
  AppPlacement,
  AppPlacementHost,
  AppPlacementMode,
  CatalogImage,
  Host,
} from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { Chip, type ChipVariant } from "../../../../components/Chip";
import { ResourceStates } from "../../../../components/ResourceStates";
import { SegmentedControl } from "../../../../components/SegmentedControl";
import { useAdminAction } from "../../../../lib/resource/action";
import { useResource } from "../../../../lib/resource/react";
import {
  AWAITING_PREPARATION,
  hostImageLine,
  placementImage,
  placementReason,
  type PlacementImage,
  type PlacementImageRef,
} from "./placementImage";
import { Section } from "./primitives";

export type { PlacementImageRef } from "./placementImage";

/** Preparation is progress, so both reads refresh while the tab is open. */
const POLL_MS = 5000;

export interface PlacementDraft {
  mode: AppPlacementMode;
  hostIds: string[];
  /** Revision observed when this edit began. It survives tab unmounts. */
  revision: string;
}

interface PlacementTabProps {
  appId: string;
  draft: PlacementDraft | null;
  setDraft: (draft: PlacementDraft | null) => void;
  /** The parent tile when this app is derived; used for its name and link.
   *  The placement read's `inherited_from` is what decides read-only. */
  parent: { id: string; name: string } | null;
  /** Null while unknown; no managed/unmanaged claim is made without it. */
  image?: PlacementImageRef | null;
}

const STALE_COPY =
  "Placement changed elsewhere while you were editing, so nothing was saved. The current selection is shown below; review it and save again.";

function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  const s = new Set(a);
  return b.every((id) => s.has(id));
}

function saved(p: AppPlacement): PlacementDraft {
  return { mode: p.mode, hostIds: p.mode === "fixed" ? p.host_ids : [], revision: p.revision };
}

/** One row per host the server reports on, plus any selected or registered
 *  host it does not, so a fixed selection can always name every host. */
function hostRows(p: AppPlacement, hosts: Host[]): { id: string; name: string; obs: AppPlacementHost | null }[] {
  const ids = [...p.hosts.map((h) => h.host_id), ...p.host_ids, ...hosts.map((h) => h.id)];
  const seen = new Set<string>();
  const names = new Map(hosts.map((h) => [h.id, h.node_name]));
  const obs = new Map(p.hosts.map((h) => [h.host_id, h]));
  return ids
    .filter((id) => (seen.has(id) ? false : (seen.add(id), true)))
    .map((id) => ({ id, name: names.get(id) ?? `Host ${id.slice(0, 8)}`, obs: obs.get(id) ?? null }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

function stateChip(label: string, value: boolean | null | undefined, noVariant: ChipVariant) {
  const [variant, text] =
    value === true
      ? (["success", label] as const)
      : value === false
        ? ([noVariant, `not ${label.toLowerCase()}`] as const)
        : (["neutral", `${label.toLowerCase()} unknown`] as const);
  return (
    <Chip variant={variant} className="chip-sm">
      {text}
    </Chip>
  );
}

function HostStates({ obs, unmanaged }: { obs: AppPlacementHost | null; unmanaged: boolean }) {
  return (
    <div className="row gap2" aria-label="Host state">
      {stateChip("Selected", obs?.selected ?? null, "neutral")}
      {unmanaged ? (
        <Chip variant="neutral" className="chip-sm">
          not managed
        </Chip>
      ) : (
        stateChip("Prepared", obs?.prepared ?? null, "warning")
      )}
      {stateChip("Ready", obs?.ready ?? null, "warning")}
    </div>
  );
}

function HostDetail({ obs, prep, retry, retrying }: { obs: AppPlacementHost | null; prep: PlacementImage; retry: (hostId: string, imageId: string) => void; retrying: boolean }) {
  // Only a host that may start the app needs the image; elsewhere it is noise.
  const line = prep.kind === "managed" && obs?.selected ? hostImageLine(prep.image, obs.host_id) : null;
  // The image line says more than the generic "not prepared" reason; any other
  // reason is its own fact and stays.
  const reason =
    obs?.reason && !(line && obs.reason === AWAITING_PREPARATION) ? placementReason(obs.reason) : null;
  return (
    <>
      {line && prep.kind === "managed" && (
        <div className="ae-item-m">
          {line.text}
          {line.error ? `: ${line.error}` : ""}
          {line.actionable && (
            <>
              {" "}
              <Link to={`/admin/library/images/${prep.image.id}`}>
                Open {prep.image.display_name}
              </Link>
            </>
          )}
          {obs?.selected && obs.reason === "preparation_failed" && (
            <Button variant="ghost" disabled={retrying} onClick={() => retry(obs.host_id, prep.image.id)}>
              {retrying ? "Scheduling…" : "Retry preparation"}
            </Button>
          )}
        </div>
      )}
      {reason && <div className="ae-item-m">{reason}</div>}
    </>
  );
}

function ImageNote({ prep }: { prep: PlacementImage }) {
  if (prep.kind === "managed") {
    const { image } = prep;
    return (
      <span className="hint">
        Prepared follows the catalog image{" "}
        <Link to={`/admin/library/images/${image.id}`}>{image.display_name}</Link>
        {image.installed_version ? ` (version ${image.installed_version})` : ""} on each selected
        host.
        {image.lazy ? " It is installed to download on first launch, not ahead of time." : ""}
      </span>
    );
  }
  if (prep.kind === "unmanaged") {
    return (
      <div className="note">
        <div>
          This app&rsquo;s image{prep.ref && " "}
          {prep.ref && <span className="mono">{prep.ref}</span>} is not installed from the image
          catalog, so Quasar does not prepare it on hosts and Prepared reads not managed rather
          than a failure. A selected host fetches it itself when a session starts there, and that
          launch fails if the host cannot. To have Quasar prepare it ahead of time, install it
          from <Link to="/admin/library/images">Images</Link> and use that image for this app.
        </div>
      </div>
    );
  }
  return null;
}

const STATES_NOTE = (
  <span className="hint">
    Selected is what this placement allows. Prepared and Ready are what the host last reported for
    this app, each on its own. Unknown means there is no evidence yet, not that the host failed.
  </span>
);

const LOCALITY_NOTE = (
  <div className="note">
    <div>
      Removing a host stops <strong>new</strong> sessions of this app from starting there. Sessions
      already running finish normally, and nothing is deleted: users&rsquo; homes and the app image
      stay on that host. A user who already has a home for this app can only launch it on the host
      that holds that home, so leaving that host out blocks their launches instead of starting them
      with an empty home somewhere else.
    </div>
  </div>
);

export function PlacementTab({ appId, parent, image = null, draft, setDraft }: PlacementTabProps) {
  const { token } = useAuth();
  const placement = useResource<AppPlacement>(
    {
      label: "placement",
      fetch: (ctx) => adminApi.getAppPlacement(ctx.token, appId),
      pollMs: POLL_MS,
    },
    [appId],
  );
  const hosts = useResource<Host[]>(
    { label: "hosts", initialData: [], fetch: (ctx) => adminApi.listAllHosts(ctx.token) },
    [],
  );
  const hasImage = image != null;
  const images = useResource<CatalogImage[]>(
    {
      label: "images",
      fetch: async (ctx) => (hasImage ? (await adminApi.listImages(ctx.token)).images : []),
      pollMs: hasImage ? POLL_MS : undefined,
    },
    [hasImage],
  );
  // null = untouched, so a refreshed read shows through until the first edit.
  const [conflict, setConflict] = useState<string | null>(null);

  const data = placement.data;
  const current = data ? (draft ?? saved(data)) : null;
  // A 409 requires a fresh read and a second explicit Save. Only that
  // conflict path rebases the surviving edit; ordinary tab remounts do not.
  useEffect(() => {
    if (conflict && data && draft && draft.revision !== data.revision) {
      setDraft({ ...draft, revision: data.revision });
    }
  }, [conflict, data, draft, setDraft]);
  const dirty =
    !!data &&
    !!draft &&
    (draft.mode !== data.mode || (draft.mode === "fixed" && !sameSet(draft.hostIds, data.host_ids)));

  const save = useAdminAction(
    async (next: PlacementDraft, revision: string) =>
      placement.mutate(
        (ctx) =>
          adminApi.updateAppPlacement(ctx.token, appId, {
            expected_revision: revision,
            mode: next.mode,
            host_ids: next.mode === "fixed" ? [...next.hostIds].sort() : [],
          }),
        (_old, updated) => updated,
      ),
    {
      success: "Placement saved.",
      failure: (e) =>
        e instanceof ApiError && e.code === "stale_revision"
          ? "Placement changed elsewhere; nothing was saved."
          : e instanceof ApiError
            ? e.message
            : "Could not save placement.",
      onSuccess: () => {
        setDraft(null);
        setConflict(null);
      },
      onFailure: (e) => {
        if (e instanceof ApiError && e.code === "stale_revision") {
          // Keep the edit so it can be re-applied deliberately against the
          // fresh revision; never re-send it on the admin's behalf.
          setConflict(STALE_COPY);
          void placement.refresh({ silent: true });
        } else if (e instanceof ApiError && e.code === "inherited_placement") {
          setDraft(null);
          setConflict(
            "This tile inherits its placement from its parent, so it cannot be set here. Edit the parent app instead.",
          );
          void placement.refresh({ silent: true });
        }
      },
    },
  );

  const retryAction = useAdminAction<[string, string], void>(
    async (hostId, imageId) => {
      if (!token) throw new Error("Sign in to retry image preparation.");
      await adminApi.retryHostImage(token, hostId, imageId);
      // The 202 already accepted the retry. A following read failure must
      // not report that the retry itself failed; polling will converge.
      void images.refresh({ silent: true });
      void placement.refresh({ silent: true });
    },
    {
      success: "Image retry scheduled",
      failure: (e) => e instanceof ApiError ? e.message : "Could not retry image preparation.",
    },
  );

  if (!data || !current) {
    return (
      <Section title="Placement" desc="Which hosts may start new sessions of this app.">
        <ResourceStates loading={placement.loading} error={placement.errorMessage} />
      </Section>
    );
  }

  const rows = hostRows(data, hosts.data ?? []);
  // A failed catalog read leaves `data` undefined, or stale from an earlier
  // tick; only a current read may call the image unmanaged.
  const prep = placementImage(image, images.errorMessage ? undefined : images.data, data);
  const unmanaged = prep.kind === "unmanaged";
  const imageStates = (
    <>
      <ImageNote prep={prep} />
      {hasImage && <ResourceStates loading={false} error={images.errorMessage} />}
    </>
  );
  const retry = (hostId: string, imageId: string) => {
    if (retryAction.pending) return;
    void retryAction.run(hostId, imageId);
  };

  if (data.inherited_from) {
    const parentId = data.inherited_from;
    const parentName = parent?.id === parentId ? parent.name : "its parent app";
    return (
      <Section title="Placement" desc="Inherited from the parent tile.">
        {conflict && (
          <p className="form-error" role="alert">
            {conflict}
          </p>
        )}
        <div className="note">
          <div>
            This tile is discovered under <strong>{parentName}</strong> and has no placement of its
            own. It starts on whichever hosts the parent allows. Edit{" "}
            <Link to={`/admin/library/apps/${parentId}/placement`}>{parentName}</Link> to change
            them.
          </div>
        </div>
        <p className="hint">
          {data.mode === "all_eligible"
            ? "The parent allows every eligible host, including hosts added later."
            : data.host_ids.length === 0
              ? "The parent allows no host, so this tile cannot be launched."
              : `The parent allows only the ${data.host_ids.length === 1 ? "host" : `${data.host_ids.length} hosts`} marked Selected below.`}
        </p>
        {imageStates}
        <div className="ae-list">
          {rows.map((r) => (
            <div key={r.id} className="ae-item">
              <div>
                <div className="ae-item-t">{r.name}</div>
                <HostDetail obs={r.obs} prep={prep} retry={retry} retrying={retryAction.pending?.[0] === r.id} />
              </div>
              <HostStates obs={r.obs} unmanaged={unmanaged} />
            </div>
          ))}
        </div>
        {STATES_NOTE}
      </Section>
    );
  }

  const toggle = (hostId: string) => {
    setConflict(null);
    const ids = current.hostIds.includes(hostId)
      ? current.hostIds.filter((id) => id !== hostId)
      : [...current.hostIds, hostId];
    setDraft({ mode: "fixed", hostIds: ids, revision: current.revision });
  };
  const setMode = (mode: AppPlacementMode) => {
    setConflict(null);
    // Returning to fixed starts from the saved list, not an empty one.
    setDraft({ mode, hostIds: mode === "fixed" ? (draft?.hostIds ?? data.host_ids) : [], revision: current.revision });
  };
  const pending = save.pending != null;

  return (
    <Section
      title="Placement"
      desc="Which hosts may start new sessions of this app. Saved on its own, separately from Save changes above."
    >
      {conflict && (
        <p className="form-error" role="alert">
          {conflict}
        </p>
      )}
      <SegmentedControl
        aria-label="Which hosts may run this app"
        value={current.mode}
        onChange={setMode}
        disabled={pending}
        options={[
          { value: "all_eligible", label: "Every eligible host" },
          { value: "fixed", label: "Only these hosts" },
        ]}
      />
      <span className="hint">
        {current.mode === "all_eligible"
          ? "Dynamic: any host that can run this app is used, including hosts added later."
          : "Fixed: only the ticked hosts. A host added later is not used until you tick it."}
      </span>
      <ResourceStates loading={false} error={hosts.errorMessage} />
      {imageStates}
      <div className="ae-list">
        {rows.length === 0 ? (
          <span className="hint">No hosts are registered yet.</span>
        ) : (
          rows.map((r) => (
            <div key={r.id} className="ae-item">
              <div>
                {current.mode === "fixed" ? (
                  <label className="check">
                    <input
                      type="checkbox"
                      checked={current.hostIds.includes(r.id)}
                      disabled={pending}
                      onChange={() => toggle(r.id)}
                    />
                    {r.name}
                  </label>
                ) : (
                  <div className="ae-item-t">{r.name}</div>
                )}
                <HostDetail obs={r.obs} prep={prep} retry={retry} retrying={retryAction.pending?.[0] === r.id} />
              </div>
              <HostStates obs={r.obs} unmanaged={unmanaged} />
            </div>
          ))
        )}
      </div>
      {STATES_NOTE}
      {current.mode === "fixed" && current.hostIds.length === 0 && (
        <div className="note warn">
          <div>
            <b>No host is selected.</b> Saved like this, nobody can launch this app until a host is
            ticked.
          </div>
        </div>
      )}
      {LOCALITY_NOTE}
      <div className="row gap2">
        <Button
          variant="ghost"
          disabled={!draft || pending}
          onClick={() => {
            setDraft(null);
            setConflict(null);
          }}
        >
          Discard
        </Button>
        <Button
          variant="primary"
          disabled={!dirty || pending}
          onClick={() => void save.run(current, current.revision)}
        >
          {pending ? "Saving…" : "Save placement"}
        </Button>
      </div>
    </Section>
  );
}
