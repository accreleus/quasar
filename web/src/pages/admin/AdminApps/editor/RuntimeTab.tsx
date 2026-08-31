// Runtime tab (handoff §A.10): image, arguments, environment, and the storage
// this app launches with. A derived tile has none of its own: the reconciler
// keeps its runtime_spec empty and the launch path merges the parent's, so the
// tab shows what it will run with rather than controls that write nowhere.
//
// Read mergeRuntimePreset (control-plane/internal/session/runtime_preset.go)
// before changing what layers over what.

import { Link } from "react-router-dom";
import type { AdminApp, RuntimePreset, StorageProvider } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { IconClose, IconPlus } from "../../../../components/icons";
import { SelectField, Switch, TextField } from "../../../../components/TextField";
import { imageFieldPlaceholder } from "../imagePlaceholder";
import { parseSpec } from "../runtimeSpec";
import { Fact, Section } from "./primitives";
import type { AppDraft, DraftErrors } from "./appDraft";

// The control is per host (Fleet › Hosts); this is the read-only explanation.
// "volume" is unreachable (#473, migration 0068) but stays because the frozen
// protocol/openapi.yaml enum still lists it and this Record is exhaustive.
const STORAGE_PROVIDER_HINT: Record<StorageProvider, string> = {
  auto: "host directories under each host's storage root",
  local: "host directories under each host's storage root",
  volume: "removed docker-volume driver (#473), treated as local",
};

interface RuntimeTabProps {
  draft: AppDraft;
  onChange: (draft: AppDraft) => void;
  errors: DraftErrors;
  app: AdminApp | null;
  parent: AdminApp | null;
  presets: RuntimePreset[];
  /** Instance-wide, read-only here. Null when the settings read failed. */
  storageProvider: StorageProvider | null;
}

