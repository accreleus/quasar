import { useEffect, useMemo, useState } from "react";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { ConfigKnob } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { Chip, type ChipVariant } from "../../../../components/Chip";
import { useResource } from "../../../../lib/resource/react";
import { KnobControl, type RenderNodeOption } from "./KnobControl";
import { KnobPanel } from "./KnobPanel";
import { knobHelp, knobLabel, valueLabel, type SettingValue } from "./knobs";

type Group = adminApi.HostPolicyView["groups"][string];
type Draft = { source: "deployment" | "explicit"; value?: SettingValue };

const STATUS_CHIP: Record<string, ChipVariant> = { applied: "success", pending: "info", failed: "danger", uncertain: "warning" };

/** A group the typed writer owns on this host: next-session scope and negotiated. */
function typedOwned(group: Group | undefined): boolean {
  return Boolean(group && group.scope === "next_session" && group.status !== "upgrade_required");
}

/** Retry is offered only once the server says the transient budget is spent. */
function waitsForRetry(group: Group): boolean {
  return group.status === "failed" && Boolean(group.remedy?.startsWith("retry_exhausted"));
}

/** Strip the machine code (`retry_exhausted: `) the server prefixes to a remedy. */
function remedyText(remedy: string): string {
  return remedy.replace(/^[a-z_]+: /, "");
}

function sameChoice(a: Draft, b: Draft): boolean {
  return a.source === b.source && (a.source === "deployment" || a.value === b.value);
}

/** RH05 next-session settings through the typed policy API: one revisioned
 *  edit, then each group reports its own provenance, freshness and outcome.
 *  Restart-scope groups stay on the existing editor. */
