// Create/edit drawer for a runtime preset — v3 handoff §A.12 (openPreset()).

import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { RuntimePreset, RuntimePresetWrite } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Drawer } from "../../../components/Drawer";
import { IconClose } from "../../../components/icons";
import { Switch, TextareaField, TextField } from "../../../components/TextField";
import { isPresetInUse } from "./presetHelpers";

interface RuntimePresetDrawerProps {
  /** null = creating a new preset (an empty drawer, per the mockup's `openPreset(null)`). */
  preset: RuntimePreset | null;
  token: string;
  onClose: () => void;
  onSaved: (preset: RuntimePreset) => void;
  /** Delete confirmation + the actual DELETE call live in the parent (shared
   *  with the table row's own menu) — this just asks for it. */
  onRequestDelete: (preset: RuntimePreset) => void;
}

// handoff §A.12: `.drawer.wide` is `min(760px, 94vw)`.
const WIDE_DRAWER = 760;

// Host networking is operator-only (QUASAR_CONTAINER_NETWORK), never
// preset-selectable — "host" collapses the sandbox onto the node-agent's own
// network namespace. This UI writes the tightened set even while
// `RuntimePreset["network"]` is still the wider generated type. No mock
// section covers this field (§A.12 lists Identity/Container/Storage/Used by
// only) — kept because it is real, load-bearing configuration (first-run-
// experience §S2) with nowhere else to live.
const NETWORK_OPTIONS = [
  { value: "", label: "Inherit host default" },
  { value: "none", label: "None" },
  { value: "bridge", label: "Bridge" },
] as const;

type SelectableNetwork = (typeof NETWORK_OPTIONS)[number]["value"];

/** Narrows a preset's stored `network` to the tightened, selectable set. A
 *  value outside it (only reachable today via `host`, which this UI can no
 *  longer write) falls back to "" (inherit) rather than rendering an
 *  unselectable option. */
function toSelectableNetwork(v: RuntimePreset["network"] | undefined): SelectableNetwork {
  return v === "none" || v === "bridge" ? v : "";
}