export function RuntimeTab({
  draft,
  onChange,
  errors,
  app,
  parent,
  presets,
  storageProvider,
}: RuntimeTabProps) {
  if (parent && app) {
    const parentSpec = parseSpec(parent.runtime_spec);
    const parentPreset = presets.find((p) => p.id === parent.runtime_preset_id) ?? null;
    return (
      <Section title="Runtime" desc="Merged from the parent tile at launch.">
        <div className="note">
          <div>
            This tile is discovered under <strong>{parent.name}</strong> and contributes no runtime
            of its own. Image, arguments, environment and mounts all come from the parent. Edit{" "}
            <Link to={`/admin/library/apps/${parent.id}/runtime`}>{parent.name}</Link> to change
            what it launches with.
          </div>
        </div>
        <div className="ae-facts">
          <Fact label="Image">
            <span className="mono">{parentSpec.image || "inherited from the preset"}</span>
          </Fact>
          <Fact label="Runtime preset">
            {parentPreset ? (
              <Link to={`/admin/library/presets?preset=${parentPreset.id}`}>{parentPreset.name}</Link>
            ) : (
              <span className="muted">None</span>
            )}
          </Fact>
          <Fact label="Own runtime spec">
            <span className="mono">{JSON.stringify(app.runtime_spec ?? {})}</span>
          </Fact>
        </div>
      </Section>
    );
  }

  const preset = presets.find((p) => p.id === draft.runtimePresetId) ?? null;
  const presetEnvKeys = Object.keys(preset?.env ?? {});
  const overridden = draft.env.filter(([k]) => presetEnvKeys.includes(k.trim())).length;

  const setArgs = (args: string[]) => onChange({ ...draft, args });
  const setEnv = (env: [string, string][]) => onChange({ ...draft, env });
  const setMounts = (mounts: string[]) => onChange({ ...draft, mounts });

  return (
    <>
      <Section
        title="Runtime"
        desc="Image, arguments and environment. What you set here layers on top of the preset."
      >
        <SelectField
          label="Runtime preset"
          value={draft.runtimePresetId}
          onChange={(e) => onChange({ ...draft, runtimePresetId: e.target.value })}
          hint="Reusable image, environment and storage defaults, managed under Runtime presets."
        >
          <option value="">None, configure everything here</option>
          {presets.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </SelectField>
        {preset && (
          <div className="note">
            <div>
              Values below <strong>layer on top of the preset</strong>. Environment merges with the
              app winning on a shared key; mounts and launch arguments append.
            </div>
          </div>
        )}

        <div>
          <TextField
            label="Image"
            mono
            value={draft.image}
            onChange={(e) => onChange({ ...draft, image: e.target.value })}
            placeholder={imageFieldPlaceholder(!!draft.runtimePresetId)}
            hint={preset ? "Leave blank to use the preset image." : undefined}
            aria-invalid={!!errors.image}
          />
          {errors.image && <p className="form-error">{errors.image}</p>}
        </div>

        <div className="field">
          <span className="label">Launch arguments</span>
          <div className="kv">
            {draft.args.map((arg, i) => (
              <div key={i} style={{ width: "100%" }}>
                <div className="kv-row">
                  <input
                    className={`input mono${errors[`arg_${i}`] ? " input-error" : ""}`}
                    value={arg}
                    onChange={(e) => setArgs(draft.args.map((a, idx) => (idx === i ? e.target.value : a)))}
                    placeholder="--flag=value"
                    aria-label={`Argument ${i + 1}`}
                  />
                  <button
                    className="kv-del"
                    aria-label="Remove argument"
                    onClick={() => setArgs(draft.args.filter((_, idx) => idx !== i))}
                    type="button"
                  >
                    <IconClose width={14} height={14} />
                  </button>
                </div>
                {errors[`arg_${i}`] && <p className="form-error">{errors[`arg_${i}`]}</p>}
              </div>
            ))}
            <Button variant="ghost" size="sm" type="button" onClick={() => setArgs([...draft.args, ""])}>
              <IconPlus width={13} height={13} />
              Add argument
            </Button>
          </div>
        </div>

        <div className="field">
          <span className="label">Environment variables</span>
          {preset && (
            <span className="hint">
              Merged over the preset&rsquo;s. A key set here wins.{" "}
              {overridden} of the preset&rsquo;s {presetEnvKeys.length} keys overridden.
            </span>
          )}
          <div className="kv">
            {draft.env.map(([k, v], i) => (
              <div key={i} style={{ width: "100%" }}>
                <div className="kv-row">
                  <input
                    className={`input mono${errors[`env_key_${i}`] ? " input-error" : ""}`}
                    style={{ maxWidth: 200 }}
                    value={k}
                    onChange={(e) =>
                      setEnv(draft.env.map((entry, idx) => (idx === i ? [e.target.value, entry[1]] : entry)))
                    }
                    placeholder="KEY"
                    aria-label={`Env key ${i + 1}`}
                  />
                  <input
                    className="input mono"
                    value={v}
                    onChange={(e) =>
                      setEnv(draft.env.map((entry, idx) => (idx === i ? [entry[0], e.target.value] : entry)))
                    }
                    placeholder="value"
                    aria-label={`Env value ${i + 1}`}
                  />
                  <button
                    className="kv-del"
                    aria-label="Remove variable"
                    onClick={() => setEnv(draft.env.filter((_, idx) => idx !== i))}
                    type="button"
                  >
                    <IconClose width={14} height={14} />
                  </button>
                </div>
                {errors[`env_key_${i}`] && <p className="form-error">{errors[`env_key_${i}`]}</p>}
              </div>
            ))}
            <Button
              variant="ghost"
              size="sm"
              type="button"
              onClick={() => setEnv([...draft.env, ["", ""]])}
            >
              <IconPlus width={13} height={13} />
              Add variable
            </Button>
          </div>
        </div>
      </Section>

      <Section
        title="Storage"
        desc="A persistent home directory per user, plus anything extra this app needs mounted."
      >
        <Switch
          checked={draft.managedHome}
          onChange={(managedHome) => onChange({ ...draft, managedHome })}
          label="Managed home"
          id="managed-home"
        />
        <span className="hint">
          Provisions a per-user home mounted into the container. Data lives outside the container,
          under the storage root of whichever host runs the session.
        </span>
        {draft.managedHome && (
          <TextField
            label="Mount path inside container"
            mono
            value={draft.containerPath}
            onChange={(e) => onChange({ ...draft, containerPath: e.target.value })}
            placeholder="/home/quasar"
            hint="Where the home appears inside the container, not where the data is stored."
          />
        )}
        <div className="ae-facts">
          {storageProvider && (
            <Fact label="Storage backend">{STORAGE_PROVIDER_HINT[storageProvider]}</Fact>
          )}
          <Fact label="Set per host">
            <Link to="/admin/fleet/hosts">Fleet › Hosts</Link>
          </Fact>
        </div>

        <div className="field">
          <span className="label">Extra mounts</span>
          <div className="kv">
            {draft.mounts.map((mount, i) => (
              <div key={i} style={{ width: "100%" }}>
                <div className="kv-row">
                  <input
                    className={`input mono${errors[`mount_${i}`] ? " input-error" : ""}`}
                    value={mount}
                    onChange={(e) =>
                      setMounts(draft.mounts.map((m, idx) => (idx === i ? e.target.value : m)))
                    }
                    placeholder="/host/path:/container/path"
                    aria-label={`Mount ${i + 1}`}
                  />
                  <button
                    className="kv-del"
                    aria-label="Remove mount"
                    onClick={() => setMounts(draft.mounts.filter((_, idx) => idx !== i))}
                    type="button"
                  >
                    <IconClose width={14} height={14} />
                  </button>
                </div>
                {errors[`mount_${i}`] && <p className="form-error">{errors[`mount_${i}`]}</p>}
              </div>
            ))}
            <Button
              variant="ghost"
              size="sm"
              type="button"
              onClick={() => setMounts([...draft.mounts, ""])}
            >
              <IconPlus width={13} height={13} />
              Add mount
            </Button>
          </div>
          {preset && (
            <span className="hint">Appended to whatever the runtime preset already mounts.</span>
          )}
        </div>
      </Section>
    </>
  );
}
