// CM-05 — admin console-mode settings page for one host (handoff-v3-spec
// §A.6). Reads/writes the CM-01 console-config API (GET/PATCH
// /v1/admin/hosts/{id}/console-config).
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
import type { AdminApp, AdminUser, ConsoleConfig } from "../../../api/types";
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
import { consoleAudioBackend, readsAsOn } from "./console/access";
import { ConsoleAccessNote } from "./console/ConsoleAccessNote";
import { ConsoleAccessConfirmModal } from "./console/ConsoleAccessConfirmModal";
import { InputDevicesRow } from "./console/InputDevicesRow";
import { CapabilitiesRail } from "./console/CapabilitiesRail";

const AUTO = "auto";
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

/** One `.cset` row: title + help on the left, one control on the right. */
function ConsoleRow({ title, help, children }: { title: string; help: ReactNode; children: ReactNode }) {
  return (
    <div className="cset">
      <div>
        <h3>{title}</h3>
        <p className="hint">{help}</p>
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
  const apps = data?.apps ?? [];
  const users = data?.users ?? [];

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
    capabilities.audio_sinks.length > 0 ||
    capabilities.input_devices.length > 0
  );
  // RH07-15 (#407): the reported audio_sinks are one family or the other —
  // the agent hides ALSA hw:* sinks while its host's PipeWire answers — so
  // the help line under the selector names whichever this host reported
  // (design_handoff_v3/screens/rh07/README.md specimens "on" / "on-alsa").
  const audioBackend = consoleAudioBackend(capabilities?.audio_sinks);
  const audioHelp =
    audioBackend === "pipewire"
      ? "This machine runs PipeWire, so console audio plays through it, beside the desktop's own sound. Quasar never takes the sound device from it."
      : audioBackend === "alsa"
        ? "No PipeWire runs on this machine, so console audio goes straight to the sound device (ALSA)."
        : "Host sink for console-mode audio. Quiet plays no local audio.";
  const connectedOutputs = (capabilities?.outputs ?? []).filter((output) => output.connected);
  const selectedOutput = connectedOutputs.find((output) => output.id === effective.output_id);
  const selectedModeValue = effective.mode
    ? `${effective.mode.width}x${effective.mode.height}@${effective.mode.refresh_millihz}`
    : NONE;

  const discard = () => setPending({});

  const save = async () => {
    if (!token || !id || changedCount === 0) return;
    setSaving(true);
    try {
      const saved = await adminApi.updateConsoleConfig(token, id, pending);
      res.setData((prev) => ({ ...prev, config: saved.config, capabilities: saved.capabilities }));
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
        sub={`Local display on ${host ? host.node_name : "this host"} with an explicit per-session output topology`}
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
                    ? "This machine shows games on its own screen, and can stream them too."
                    : "Local display with an explicit per-session output topology."}
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

            <Group title="Video" />

            <ConsoleRow
              title="Video topology"
              help={<>Local-only uses no encoder or WebRTC signaling resources. Dual output adds a
                browser stream from the same VulkanImage source. Select a card-scoped output
                and exact reported timing, or leave both automatic.</>}
            >
              <span className="mono t-xs muted">
                Weston · Static mode · Fullscreen
              </span>
            </ConsoleRow>

            <ConsoleRow title="Physical output" help="Card-scoped DRM connector. Automatic uses Weston's preferred connected output.">
              <select
                className="select"
                aria-label="Physical output"
                disabled={locked}
                value={effective.output_id ?? NONE}
                onChange={(e) => {
                  const output = connectedOutputs.find((item) => item.id === e.target.value);
                  const preferred = output?.modes.find((mode) => mode.preferred) ?? output?.modes[0];
                  setPending((prev) => ({
                    ...prev,
                    output_id: output?.id ?? null,
                    mode: preferred ? {
                      width: preferred.width,
                      height: preferred.height,
                      refresh_millihz: preferred.refresh_millihz,
                    } : null,
                  }));
                }}
              >
                <option value={NONE}>Automatic</option>
                {connectedOutputs.map((output) => (
                  <option key={output.id} value={output.id}>{output.id}</option>
                ))}
              </select>
            </ConsoleRow>

            <ConsoleRow title="Physical mode" help="Exact DRM timing identity; fractional refresh rates are preserved.">
              <select
                className="select"
                aria-label="Physical mode"
                style={{ width: 260 }}
                disabled={locked || !selectedOutput}
                value={selectedModeValue}
                onChange={(e) => {
                  // "Preferred" stores the output's preferred mode: the API pins
                  // output_id and mode together, so it cannot store a null mode (#422).
                  const mode = e.target.value === NONE
                    ? selectedOutput?.modes.find((item) => item.preferred) ?? selectedOutput?.modes[0]
                    : selectedOutput?.modes.find((item) =>
                      `${item.width}x${item.height}@${item.refresh_millihz}` === e.target.value);
                  if (mode) setField("mode", {
                    width: mode.width, height: mode.height, refresh_millihz: mode.refresh_millihz,
                  });
                }}
              >
                <option value={NONE}>Preferred</option>
                {(selectedOutput?.modes ?? []).map((mode, index) => (
                  <option key={`${mode.width}x${mode.height}@${mode.refresh_millihz}-${index}`} value={`${mode.width}x${mode.height}@${mode.refresh_millihz}`}>
                    {mode.width}×{mode.height} @ {(mode.refresh_millihz / 1000).toFixed(3)} Hz{mode.preferred ? " · preferred" : ""}
                  </option>
                ))}
              </select>
            </ConsoleRow>

            <Group title="Streaming" />

            <ConsoleRow title="Also stream" help="Adds WebRTC video for dual output. Off is local-only.">
              <Switch label="Also stream" checked={Boolean(effective.stream)} disabled={locked} onChange={(v) => setField("stream", v)} />
            </ConsoleRow>

            <ConsoleRow title="Stream audio" help="Adds the WebRTC Opus audio leg when streaming is enabled.">
              <Switch
                label="Stream audio"
                checked={Boolean(effective.stream_audio)}
                disabled={locked || !effective.stream}
                onChange={(v) => setField("stream_audio", v)}
              />
            </ConsoleRow>

            <Group title="Local input and audio" />

            <ConsoleRow title="Local audio output" help={audioHelp}>
              <select
                className="select"
                aria-label="Local audio output"
                disabled={locked}
                value={effective.audio_output ?? NONE}
                onChange={(e) => {
                  const v = e.target.value;
                  setField("audio_output", v === NONE ? null : v);
                }}
              >
                <option value={AUTO}>Auto</option>
                <option value={NONE}>Quiet (no local audio)</option>
                {(capabilities?.audio_sinks ?? []).map((s) => (
                  <option key={s.id} value={s.id}>{s.label}</option>
                ))}
              </select>
            </ConsoleRow>

            <ConsoleRow title="Grab local input" help="Exclusively grab the physical keyboard/mouse for the console session.">
              <Switch label="Grab local input" checked={Boolean(effective.grab)} disabled={locked} onChange={(v) => setField("grab", v)} />
            </ConsoleRow>

            <InputDevicesRow
              value={effective.input_devices}
              devices={capabilities?.input_devices ?? []}
              onChange={(v) => setField("input_devices", v)}
              disabled={locked}
            />

            <Group title="Startup" />

            <ConsoleRow title="Default app" help="App auto-launched on console start.">
              <select
                className="select"
                disabled={locked}
                value={effective.default_app ?? NONE}
                onChange={(e) => {
                  const v = e.target.value;
                  setField("default_app", v === NONE ? null : v);
                }}
              >
                <option value={NONE}>None</option>
                {apps.map((a: AdminApp) => (
                  <option key={a.id} value={a.id}>{a.name}</option>
                ))}
              </select>
            </ConsoleRow>

            <ConsoleRow title="Default user" help="Owner of auto-started console sessions. Required for auto-start on display.">
              <select
                className="select"
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

            <ConsoleRow title="Auto-start on display" help="Auto-launch the console session when a display connects.">
              <Switch
                label="Auto-start on display"
                checked={Boolean(effective.auto_start_on_display)}
                disabled={locked}
                onChange={(v) => setField("auto_start_on_display", v)}
              />
            </ConsoleRow>

            <ConsoleRow title="Auto-connect controller" help="Auto-attach a connected controller to the console session.">
              <Switch
                label="Auto-connect controller"
                checked={Boolean(effective.auto_connect_controller)}
                disabled={locked}
                onChange={(v) => setField("auto_connect_controller", v)}
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