export function SafeSettingsPolicy({
  hostId,
  knobs,
  renderNodeOptions,
  onOwnedKeys,
}: {
  hostId: string | undefined;
  knobs: ConfigKnob[];
  renderNodeOptions: RenderNodeOption[];
  /** The keys this panel owns, so the legacy editor hides them. */
  onOwnedKeys: (keys: Set<string>) => void;
}) {
  const policy = useResource<adminApi.HostPolicyView | null>(
    {
      label: "next-session settings",
      pollMs: 5000,
      fetch: async (ctx) => {
        try {
          return await adminApi.getHostPolicy(ctx.token, hostId ?? "");
        } catch (e) {
          // A control plane without typed policy: the legacy editor keeps every key.
          if (e instanceof ApiError && e.status === 404) return null;
          throw e;
        }
      },
    },
    [hostId],
  );
  const view = policy.data ?? null;
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});
  const [saving, setSaving] = useState(false);
  const [retrying, setRetrying] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const owned = useMemo(
    () => (view ? knobs.filter((k) => typedOwned(view.groups[k.key])) : []),
    [view, knobs],
  );
  // Saved typed intent the agent cannot run yet: the legacy editor keeps the key,
  // and this panel shows only the remedy.
  const awaitingUpgrade = view
    ? knobs.filter((k) => view.groups[k.key]?.scope === "next_session" && view.groups[k.key]?.status === "upgrade_required")
    : [];
  const loaded = policy.data !== undefined;
  const ownedKey = owned.map((k) => k.key).join(",");
  useEffect(() => {
    // Report only once ownership is known; until then the caller hides every
    // candidate key rather than expose a legacy control the typed writer owns.
    if (loaded) onOwnedKeys(new Set(ownedKey ? ownedKey.split(",") : []));
  }, [loaded, ownedKey, onOwnedKeys]);

  if (!hostId || !view || owned.length + awaitingUpgrade.length === 0) return null;

  const saved = (key: string): Draft => {
    const choice = view.choices[key];
    return choice?.source === "explicit"
      ? { source: "explicit", value: choice.value as SettingValue }
      : { source: "deployment" };
  };
  const draftFor = (key: string) => drafts[key] ?? saved(key);
  const changes = Object.fromEntries(
    owned.filter((k) => !sameChoice(draftFor(k.key), saved(k.key))).map((k) => {
      const d = draftFor(k.key);
      return [k.key, d.source === "explicit" ? { source: d.source, value: d.value } : { source: d.source }];
    }),
  );
  const dirty = Object.keys(changes).length > 0;

  const edit = (knob: ConfigKnob, next: Partial<Draft>) => {
    setMessage(null);
    setDrafts((all) => {
      const current = all[knob.key] ?? saved(knob.key);
      const merged = { ...current, ...next };
      if (merged.source === "explicit" && merged.value === undefined) {
        merged.value = (view.resolved[knob.key]?.value ?? knob.default) as SettingValue;
      }
      return { ...all, [knob.key]: merged };
    });
  };

  const save = async () => {
    if (!dirty) return;
    setSaving(true); setError(null); setMessage(null);
    try {
      await policy.mutate((ctx) => adminApi.updateHostPolicy(ctx.token, hostId, view.revision, changes), (_old, result) => result);
      setDrafts({});
      setMessage("Saved. Each setting reports its own application below; new sessions pick it up once the host confirms it.");
    } catch (e) {
      if (e instanceof ApiError && e.code === "stale_revision") {
        setError("These settings changed elsewhere. Review the current values and save again.");
        void policy.refresh({ silent: true });
      } else setError(e instanceof ApiError ? e.message : "Could not save next-session settings.");
    } finally { setSaving(false); }
  };

  const retry = async (key: string) => {
    setRetrying(key); setError(null); setMessage(null);
    try {
      await policy.mutate((ctx) => adminApi.retryHostPolicyGroup(ctx.token, hostId, key), (_old, result) => result);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        setError("Retry is not available on this control plane yet. Reconnect the host or save the setting again.");
      } else setError(e instanceof ApiError ? e.message : "Could not retry this setting.");
    } finally { setRetrying(null); }
  };

  return <KnobPanel
    title="Next-session settings"
    hint="Saved as one edit. Each setting applies to new sessions once the host confirms it; running sessions keep their launch values."
    actions={owned.length > 0 && <Button variant="primary" disabled={!dirty || saving} onClick={() => void save()}>{saving ? "Saving…" : "Save next-session settings"}</Button>}
  >
    {message && <p role="status" className="hint">{message}</p>}
    {error && <p role="alert" className="form-error">{error}</p>}
    {awaitingUpgrade.map((knob) => <div key={knob.key} className="cset">
      <div>
        <h3 className="row gap2 center">
          {knobLabel(knob)}
          <Chip variant="warning" className="chip-sm">upgrade required</Chip>
        </h3>
        {view.groups[knob.key].remedy && <p className="hint">{remedyText(view.groups[knob.key].remedy ?? "")}</p>}
      </div>
      <div><p className="hint">Edit this setting in the panels below until the agent is upgraded.</p></div>
    </div>)}
    {owned.map((knob) => {
      const label = knobLabel(knob);
      const group = view.groups[knob.key];
      const resolved = view.resolved[knob.key];
      const draft = draftFor(knob.key);
      const verified = group.status === "applied" && group.fresh;
      return <div key={knob.key} className="cset" role="group" aria-label={label}>
        <div>
          <h3 className="row gap2 center">
            {label}
            <Chip variant={STATUS_CHIP[group.status] ?? "neutral"} className="chip-sm">{group.status.replaceAll("_", " ")}</Chip>
          </h3>
          <p className="hint">{knobHelp(knob)}</p>
          <p className="hint" style={{ marginTop: 4 }}>
            {`${resolved?.source === "explicit" ? "Explicit value" : "Deployment setting"} ${valueLabel((resolved?.value ?? undefined) as SettingValue | undefined)} · `}
            {verified
              ? `Verified on the current connection${group.observed_at ? ` at ${new Date(group.observed_at).toLocaleString()}` : ""}`
              : "Not yet verified"}
          </p>
          <p className="hint">
            Desired revision {group.desired_revision} · applied revision {group.applied_revision ?? "none"} · {group.scope.replaceAll("_", " ")}
          </p>
          {group.remedy && <p className="hint" style={{ marginTop: 4 }}>{remedyText(group.remedy)}</p>}
          {group.next_retry_at && <p className="hint">Next retry {new Date(group.next_retry_at).toLocaleTimeString()}</p>}
        </div>
        <div>
          <select
            className="select"
            style={{ width: 260 }}
            aria-label={`${label} source`}
            value={draft.source}
            onChange={(e) => edit(knob, { source: e.target.value as Draft["source"] })}
          >
            <option value="deployment">Deployment setting</option>
            <option value="explicit">Explicit value</option>
          </select>
          {draft.source === "explicit" && <div style={{ marginTop: 6 }}>
            <KnobControl
              knob={knob}
              value={draft.value}
              onChange={(value) => edit(knob, { value })}
              renderNodeOptions={renderNodeOptions}
              ariaLabel={`${label} value`}
            />
          </div>}
          {waitsForRetry(group) && <div style={{ marginTop: 6 }}>
            <Button disabled={retrying === knob.key} onClick={() => void retry(knob.key)}>Retry</Button>
          </div>}
        </div>
      </div>;
    })}
  </KnobPanel>;
}
