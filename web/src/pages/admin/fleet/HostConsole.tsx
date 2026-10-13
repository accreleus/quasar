// CM-05 — admin console-mode settings page for one host (handoff-v3-spec
// §A.6). Reads/writes the CM-01 console-config API (GET/PATCH
// /v1/admin/hosts/{id}/console-config).
//
// The console desktop owns its mode, audio output and input, so the page edits
// only what control-api.md §Console mode defines. The default-app list is the
// server's `default_apps`; its `console_default_app` check explains a saved app
// that cannot run direct.
//
// Input devices is the one place this page goes beyond a straight restyle
// (InputDevicesRow, in ./console/): `ConsoleConfig.input_devices` is
// `"auto" | string[]` of device paths and `ConsoleCapabilities.input_devices`
// reports only `{path, label}` — no server-side class or hot-plug/pinned
// distinction, so class is a client-side label heuristic, display only.
//
// Console access (amendment 18, RH07 #395): a host whose agent reports
// `capabilities.access` needs console mode's `enabled` switch to go through a
// confirmation and an immediate PATCH — turning it on or off replaces the
// node agent through the recovery actor and ends the host's live sessions —
// rather than riding the draft/"Save changes" flow the rest of this page
// uses. A host reporting no `access` (Compose/source/older agent) keeps
// today's page exactly: `access` stays undefined and every branch below
// falls through to the original behaviour.

import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AdminUser, ConsoleConfig } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Breadcrumbs } from "../../../components/Breadcrumbs";
import { shortId } from "../../../lib/format/shortId";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { PageHeader } from "../../../components/PageHeader";
import { ResourceStates } from "../../../components/ResourceStates";
import { useToast } from "../../../components/Toast";
import { useAdminAction } from "../../../lib/resource/action";
import { useConsoleLoad } from "./console/useConsoleLoad";
import { readsAsOn } from "./console/access";
import { ConsoleAccessNote } from "./console/ConsoleAccessNote";
import { ConsoleAccessConfirmModal } from "./console/ConsoleAccessConfirmModal";
import { InputDevicesRow } from "./console/InputDevicesRow";
import { CapabilitiesRail } from "./console/CapabilitiesRail";

const NONE = "__none__";

function Switch({
  checked,
  onChange,
  disabled,
  label,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  label: string;
}) {
  return (
    <button
      className="switch"
      role="switch"
      aria-label={label}
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      type="button"
    />
  );
}

/** One `.cset` row: title + help (and an optional error) on the left, one control on the right. */
function ConsoleRow({
  title,
  help,
  error,
  children,
}: {
  title: string;
  help: ReactNode;
  error?: string | null;
  children: ReactNode;
}) {
  return (
    <div className="cset">
      <div>
        <h3>{title}</h3>
        <p className="hint">{help}</p>
        {error && <p className="form-error mt1">{error}</p>}
      </div>
      <div>{children}</div>
    </div>
  );
}

/** `.eyebrow` group header between `.cset` rows. */
function Group({ title }: { title: string }) {
  return <div className="eyebrow console-group">{title}</div>;
}

