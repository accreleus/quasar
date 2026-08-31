// "New launch profile" drawer (v3 handoff §A.16). Creation only: existing
// profiles are edited inline on the Launch tab's cards (see LaunchTab.tsx); a
// brand-new profile has nowhere to inline-edit until it exists, so this
// collects Identity + the initial rung chain before the single POST.

import { useState } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { LaunchProfile, LaunchProfileWrite, StreamProfile } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Drawer } from "../../../components/Drawer";
import { TextareaField, TextField } from "../../../components/TextField";
import { moveRung, RungEditor } from "./RungEditor";

interface LaunchProfileDrawerProps {
  token: string;
  /** The full stream-profile catalog — candidates for the rung chain. */
  streamProfiles: StreamProfile[];
  onClose: () => void;
  onSaved: (profile: LaunchProfile) => void;
}

const WIDE_DRAWER = 640;

export function LaunchProfileDrawer({ token, streamProfiles, onClose, onSaved }: LaunchProfileDrawerProps) {
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [rungs, setRungs] = useState<StreamProfile[]>([]);

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [validationErrors, setValidationErrors] = useState<Record<string, string>>({});

  const availableToAdd = streamProfiles.filter((sp) => !rungs.some((r) => r.id === sp.id));

  const validate = (): boolean => {
    const errs: Record<string, string> = {};
    if (!id.trim()) errs.id = "Profile ID is required";
    else if (!/^[a-z0-9][a-z0-9-]*$/.test(id.trim())) {
      errs.id = "Use lowercase letters, digits and hyphens (e.g. balanced)";
    }
    if (!name.trim()) errs.name = "Name is required";
    if (rungs.length === 0) errs.rungs = "Add at least one rung";
    else if (!rungs.some((r) => r.codec === "h264")) {
      errs.rungs = "Must include at least one H.264 rung, the resolution floor";
    }
    setValidationErrors(errs);
    return Object.keys(errs).length === 0;
  };

  const handleSubmit = async () => {
    setError(null);
    if (!validate()) return;

    const req: LaunchProfileWrite = {
      id: id.trim(),
      display_name: name.trim(),
      description: description.trim(),
      rungs: rungs.map((r) => r.id),
    };

    setSaving(true);
    try {
      const saved = await adminApi.createLaunchProfile(token, req);
      onSaved(saved);
    } catch (e: unknown) {
      setError(e instanceof ApiError ? e.message : "Save failed.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title="New launch profile"
      eyebrow="launch profile · ordered chain of stream profiles"
      width={WIDE_DRAWER}
      footer={
        <>
          <span className="grow" />
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={() => void handleSubmit()} disabled={saving}>
            {saving ? "Creating…" : "Create launch profile"}
          </Button>
        </>
      }
    >
      <div className="fsec">
        <div className="fs-label">
          <h4>Identity</h4>
          <p>What a user sees when picking a quality.</p>
        </div>
        <div className="fs-fields">
          <div>
            <TextField
              label="Profile ID"
              mono
              value={id}
              onChange={(e) => setId(e.target.value)}
              placeholder="balanced"
              hint="Operator-chosen, permanent once created."
              aria-invalid={!!validationErrors.id}
            />
            {validationErrors.id && <p className="apps-field-err">{validationErrors.id}</p>}
          </div>
          <div>
            <TextField
              label="Name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              aria-invalid={!!validationErrors.name}
            />
            {validationErrors.name && <p className="apps-field-err">{validationErrors.name}</p>}
          </div>
          <TextareaField
            label="Description"
            rows={2}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
      </div>

      <div className="fsec">
        <div className="fs-label">
          <h4>Rungs</h4>
          <p>Order sets preference. The last rung must be H.264.</p>
        </div>
        <div className="fs-fields">
          <RungEditor
            rungs={rungs}
            availableToAdd={availableToAdd}
            disabled={saving}
            onMove={(from, to) => setRungs((prev) => moveRung(prev, from, to))}
            onRemove={(index) => setRungs((prev) => prev.filter((_, i) => i !== index))}
            onAdd={(profileId) => {
              const sp = streamProfiles.find((p) => p.id === profileId);
              if (sp) setRungs((prev) => [...prev, sp]);
            }}
          />
          {validationErrors.rungs && <p className="apps-field-err">{validationErrors.rungs}</p>}
        </div>
      </div>

      {error && <p className="form-error" style={{ marginTop: "var(--s4)" }}>{error}</p>}
    </Drawer>
  );
}
