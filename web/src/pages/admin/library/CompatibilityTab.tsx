// Library ▸ Compatibility (mock #1067 v2, spec #1063): the default tool, one
// table of installed tools, and Add tool. Valve's own Proton builds need no
// install, so they are choices in the selects only. Server-enforced: every
// route is admin-gated (control-api.md §Compatibility tools).

import { Fragment, useMemo, useState } from "react";
import * as adminApi from "../../../api/admin";
import type { CompatTool } from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { CompatToolList } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { IconPlus } from "../../../components/icons";
import { ResourceStates } from "../../../components/ResourceStates";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { SelectField } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { bytes } from "../../../lib/format/bytes";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { AddToolModal } from "./AddToolModal";
import {
  hostNotes,
  hostTotal,
  installedTools,
  installState,
  refusalCopy,
  selectableInstalled,
  valveBuilds,
} from "./compatDerived";
import "../../../styles/admin/editor.css";
import "../../../styles/admin/fleet.css";

/** An install changes under the admin's eyes; a chained, visibility-aware poll. */
const POLL_MS = 5_000;

export function CompatibilityTab() {
  const { token } = useAuth();
  const { addToast } = useToast();
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<ReadonlySet<string>>(new Set());
  const [refused, setRefused] = useState<Record<string, string>>({});

  const res = useResource<CompatToolList>({
    label: "compatibility tools",
    pollMs: POLL_MS,
    fetch: (ctx) => adminApi.listCompatTools(ctx.token, ctx.signal),
  });

  const items = res.data?.items ?? [];
  const installed = installedTools(items);
  const sizeByToolId = useMemo(
    () => new Map(items.flatMap((t) => (t.asset ? [[t.id, t.asset.size_bytes] as const] : []))),
    [items],
  );

  useSectionHead({
    sub: "What your games can run on",
    counts: res.data ? { compatibility: installed.length } : undefined,
  });

  const setDefault = useAdminAction(
    (toolId: string | null) => adminApi.setCompatToolDefault(token!, toolId),
    {
      success: "Default tool saved",
      failure: "Could not save the default tool",
      onSuccess: () => res.refresh(),
    },
  );

  async function remove(tool: CompatTool) {
    if (!token) return;
    setRefused(({ [tool.id]: _drop, ...rest }) => rest);
    setRemoving((s) => new Set(s).add(tool.id));
    try {
      // mutate, not a bare call: the poll is suspended for the write (a GET that
      // started before it must not stomp the refresh below).
      await res.mutate((ctx) => adminApi.setCompatToolEnabled(ctx.token, tool.id, false));
      addToast({ variant: "success", title: `${tool.display_name} will be removed from every host` });
      res.refresh();
    } catch (e: unknown) {
      const inUse = e instanceof ApiError && e.code === "compat_tool_in_use";
      const msg = inUse
        ? refusalCopy(tool, e.message)
        : `Not removed: ${e instanceof ApiError ? e.message : "Could not reach the control plane."}`;
      setRefused((r) => ({ ...r, [tool.id]: msg }));
    } finally {
      setRemoving((s) => {
        const next = new Set(s);
        next.delete(tool.id);
        return next;
      });
    }
  }

  const defaultId = res.data?.default_tool_id ?? "";
  const selectedInstalled = selectableInstalled(items);

  return (
    <section className="page">
      <ResourceStates loading={res.loading} error={res.errorMessage} />
      {res.data && (
        <>
          <div className="card card-pad">
            <SelectField
              label="Default tool"
              id="default-tool"
              value={defaultId}
              disabled={setDefault.pending != null}
              onChange={(e) => void setDefault.run(e.target.value || null)}
              hint="Used by every Windows game with no tool of its own. Steam's default means Quasar does nothing. A player's own choice in Steam still wins, unless the game's tool is forced."
            >
              <option value="">Steam's default</option>
              {selectedInstalled.length > 0 && (
                <optgroup label="Installed tools">
                  {selectedInstalled.map((t) => (
                    <option key={t.id} value={t.id}>{t.display_name}</option>
                  ))}
                </optgroup>
              )}
              <optgroup label="Valve's builds">
                {valveBuilds(items).map((t) => (
                  <option key={t.id} value={t.id}>{t.display_name}</option>
                ))}
              </optgroup>
            </SelectField>
          </div>

          <div className="card mt4">
            <div className="panel-head">
              <div>
                <span className="panel-title">Installed tools</span>
                <div className="hint">
                  Installed on every registered host. Valve's own Proton builds need no install and appear only in the selects.
                </div>
              </div>
              <div className="acts">
                <Button size="sm" onClick={() => setAdding(true)}>
                  <IconPlus /> Add tool
                </Button>
              </div>
            </div>
            <div className="table-wrap">
              <table className="qtable">
                <thead>
                  <tr>
                    <th>Tool</th>
                    <th>Installed on</th>
                    <th>Games using it</th>
                    <th className="right"><span className="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  {installed.length === 0 && (
                    <tr className="qtable-empty-row">
                      <td colSpan={4}>No tools installed. Add a GE-Proton release to install it on every host.</td>
                    </tr>
                  )}
                  {installed.map((t) => {
                    const state = installState(t.hosts);
                    const total = t.hosts ? hostTotal(t.hosts) : 0;
                    return (
                      <Fragment key={t.id}>
                        <tr>
                          <td>
                            <div className="stack">
                              <span className="primary">{t.display_name}</span>
                              {t.asset && <span className="sub">{bytes(t.asset.size_bytes)} per host</span>}
                            </div>
                          </td>
                          <td>
                            <span id={`compat-state-${t.id}`}>
                              {state === "installing" && <Chip variant="info" dot>Installing</Chip>}
                              {state === "failed" && <Chip variant="danger">Failed</Chip>}
                              {state === "waiting" && <Chip>Waiting</Chip>}
                            </span>{" "}
                            <span className="num">{t.hosts?.installed ?? 0} / {total}</span>{" "}
                            <span className="sub">
                              {["hosts", ...(t.hosts ? hostNotes(t.hosts) : [])].join(" · ")}
                            </span>
                          </td>
                          <td className="num">{t.mappings}</td>
                          <td className="right">
                            <Button
                              variant="ghost"
                              size="sm"
                              aria-label={`Remove ${t.display_name}`}
                              aria-describedby={state === "installing" ? `compat-state-${t.id}` : undefined}
                              disabled={state === "installing" || removing.has(t.id)}
                              onClick={() => void remove(t)}
                            >
                              Remove
                            </Button>
                          </td>
                        </tr>
                        {refused[t.id] && (
                          <tr className="exp-row">
                            <td colSpan={4}>
                              <p className="form-error run-error" role="alert">{refused[t.id]}</p>
                            </td>
                          </tr>
                        )}
                      </Fragment>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>
        </>
      )}
      {adding && (
        <AddToolModal
          sizeByToolId={sizeByToolId}
          onClose={() => setAdding(false)}
          onInstalled={() => {
            setAdding(false);
            res.refresh();
          }}
        />
      )}
    </section>
  );
}
