// Library ▸ Compatibility ▸ Add tool (mock #1067 v2 compat-picker): the curated
// list this release ships, then the latest GE-Proton releases from github.com.
// Mounted only while open, so the list is read when the admin asks for it.

import * as adminApi from "../../../api/admin";
import type { CompatToolAvailable } from "../../../api/admin";
import { ApiError } from "../../../api/client";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { bytes } from "../../../lib/format/bytes";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { GITHUB_UNAVAILABLE, pickerState } from "./compatDerived";

interface Props {
  /** Size per tool id, from the catalogue the page already holds: the available
   *  list itself carries none. */
  sizeByToolId: Map<string, number>;
  onClose: () => void;
  /** An install was started (or its tool is now desired everywhere). */
  onInstalled: () => void;
}

/** The pin took but the enable did not: the tool is in the catalogue, disabled. */
class AddedNotEnabled extends Error {}

function released(iso: string | null): string | null {
  return iso ? new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric" }) : null;
}

export function AddToolModal({ sizeByToolId, onClose, onInstalled }: Props) {
  const { token } = useAuth();
  const list = useResource<adminApi.CompatToolAvailableList>({
    label: "available compatibility tools",
    fetch: (ctx) => adminApi.listAvailableCompatTools(ctx.token, ctx.signal),
  });

  // A release with no catalogue row is pinned first (POST), then enabled (PATCH);
  // one with a row is only enabled. The enable is what installs on every host.
  // A stale list can POST a tag that has since been pinned (409 compat_tool_exists,
  // e.g. a retry after an enable failed): look the row up by tag and enable it.
  const install = useAdminAction(
    async (a: CompatToolAvailable) => {
      if (!token) throw new Error("not signed in");
      let id = a.tool_id;
      if (id === null) {
        try {
          id = (await adminApi.addCompatTool(token, a.tag)).id;
        } catch (e) {
          if (!(e instanceof ApiError && e.code === "compat_tool_exists")) throw e;
          const row = (await adminApi.listCompatTools(token)).items.find((t) => t.tag === a.tag);
          if (!row) throw e;
          id = row.id;
        }
      }
      try {
        await adminApi.setCompatToolEnabled(token, id, true);
      } catch (e) {
        if (a.tool_id === null) throw new AddedNotEnabled(e instanceof Error ? e.message : "");
        throw e;
      }
    },
    {
      success: (_r, a) => `Installing ${a.display_name} on every host`,
      failure: (e, a) =>
        e instanceof AddedNotEnabled
          ? { title: `${a.display_name} was added but not enabled`, body: e.message || undefined }
          : { title: `Could not install ${a.display_name}`, body: e instanceof Error ? e.message : undefined },
      onSuccess: onInstalled,
      onFailure: () => list.refresh(),
    },
  );

  const data = list.data;
  const recommended = data?.items.filter((a) => a.source === "recommended") ?? [];
  const latest = data?.items.filter((a) => a.source === "github_latest") ?? [];

  function row(a: CompatToolAvailable) {
    const state = pickerState(a);
    const size = a.tool_id ? sizeByToolId.get(a.tool_id) : undefined;
    const meta =
      state.kind === "pin_conflict"
        ? "The release changed after its checksum was pinned, so it can't be installed."
        : state.kind === "name_conflict"
          ? "A host reported a name Steam can't use for it. Remove it, then add it again."
          : [released(a.published_at) && `Released ${released(a.published_at)}`, size != null && `${bytes(size)} per host`]
              .filter(Boolean)
              .join(" · ");
    return (
      <div className="ae-item" key={a.tag}>
        <div>
          <div className="ae-item-t">{a.display_name}</div>
          {meta && <div className="ae-item-m">{meta}</div>}
        </div>
        {state.kind === "installable" ? (
          <Button size="sm" disabled={install.pending != null} onClick={() => void install.run(a)}>
            Install
          </Button>
        ) : state.kind === "installed" ? (
          <Chip variant="success">installed</Chip>
        ) : state.kind === "installing" ? (
          <Chip variant="info" dot>installing</Chip>
        ) : state.kind === "failed" ? (
          <Chip variant="danger">install failed</Chip>
        ) : state.kind === "waiting" ? (
          <Chip>waiting for hosts</Chip>
        ) : (
          <Chip variant="danger">{state.kind === "pin_conflict" ? "pin conflict" : "name conflict"}</Chip>
        )}
      </div>
    );
  }

  return (
    <Modal
      open
      onClose={onClose}
      title="Add tool"
      maxWidth={560}
      footer={<Button variant="ghost" onClick={onClose}>Close</Button>}
    >
      <p className="hint">
        Quasar downloads the release from github.com, checks the checksum published with it, then installs it on
        every registered host. Progress shows in the table.
      </p>
      <ResourceStates loading={list.loading} error={list.errorMessage} />
      {data && (
        <>
          <p className="eyebrow mt4">Recommended with this Quasar release</p>
          <div className="ae-list">{recommended.map(row)}</div>
          <p className="eyebrow mt4">Latest GE-Proton releases · from github.com</p>
          {data.github_latest.available ? (
            <div className="ae-list">{latest.map(row)}</div>
          ) : (
            <div className="note warn">
              <div>
                {GITHUB_UNAVAILABLE[data.github_latest.reason ?? ""] ?? "The latest releases could not be loaded."} The
                recommended list above still installs.
              </div>
            </div>
          )}
        </>
      )}
    </Modal>
  );
}