export function RuntimePresetDrawer({
  preset,
  token,
  onClose,
  onSaved,
  onRequestDelete,
}: RuntimePresetDrawerProps) {
  const isNew = preset === null;
  const navigate = useNavigate();

  const [name, setName] = useState(preset?.name ?? "");
  const [description, setDescription] = useState(preset?.description ?? "");
  const [image, setImage] = useState(preset?.image ?? "");
  const [args, setArgs] = useState<string[]>(preset?.args ?? []);
  const [envEntries, setEnvEntries] = useState<[string, string][]>(
    Object.entries(preset?.env ?? {}),
  );
  const [mounts, setMounts] = useState<string[]>(preset?.mounts ?? []);
  const [managedHome, setManagedHome] = useState(preset?.managed_home ?? false);
  const [containerPath, setContainerPath] = useState(
    preset?.home_container_path || "/home/quasar",
  );
  const [network, setNetwork] = useState<SelectableNetwork>(toSelectableNetwork(preset?.network));

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [validationErrors, setValidationErrors] = useState<Record<string, string>>({});

  // Reset form when the target preset changes (switching which row is open).
  useEffect(() => {
    setName(preset?.name ?? "");
    setDescription(preset?.description ?? "");
    setImage(preset?.image ?? "");
    setArgs(preset?.args ?? []);
    setEnvEntries(Object.entries(preset?.env ?? {}));
    setMounts(preset?.mounts ?? []);
    setManagedHome(preset?.managed_home ?? false);
    setContainerPath(preset?.home_container_path || "/home/quasar");
    setNetwork(toSelectableNetwork(preset?.network));
    setError(null);
    setValidationErrors({});
  }, [preset]);

  const hasError = (key: string) => validationErrors[key];

  const validate = (): boolean => {
    const errs: Record<string, string> = {};
    if (!name.trim()) errs.name = "Name is required";

    envEntries.forEach(([k], i) => {
      if (!k.trim()) {
        errs[`env_key_${i}`] = "Key cannot be empty";
      } else if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(k.trim())) {
        errs[`env_key_${i}`] =
          "Invalid key (use letters, digits, underscores; start with letter or _)";
      }
    });
    args.forEach((a, i) => {
      if (!a.trim()) errs[`arg_${i}`] = "Argument cannot be empty";
    });
    mounts.forEach((m, i) => {
      if (!m.trim()) errs[`mount_${i}`] = "Mount cannot be empty";
    });
    if (managedHome && !containerPath.trim().startsWith("/")) {
      errs.containerPath = "Must be an absolute path";
    }

    setValidationErrors(errs);
    return Object.keys(errs).length === 0;
  };

  // ── List helpers ─────────────────────────────────────────────────────────
  const addArg = () => setArgs((prev) => [...prev, ""]);
  const updateArg = (i: number, val: string) =>
    setArgs((prev) => prev.map((a, idx) => (idx === i ? val : a)));
  const removeArg = (i: number) => setArgs((prev) => prev.filter((_, idx) => idx !== i));

  const addEnv = () => setEnvEntries((prev) => [...prev, ["", ""]]);
  const updateEnvKey = (i: number, k: string) =>
    setEnvEntries((prev) => prev.map((e, idx) => (idx === i ? [k, e[1]] : e)));
  const updateEnvVal = (i: number, v: string) =>
    setEnvEntries((prev) => prev.map((e, idx) => (idx === i ? [e[0], v] : e)));
  const removeEnv = (i: number) => setEnvEntries((prev) => prev.filter((_, idx) => idx !== i));

  const addMount = () => setMounts((prev) => [...prev, ""]);
  const updateMount = (i: number, val: string) =>
    setMounts((prev) => prev.map((m, idx) => (idx === i ? val : m)));
  const removeMount = (i: number) => setMounts((prev) => prev.filter((_, idx) => idx !== i));

  const handleSubmit = async () => {
    setError(null);
    if (!validate()) return;

    const req: RuntimePresetWrite = {
      name: name.trim(),
      description,
      image: image.trim(),
      args: args.map((a) => a.trim()).filter(Boolean),
      env: Object.fromEntries(envEntries.map(([k, v]) => [k.trim(), v])),
      mounts: mounts.map((m) => m.trim()).filter(Boolean),
      managed_home: managedHome,
      home_container_path: containerPath,
      network,
    };

    setSaving(true);
    try {
      const { runtime_preset } = isNew
        ? await adminApi.createRuntimePreset(token, req)
        : await adminApi.updateRuntimePreset(token, preset.id, req);
      onSaved(runtime_preset);
    } catch (e: unknown) {
      setError(e instanceof ApiError ? e.message : "Save failed.");
    } finally {
      setSaving(false);
    }
  };

  const usedBy = preset?.used_by ?? [];
  const inUse = !isNew && isPresetInUse(usedBy);

  return (
    <Drawer
      open
      onClose={onClose}
      title={isNew ? "New preset" : preset.name}
      eyebrow="runtime preset"
      width={WIDE_DRAWER}
      footer={
        <>
          <Button
            variant="danger"
            disabled={isNew || inUse}
            title={inUse ? "In use. Remove it from every app first." : undefined}
            onClick={() => preset && onRequestDelete(preset)}
          >
            Delete
          </Button>
          <span className="grow" />
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={() => void handleSubmit()} disabled={saving}>
            {saving ? "Saving…" : "Save changes"}
          </Button>
        </>
      }
    >
      {/* Identity */}
      <div className="fsec">
        <div className="fs-label">
          <h4>Identity</h4>
          <p>How the preset appears when picking one on an app.</p>
        </div>
        <div className="fs-fields">
          <div>
            <TextField
              label="Name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              aria-invalid={!!hasError("name")}
            />
            {hasError("name") && <p className="apps-field-err">{validationErrors.name}</p>}
          </div>
          <TextareaField
            label="Description"
            rows={2}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
      </div>

      {/* Container */}
      <div className="fsec">
        <div className="fs-label">
          <h4>Container</h4>
          <p>Image, arguments and environment every app using this preset starts from.</p>
        </div>
        <div className="fs-fields">
          <TextField
            label="Image"
            mono
            value={image}
            onChange={(e) => setImage(e.target.value)}
            placeholder="quasar-agent-dev:latest"
          />

          {/* First-run-experience §S2 — network is a per-app requirement
              carried by the preset, not a host env var. "" (inherit) is the
              default: the app keeps whatever the agent's host default is
              (QUASAR_CONTAINER_NETWORK, else none). An app's own
              runtime_spec.network still overrides this at launch.
              No "Host" option: the security round (Alice PR #464 round 2)
              ruled host networking operator-only — it collapses an app
              container onto the node-agent's own network namespace, which
              is a host-level decision (QUASAR_CONTAINER_NETWORK), never
              something a preset should be able to opt an app into. */}
          <div className="field">
            <span className="label">Network</span>
            <select
              className="select"
              value={network}
              onChange={(e) => setNetwork(e.target.value as SelectableNetwork)}
              aria-label="Network"
            >
              {NETWORK_OPTIONS.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>
            <span className="hint">
              Per-app network requirement. Steam needs bridge for its first-boot self-update;
              most other apps are fine on the hardened "none" default. Host networking is a
              host-level operator setting, not selectable here.
            </span>
          </div>

          {/* Launch arguments */}
          <div className="field">
            <span className="label">Launch arguments</span>
            <div className="kv">
              {args.map((arg, i) => (
                <div key={i} className="kv-row">
                  <input
                    className={["input", "mono", hasError(`arg_${i}`) ? "input-error" : ""]
                      .filter(Boolean)
                      .join(" ")}
                    value={arg}
                    onChange={(e) => updateArg(i, e.target.value)}
                    placeholder="--headless"
                    aria-label={`Argument ${i + 1}`}
                  />
                  <button
                    className="kv-del"
                    aria-label="Remove argument"
                    onClick={() => removeArg(i)}
                    type="button"
                  >
                    <IconClose className="" width={14} height={14} />
                  </button>
                  {hasError(`arg_${i}`) && (
                    <p className="apps-field-err">{validationErrors[`arg_${i}`]}</p>
                  )}
                </div>
              ))}
              <Button variant="ghost" size="sm" onClick={addArg} type="button">
                + Add argument
              </Button>
              <span className="hint">Apps append their own after these.</span>
            </div>
          </div>

          {/* Environment variables */}
          <div className="field">
            <span className="label">Environment variables</span>
            <div className="kv">
              {envEntries.map(([k, v], i) => (
                <div key={i}>
                  <div className="kv-row">
                    <input
                      className={["input", "mono", hasError(`env_key_${i}`) ? "input-error" : ""]
                        .filter(Boolean)
                        .join(" ")}
                      value={k}
                      onChange={(e) => updateEnvKey(i, e.target.value)}
                      placeholder="KEY"
                      aria-label={`Env key ${i + 1}`}
                      style={{ flex: 1 }}
                    />
                    <input
                      className="input mono"
                      value={v}
                      onChange={(e) => updateEnvVal(i, e.target.value)}
                      placeholder="value"
                      aria-label={`Env value ${i + 1}`}
                      style={{ flex: 1.4 }}
                    />
                    <button
                      className="kv-del"
                      aria-label="Remove variable"
                      onClick={() => removeEnv(i)}
                      type="button"
                    >
                      <IconClose className="" width={14} height={14} />
                    </button>
                  </div>
                  {hasError(`env_key_${i}`) && (
                    <p className="apps-field-err">{validationErrors[`env_key_${i}`]}</p>
                  )}
                </div>
              ))}
              <Button variant="ghost" size="sm" onClick={addEnv} type="button">
                + Add variable
              </Button>
              <span className="hint">An app setting the same key overrides the value here.</span>
            </div>
          </div>
        </div>
      </div>

      {/* Storage */}
      <div className="fsec">
        <div className="fs-label">
          <h4>Storage</h4>
          <p>Defaults for apps using this preset. An app can override them.</p>
        </div>
        <div className="fs-fields">
          <Switch checked={managedHome} onChange={setManagedHome} label="Managed home" id="preset-managed-home" />
          {managedHome && (
            <div>
              <TextField
                label="Mount path inside container"
                mono
                value={containerPath}
                onChange={(e) => setContainerPath(e.target.value)}
                placeholder="/home/quasar"
                aria-invalid={!!hasError("containerPath")}
              />
              {hasError("containerPath") && (
                <p className="apps-field-err">{validationErrors.containerPath}</p>
              )}
            </div>
          )}

          {/* Mounts */}
          <div className="field">
            <span className="label">Mounts</span>
            <div className="kv">
              {mounts.map((mount, i) => (
                <div key={i} className="kv-row">
                  <input
                    className={["input", "mono", hasError(`mount_${i}`) ? "input-error" : ""]
                      .filter(Boolean)
                      .join(" ")}
                    value={mount}
                    onChange={(e) => updateMount(i, e.target.value)}
                    placeholder="/host/path:/container/path"
                    aria-label={`Mount ${i + 1}`}
                  />
                  <button
                    className="kv-del"
                    aria-label="Remove mount"
                    onClick={() => removeMount(i)}
                    type="button"
                  >
                    <IconClose className="" width={14} height={14} />
                  </button>
                  {hasError(`mount_${i}`) && (
                    <p className="apps-field-err">{validationErrors[`mount_${i}`]}</p>
                  )}
                </div>
              ))}
              <Button variant="ghost" size="sm" onClick={addMount} type="button">
                + Add mount
              </Button>
              <span className="hint">
                Apps append their own. Two mounts on one container path is a misconfiguration, not a merge.
              </span>
            </div>
          </div>
        </div>
      </div>

      {/* Used by */}
      <div className="fsec">
        <div className="fs-label">
          <h4>Used by</h4>
          <p>Apps inheriting this preset. Editing it changes all of them.</p>
        </div>
        <div className="fs-fields">
          <div className="used-by">
            {usedBy.length === 0 ? (
              <span className="hint">Not used by any app yet.</span>
            ) : (
              usedBy.map((a) => (
                <button
                  key={a.id}
                  type="button"
                  className="chip chip-accent"
                  style={{ cursor: "pointer", border: 0 }}
                  onClick={() => {
                    onClose();
                    navigate(`/admin/library/apps/${a.id}`);
                  }}
                >
                  {a.name}
                </button>
              ))
            )}
          </div>
          <span className="hint">A preset in use cannot be deleted.</span>
        </div>
      </div>

      {error && <p className="form-error" style={{ marginTop: "var(--s4)" }}>{error}</p>}
    </Drawer>
  );
}
