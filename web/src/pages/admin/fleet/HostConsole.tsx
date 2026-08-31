// CM-05 — admin console-mode settings page for one host (handoff-v3-spec
// §A.6). Reads/writes the CM-01 console-config API (GET/PATCH
// /v1/admin/hosts/{id}/console-config).
//
// Input devices is the one place this page goes beyond a straight restyle
// (InputDevicesRow, in ./console/): `ConsoleConfig.input_devices` is
// `"auto" | string[]` of device paths and `ConsoleCapabilities.input_devices`
// reports only `{path, label}` — no server-side class or hot-plug/pinned
// distinction, so class is a client-side label heuristic, display only.

import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";
import { ApiError } from "../../../api/client";
import * as adminApi from "../../../api/admin";
import type { AdminApp, AdminUser, ConsoleConfig } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Breadcrumbs } from "../../../components/Breadcrumbs";
import { shortId } from "../../../lib/format/shortId";
import { Button } from "../../../components/Button";
import { LoadingState } from "../../../components/LoadingState";
import { PageHeader } from "../../../components/PageHeader";
import { useToast } from "../../../components/Toast";
import { useConsoleLoad } from "./console/useConsoleLoad";
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
  return <div className="eyebrow" style={{ padding: "var(--s5) var(--card-pad) 2px" }}>{title}</div>;
}

export function HostConsole() {
  const { token } = useAuth();
  const { id } = useParams();
  const { addToast } = useToast();

  const { host, config, capabilities, apps, users, loading, error, setError, setLoaded } = useConsoleLoad(id);
  const [pending, setPending] = useState<ConsoleConfig>({});
  const [saving, setSaving] = useState(false);

  // A fresh host means a fresh draft.
  useEffect(() => setPending({}), [id]);

  const changedCount = Object.keys(pending).length;
  const effective = useMemo<ConsoleConfig>(() => ({ ...config, ...pending }), [config, pending]);

  const setField = <K extends keyof ConsoleConfig>(key: K, value: ConsoleConfig[K]) => {
    setPending((prev) => ({ ...prev, [key]: value }));
  };

  const hasCapabilities = capabilities != null && (
    capabilities.connectors.length > 0 ||
    (capabilities.outputs?.length ?? 0) > 0 ||
    capabilities.audio_sinks.length > 0 ||
    capabilities.input_devices.length > 0
  );
  const connectedOutputs = (capabilities?.outputs ?? []).filter((output) => output.connected);
  const selectedOutput = connectedOutputs.find((output) => output.id === effective.output_id);
  const selectedModeValue = effective.mode
    ? `${effective.mode.width}x${effective.mode.height}@${effective.mode.refresh_millihz}`
    : NONE;

  const discard = () => setPending({});

  const save = async () => {
    if (!token || !id || changedCount === 0) return;
    setSaving(true);
    setError(null);
    try {
      const res = await adminApi.updateConsoleConfig(token, id, pending);
      setLoaded(res.config, res.capabilities);
      setPending({});
      addToast({ variant: "success", title: "Console config saved" });
    } catch (e: unknown) {
      const msg = e instanceof ApiError ? e.message : "Save failed.";
      setError(msg);
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
            <Button variant="ghost" disabled={loading || saving || changedCount === 0} onClick={discard}>
              Discard
            </Button>
            <Button variant="primary" disabled={loading || saving || changedCount === 0} onClick={() => void save()}>
              {saving ? "Saving…" : "Save changes"}
            </Button>
          </>
        }
      />

      {loading && <LoadingState>Loading...</LoadingState>}
      {error && <p className="form-error">{error}</p>}

      {!loading && (
        <div className="split" style={{ gridTemplateColumns: "minmax(0,1fr) 300px" }}>
          <div className="card">
            <div className="panel-head">
              <div>
                <span className="panel-title">Console mode</span>
                <p className="hint" style={{ marginTop: 3 }}>
                  Local display with an explicit per-session output topology.
                </p>
              </div>
              <div className="acts">
                <Switch label="Enabled" checked={Boolean(effective.enabled)} onChange={(v) => setField("enabled", v)} />
              </div>
            </div>

            <Group title="Video" />

            <ConsoleRow
              title="Video topology"
              help={<>Local-only uses no encoder or WebRTC signaling resources. Dual output adds a
                browser stream from the same VulkanImage source. Select a card-scoped output
                and exact reported timing, or leave both automatic.</>}
            >
              <span className="mono" style={{ fontSize: "var(--t-xs)", color: "var(--text-3)" }}>
                Weston · Static mode · Fullscreen
              </span>
            </ConsoleRow>

            <ConsoleRow title="Physical output" help="Card-scoped DRM connector. Automatic uses Weston's preferred connected output.">
              <select
                className="select"
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
                style={{ width: 260 }}
                disabled={!selectedOutput}
                value={selectedModeValue}
                onChange={(e) => {
                  const mode = selectedOutput?.modes.find((item) =>
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
              <Switch label="Also stream" checked={Boolean(effective.stream)} onChange={(v) => setField("stream", v)} />
            </ConsoleRow>

            <ConsoleRow title="Stream audio" help="Adds the WebRTC Opus audio leg when streaming is enabled.">
              <Switch
                label="Stream audio"
                checked={Boolean(effective.stream_audio)}
                disabled={!effective.stream}
                onChange={(v) => setField("stream_audio", v)}
              />
            </ConsoleRow>

            <Group title="Local input and audio" />

            <ConsoleRow title="Local audio output" help="Host sink for console-mode audio. Quiet plays no local audio.">
              <select
                className="select"
                aria-label="Local audio output"
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
              <Switch label="Grab local input" checked={Boolean(effective.grab)} onChange={(v) => setField("grab", v)} />
            </ConsoleRow>

            <InputDevicesRow
              value={effective.input_devices}
              devices={capabilities?.input_devices ?? []}
              onChange={(v) => setField("input_devices", v)}
            />

            <Group title="Startup" />

            <ConsoleRow title="Default app" help="App auto-launched on console start.">
              <select
                className="select"
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
                onChange={(v) => setField("auto_start_on_display", v)}
              />
            </ConsoleRow>

            <ConsoleRow title="Auto-connect controller" help="Auto-attach a connected controller to the console session.">
              <Switch
                label="Auto-connect controller"
                checked={Boolean(effective.auto_connect_controller)}
                onChange={(v) => setField("auto_connect_controller", v)}
              />
            </ConsoleRow>
          </div>

          <div className="col gap4">
            <div className="card card-pad">
              <div className="eyebrow">Host</div>
              <h3 style={{ fontSize: "var(--t-h3)", marginTop: 6 }}>{host?.node_name ?? "Unknown host"}</h3>
              <div className="mono" style={{ color: "var(--text-3)", fontSize: "var(--t-xs)", marginTop: 3 }} title={host?.id}>
                {shortId(host?.id)}
              </div>
            </div>
            <div className="card card-pad">
              <div className="eyebrow">Overrides</div>
              <div style={{ fontFamily: "var(--font-display)", fontSize: "1.7rem", fontWeight: 600, marginTop: 6 }}>
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
    </section>
  );
}