export function HostConsole() {
  const { token } = useAuth();
  const { id } = useParams();
  const { addToast } = useToast();

  const res = useConsoleLoad(id);
  const { data, loading, errorMessage } = res;
  const host = data?.host ?? null;
  const config = data?.config ?? null;
  const capabilities = data?.capabilities ?? null;
  const directApps = data?.defaultApps ?? [];
  const users = data?.users ?? [];
  const defaultAppCheck = (data?.readiness ?? []).find((check) => check.id === "console_default_app");

  const [pending, setPending] = useState<ConsoleConfig>({});
  const [saving, setSaving] = useState(false);
  const [confirmTarget, setConfirmTarget] = useState<boolean | null>(null);

  // A fresh host means a fresh draft.
  useEffect(() => setPending({}), [id]);

  const changedCount = Object.keys(pending).length;
  const effective = useMemo<ConsoleConfig>(() => ({ ...config, ...pending }), [config, pending]);

  const setField = <K extends keyof ConsoleConfig>(key: K, value: ConsoleConfig[K]) => {
    setPending((prev) => ({ ...prev, [key]: value }));
  };

  const access = capabilities?.access;
  const accessKnown = access != null;
  const applying = access?.state === "applying";
  const unsupported = access?.state === "unsupported";
  // The mockup's README: "applying (... settings locked)" — every control on
  // the page, not just the switch, while a replacement is in flight.
  const locked = applying;

  const toggleAccess = useAdminAction(
    async (nextEnabled: boolean) => {
      if (!token || !id) throw new Error("missing token or host id");
      return adminApi.updateConsoleConfig(token, id, { enabled: nextEnabled });
    },
    {
      success: (_r, nextEnabled) =>
        `Turning console mode ${nextEnabled ? "on" : "off"} on ${host?.node_name ?? "this host"}.`,
      failure: (e) => (e instanceof ApiError ? e.message : "Could not change console mode."),
      onSuccess: async () => {
        setConfirmTarget(null);
        await res.refresh({ silent: true });
      },
      onFailure: () => setConfirmTarget(null),
    },
  );

  const handleEnabledClick = (v: boolean) => {
    if (!accessKnown) {
      setField("enabled", v);
      return;
    }
    if (applying || unsupported) return;
    setConfirmTarget(v);
  };

  const tryAgain = () => {
    if (!access) return;
    void toggleAccess.run(access.target ?? !config?.enabled);
  };

  const hasCapabilities = capabilities != null && (
    capabilities.connectors.length > 0 ||
    (capabilities.outputs?.length ?? 0) > 0 ||
    capabilities.input_devices.length > 0
  );
  // The output pick means "this card, launch when this connector has a
  // monitor", so a reported output is offered whether or not one is plugged in.
  const outputs = capabilities?.outputs ?? [];
  // A saved default app the server does not offer (it cannot run direct, or is
  // gone) stays selectable as itself so the select shows the truth; the
  // console_default_app check, shown as the row's help, says why it will not
  // launch.
  const savedApp = config?.default_app ?? null;
  const savedAppOffered = savedApp == null || directApps.some((a) => a.id === savedApp);
  // The saved app's failure; a pending pick has not been checked yet.
  const defaultAppError =
    defaultAppCheck?.status === "fail" && !("default_app" in pending) ? defaultAppCheck.summary : null;

  const discard = () => setPending({});

  const save = async () => {
    if (!token || !id || changedCount === 0) return;
    setSaving(true);
    try {
      const saved = await adminApi.updateConsoleConfig(token, id, pending);
      res.setData((prev) => ({
        ...prev,
        config: saved.config,
        capabilities: saved.capabilities,
        // `?? []`: an older control plane predates this envelope.
        defaultApps: saved.default_apps ?? [],
        readiness: saved.readiness ?? [],
      }));
      setPending({});
      addToast({ variant: "success", title: "Console config saved" });
    } catch (e: unknown) {
      const msg = e instanceof ApiError ? e.message : "Save failed.";
      addToast({ variant: "danger", title: msg });
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="page">
      <Breadcrumbs
        items={[
          { label: "Fleet", to: "/admin/fleet/hosts" },
          { label: host ? host.node_name : shortId(id), title: host ? undefined : (id ?? undefined), to: id ? `/admin/fleet/hosts/${id}` : undefined },
          { label: "Local console" },
        ]}
      />
      <PageHeader
        title="Local console"
        sub={`The console desktop drives ${host ? host.node_name : "this host"}'s own display`}
        actions={
          <>
            <Button variant="ghost" disabled={loading || saving || locked || changedCount === 0} onClick={discard}>
              Discard
            </Button>
            <Button variant="primary" disabled={loading || saving || locked || changedCount === 0} onClick={() => void save()}>
              {saving ? "Saving…" : "Save changes"}
            </Button>
          </>
        }
      />

      <ResourceStates loading={loading} error={errorMessage} loadingLabel="Loading..." />

      {!loading && access && host && (
        <ConsoleAccessNote
          access={access}
          host={host}
          liveSessions={host.capacity?.active_sessions ?? null}
          onTryAgain={tryAgain}
          tryAgainPending={toggleAccess.pending != null}
        />
      )}

      {!loading && (
        <div className="split rail-split">
          <div className="card">
            <div className="panel-head">
              <div>
                <span className="panel-title">Console mode</span>
                <p className="hint mt1">
                  {accessKnown
                    ? "This machine shows games on its own screen."
                    : "The desktop drives this host's own display, with its own resolution, sound and input."}
                </p>
              </div>
              <div className="acts">
                {accessKnown && (
                  applying
                    ? <Chip variant="info">Applying</Chip>
                    : readsAsOn(config?.enabled, access)
                      ? <Chip variant="success">On</Chip>
                      : <Chip>Off</Chip>
                )}
                <Switch
                  label="Enabled"
                  checked={Boolean(accessKnown ? config?.enabled : effective.enabled)}
                  disabled={accessKnown ? applying || unsupported : false}
                  onChange={handleEnabledClick}
                />
              </div>
            </div>

            <Group title="Display" />

            <ConsoleRow
              title="Physical output"
              help="Card-scoped DRM connector. The console session starts when this connector has a monitor; Automatic uses any connected output. The desktop sets its own resolution and refresh."
            >
              <select
                className="select"
                aria-label="Physical output"
                disabled={locked}
                value={effective.output_id ?? NONE}
                onChange={(e) => {
                  const v = e.target.value;
                  setField("output_id", v === NONE ? null : v);
                }}
              >
                <option value={NONE}>Automatic</option>
                {outputs.map((output) => (
                  <option key={output.id} value={output.id}>
                    {output.connected ? output.id : `${output.id} · no monitor`}
                  </option>
                ))}
              </select>
            </ConsoleRow>

            <Group title="Input" />

            <InputDevicesRow
              value={effective.input_devices}
              devices={capabilities?.input_devices ?? []}
              onChange={(v) => setField("input_devices", v)}
              disabled={locked}
            />

            <Group title="Startup" />

            <ConsoleRow
              title="Default app"
              help="The app the console session runs. Only apps that can drive the display directly are offered."
              error={defaultAppError}
            >
              <select
                className="select"
                aria-label="Default app"
                disabled={locked}
                value={effective.default_app ?? NONE}
                onChange={(e) => {
                  const v = e.target.value;
                  setField("default_app", v === NONE ? null : v);
                }}
              >
                <option value={NONE}>None</option>
                {!savedAppOffered && savedApp != null && (
                  <option value={savedApp}>Current app (cannot run direct)</option>
                )}
                {directApps.map((a) => (
                  <option key={a.id} value={a.id}>{a.name}</option>
                ))}
              </select>
            </ConsoleRow>

            <ConsoleRow title="Default user" help="Owner of auto-started console sessions. Required for auto-start on display.">
              <select
                className="select"
                aria-label="Default user"
                disabled={locked}
                value={effective.default_user ?? NONE}
                onChange={(e) => {
                  const v = e.target.value;
                  setField("default_user", v === NONE ? null : v);
                }}
              >
                <option value={NONE}>None</option>
                {users.map((u: AdminUser) => (
                  <option key={u.id} value={u.id}>{u.username} ({u.email})</option>
                ))}
              </select>
            </ConsoleRow>

            <ConsoleRow title="Auto-start on display" help="Launch the console session when the output has a monitor.">
              <Switch
                label="Auto-start on display"
                checked={Boolean(effective.auto_start_on_display)}
                disabled={locked}
                onChange={(v) => setField("auto_start_on_display", v)}
              />
            </ConsoleRow>
          </div>

          <div className="col gap4">
            <div className="card card-pad">
              <div className="eyebrow">Host</div>
              <h3 className="t-h3 mt2">{host?.node_name ?? "Unknown host"}</h3>
              <div className="mono muted t-xs mt1" title={host?.id}>
                {shortId(host?.id)}
              </div>
            </div>
            <div className="card card-pad">
              <div className="eyebrow">Overrides</div>
              <div className="rail-stat">
                {changedCount}
              </div>
              <div className="hint">Unsaved field changes.</div>
            </div>
            <CapabilitiesRail
              capabilities={capabilities}
              hasCapabilities={hasCapabilities}
              inputDevicesValue={effective.input_devices}
            />
          </div>
        </div>
      )}

      {confirmTarget != null && host && (
        <ConsoleAccessConfirmModal
          hostName={host.node_name}
          turningOn={confirmTarget}
          liveSessions={host.capacity?.active_sessions ?? null}
          pending={toggleAccess.pending != null}
          onCancel={() => setConfirmTarget(null)}
          onConfirm={() => void toggleAccess.run(confirmTarget)}
        />
      )}
    </section>
  );
}
