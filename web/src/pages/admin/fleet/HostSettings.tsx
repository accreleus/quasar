// Full-page host runtime settings editor (handoff-v3-spec §A.7): catalog-driven
// knob editor on the v3 `.split`/`.cset` layout. See
// fleet/settings/useHostSettings.ts for the state machine and the API calls, and
// fleet/settings/{KnobPanel,KnobRow,KnobControl,RestartNote,SettingsRail}.tsx
// for the pieces this composes.
//
// Two fields have no counterpart in the mock's
// `.cset` row (label + help + control, nothing else): "Current value" (the
// agent's true running value, distinct from an unsaved override) and the
// stale-effective warning ("Agent is still running X. Restart pending." for a
// restart-class knob whose saved override hasn't reached the agent). Both are
// real signal a settings page must not drop, so they render as a `.hint`
// under each knob's control — see KnobRow.tsx.

import { useParams } from "react-router-dom";
import { useCallback, useState } from "react";
import type { ConfigKnob } from "../../../api/types";
import { Breadcrumbs } from "../../../components/Breadcrumbs";
import { shortId } from "../../../lib/format/shortId";
import { Button } from "../../../components/Button";
import { LoadingState } from "../../../components/LoadingState";
import { PageHeader } from "../../../components/PageHeader";
import { KnobPanel } from "./settings/KnobPanel";
import { KnobRow } from "./settings/KnobRow";
import { RestartNote } from "./settings/RestartNote";
import { SettingsRail } from "./settings/SettingsRail";
import { useHostSettings } from "./settings/useHostSettings";
import { IdleApplyPolicy } from "./settings/IdleApplyPolicy";
import { SafeSettingsPolicy } from "./settings/SafeSettingsPolicy";
import type { SettingValue } from "./settings/knobs";

/** Hardware stays on the legacy editor until this host confirms typed ownership. */
const RESTART_GROUP_KEYS = new Set(["encoder", "render_node", "cuda_device"]);

export function HostSettings() {
  const { id } = useParams();
  const s = useHostSettings(id);
  // Resolve writer ownership before exposing either editor. An owned first
  // edit must use the policy endpoint's expected revision even before a group
  // row exists; the legacy editor has no client CAS token. Until the typed
  // panel reports, every key it could own stays hidden here.
  const [ownership, setOwnership] = useState<{ hostId: string | undefined; keys: Set<string> } | null>(null);
  const typedKeys = ownership && ownership.hostId === id ? ownership.keys : null;
  const onOwnedKeys = useCallback((keys: Set<string>) => setOwnership({ hostId: id, keys }), [id]);
  const legacyOwns = (k: ConfigKnob) => (typedKeys ? !typedKeys.has(k.key) : RESTART_GROUP_KEYS.has(k.key));

  const legacy = {
    runtime: s.grouped.runtime.filter(legacyOwns),
    adaptation: s.grouped.adaptation.filter(legacyOwns),
    encoder: s.grouped.encoder.filter(legacyOwns),
    advanced: s.grouped.advanced.filter(legacyOwns),
  };

  const renderKnob = (k: ConfigKnob) => {
    const v = s.valueOf(k);
    const changed = k.key in s.overrides;
    const effectiveValue = s.effectiveValueOf(k);
    const currentValue = effectiveValue !== undefined ? effectiveValue : s.resolved[k.key];

    return (
      <KnobRow
        key={k.key}
        knob={k}
        value={v}
        changed={changed}
        currentValue={currentValue}
        showsStaleEffective={s.isStaleEffective(k)}
        effectiveValue={effectiveValue}
        defaultValue={k.default as SettingValue | undefined}
        onChange={(next) => s.setValue(k.key, next)}
        onReset={() => s.resetKey(k.key)}
        renderNodeOptions={s.renderNodeOptions}
      />
    );
  };

  return (
    <section className="page">
      <Breadcrumbs
        items={[
          { label: "Fleet", to: "/admin/fleet/hosts" },
          { label: s.host ? s.host.node_name : shortId(id), title: s.host ? undefined : (id ?? undefined), to: id ? `/admin/fleet/hosts/${id}` : undefined },
          { label: "Settings" },
        ]}
      />
      <PageHeader
        title="Host settings"
        sub={`Runtime configuration for ${s.host ? s.host.node_name : "this host"}. Unset values use the host's deployment setting.`}
        actions={
          <>
            <Button variant="ghost" disabled={s.loading || s.saving || !s.dirty} onClick={s.discard}>
              Discard
            </Button>
            <Button variant="primary" disabled={s.loading || s.saving || !s.dirty} onClick={s.saveChanges}>
              {s.saving ? "Saving…" : "Save changes"}
            </Button>
          </>
        }
      />

      {s.loading && <LoadingState>Loading...</LoadingState>}
      {s.error && <p className="form-error">{s.error}</p>}

      {!s.loading && (
        <>
          <RestartNote
            hasDirtyRestart={s.hasDirtyRestart}
            dirtyRestartCount={s.dirtyRestartKnobs.length}
            confirmRestart={s.confirmRestart}
            restartConfirmPending={s.restartConfirmPending}
            showRestartButton={s.showRestartButton}
            pendingRestart={s.pendingRestart}
            liveSessionsCount={s.restartLiveSessions ?? s.liveSessionsCount}
            saving={s.saving}
            restarting={s.restarting}
            onCancelConfirm={s.cancelSaveConfirm}
            onSaveConfirm={() => void s.save(true)}
            onCancelRestartPending={s.cancelRestartConfirm}
            onConfirmRestartPending={() => void s.handleRestart(true)}
            onRestartNow={() => void s.handleRestart(false)}
          />

          <div className="split mt4" style={{ gridTemplateColumns: "minmax(0,1fr) 300px" }}>
            <div>
              <SafeSettingsPolicy hostId={id} knobs={s.knobs} renderNodeOptions={s.renderNodeOptions} onOwnedKeys={onOwnedKeys} />
              <IdleApplyPolicy hostId={id} />
              <KnobPanel title="Runtime defaults" hint="Changes affect new sessions after the host applies them.">
                {legacy.runtime.map(renderKnob)}
              </KnobPanel>

              {legacy.adaptation.length > 0 && <KnobPanel
                title="Adaptation"
                hint="How a session degrades when the network cannot carry it. Changes apply to the next session launched on this host."
              >
                {legacy.adaptation.map(renderKnob)}
              </KnobPanel>}

              <KnobPanel
                title="Encoder and GPU"
                hint="Read by new agent processes. Changing these requires an agent restart."
              >
                {legacy.encoder.map(renderKnob)}
              </KnobPanel>

              {legacy.advanced.length > 0 && <KnobPanel
                title="Advanced streaming tuning"
                hint="Rarely changed. Wrong values here degrade every session on the host."
                actions={
                  <Button variant="ghost" size="sm" onClick={() => s.setShowAdvanced((v) => !v)}>
                    {s.showAdvanced ? "Hide advanced" : "Show advanced"}
                  </Button>
                }
              >
                {s.showAdvanced ? legacy.advanced.map(renderKnob) : (
                  <p className="hint knob-hidden">
                    {legacy.advanced.length} advanced controls hidden.
                  </p>
                )}
              </KnobPanel>}
            </div>

            <SettingsRail
              host={s.host}
              changedCount={s.changedCount}
              liveSessionsCount={s.liveSessionsCount}
              pendingRestart={s.pendingRestart || s.hasStaleEffectiveKnob}
              restarting={s.restarting}
              onRestart={() => void s.handleRestart(false)}
            />
          </div>
        </>
      )}
    </section>
  );
}
