// Create/edit drawer for one stream profile (encode rung) — v3 handoff §A.17.
// The `id` field is a contract-forced deviation from the mock:
// StreamProfileWrite.id is required on POST (openapi.yaml — rung ids are
// operator-chosen text) and the handler 400s without one.

import { useEffect, useState } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { CatalogCodec, StreamProfile, StreamProfileWrite } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { Drawer } from "../../../components/Drawer";
import { SelectField, Switch, TextField } from "../../../components/TextField";
import { deleteDisabledTitle, isStreamProfileInUse } from "./streamProfileHelpers";

interface StreamProfileDrawerProps {
  /** null = creating a new rung (an empty drawer). */
  profile: StreamProfile | null;
  token: string;
  onClose: () => void;
  onSaved: (profile: StreamProfile) => void;
  /** Delete confirmation lives in the parent (shared with the card table row's
   *  own menu) — this just asks for it. */
  onRequestDelete: (profile: StreamProfile) => void;
}

// This drawer's own width — 640px, unrelated to the runtime preset drawer's
// `.drawer.wide` (760px, handoff §A.12).
const WIDE_DRAWER = 640;

const CODEC_OPTIONS: { value: CatalogCodec; label: string }[] = [
  { value: "av1", label: "AV1" },
  { value: "hevc", label: "H.265 / HEVC" },
  { value: "h264", label: "H.264" },
];

const BROWSER_OPTIONS: { value: StreamProfile["browser_client"]; label: string }[] = [
  { value: "recommended", label: "Recommended" },
  { value: "supported", label: "Supported" },
  { value: "risky", label: "Risky" },
];

export function StreamProfileDrawer({
  profile,
  token,
  onClose,
  onSaved,
  onRequestDelete,
}: StreamProfileDrawerProps) {
  const isNew = profile === null;

  const [id, setId] = useState(profile?.id ?? "");
  const [name, setName] = useState(profile?.display_name ?? "");
  const [codec, setCodec] = useState<CatalogCodec>(profile?.codec ?? "h264");
  const [width, setWidth] = useState(String(profile?.width ?? 1920));
  const [height, setHeight] = useState(String(profile?.height ?? 1080));
  const [fps, setFps] = useState(String(profile?.fps ?? 60));
  const [bitrate, setBitrate] = useState(String(profile?.nominal_bitrate_kbps ?? 8000));
  const [abrFloor, setAbrFloor] = useState(String(profile?.abr_floor_kbps ?? 3000));
  const [hwRequired, setHwRequired] = useState(profile?.hardware_encoder_required ?? false);
  const [browserClient, setBrowserClient] = useState<StreamProfile["browser_client"]>(
    profile?.browser_client ?? "supported",
  );

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [validationErrors, setValidationErrors] = useState<Record<string, string>>({});

  // Reset form when the target profile changes (switching which row is open).
  useEffect(() => {
    setId(profile?.id ?? "");
    setName(profile?.display_name ?? "");
    setCodec(profile?.codec ?? "h264");
    setWidth(String(profile?.width ?? 1920));
    setHeight(String(profile?.height ?? 1080));
    setFps(String(profile?.fps ?? 60));
    setBitrate(String(profile?.nominal_bitrate_kbps ?? 8000));
    setAbrFloor(String(profile?.abr_floor_kbps ?? 3000));
    setHwRequired(profile?.hardware_encoder_required ?? false);
    setBrowserClient(profile?.browser_client ?? "supported");
    setError(null);
    setValidationErrors({});
  }, [profile]);

  const hasError = (key: string) => validationErrors[key];

  const validate = (): boolean => {
    const errs: Record<string, string> = {};
    if (isNew && !id.trim()) errs.id = "Profile ID is required";
    if (isNew && id.trim() && !/^[a-z0-9][a-z0-9-]*$/.test(id.trim())) {
      errs.id = "Use lowercase letters, digits and hyphens (e.g. 1080p60-h264)";
    }
    if (!name.trim()) errs.name = "Name is required";
    for (const [key, val] of [
      ["width", width],
      ["height", height],
      ["fps", fps],
      ["bitrate", bitrate],
      ["abrFloor", abrFloor],
    ] as const) {
      if (Number.isNaN(parseInt(val, 10)) || parseInt(val, 10) < 1) {
        errs[key] = "Must be a positive integer";
      }
    }
    setValidationErrors(errs);
    return Object.keys(errs).length === 0;
  };

  const handleSubmit = async () => {
    setError(null);
    if (!validate()) return;

    const req: StreamProfileWrite = {
      display_name: name.trim(),
      codec,
      width: parseInt(width, 10),
      height: parseInt(height, 10),
      fps: parseInt(fps, 10),
      nominal_bitrate_kbps: parseInt(bitrate, 10),
      abr_floor_kbps: parseInt(abrFloor, 10),
      hardware_encoder_required: hwRequired,
      browser_client: browserClient,
    };
    if (isNew) req.id = id.trim();

    setSaving(true);
    try {
      const saved = isNew
        ? await adminApi.createStreamProfile(token, req)
        : await adminApi.updateStreamProfile(token, profile.id, req);
      onSaved(saved);
    } catch (e: unknown) {
      setError(e instanceof ApiError ? e.message : "Save failed.");
    } finally {
      setSaving(false);
    }
  };

  const usedBy = profile?.used_by ?? [];
  const sessionCount = profile?.session_count ?? 0;
  const inUse = !isNew && !!profile && isStreamProfileInUse(profile);

  return (
    <Drawer
      open
      onClose={onClose}
      title={isNew ? "New stream profile" : profile.display_name}
      eyebrow="stream profile · one encode rung"
      width={WIDE_DRAWER}
      footer={
        <>
          <Button
            variant="danger"
            disabled={isNew || inUse}
            title={inUse ? deleteDisabledTitle(sessionCount) : undefined}
            onClick={() => profile && onRequestDelete(profile)}
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
          <p>How this rung is listed when building a launch profile.</p>
        </div>
        <div className="fs-fields">
          {isNew && (
            <div>
              <TextField
                label="Profile ID"
                mono
                value={id}
                onChange={(e) => setId(e.target.value)}
                placeholder="1080p60-h264"
                hint="Operator-chosen, permanent once created."
                aria-invalid={!!hasError("id")}
              />
              {hasError("id") && <p className="apps-field-err">{validationErrors.id}</p>}
            </div>
          )}
          <div>
            <TextField
              label="Name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              aria-invalid={!!hasError("name")}
            />
            {hasError("name") && <p className="apps-field-err">{validationErrors.name}</p>}
          </div>
        </div>
      </div>

      {/* Encode */}
      <div className="fsec">
        <div className="fs-label">
          <h4>Encode</h4>
          <p>One codec at one resolution and frame rate.</p>
        </div>
        <div className="fs-fields">
          <SelectField
            label="Codec"
            value={codec}
            onChange={(e) => setCodec(e.target.value as CatalogCodec)}
            hint="A rung is a single codec. Fallback between codecs is the launch profile's job."
          >
            {CODEC_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </SelectField>
          <div className="apps-frow apps-frow-3">
            <div>
              <TextField
                label="Width"
                type="number"
                mono
                value={width}
                onChange={(e) => setWidth(e.target.value)}
                aria-invalid={!!hasError("width")}
              />
              {hasError("width") && <p className="apps-field-err">{validationErrors.width}</p>}
            </div>
            <div>
              <TextField
                label="Height"
                type="number"
                mono
                value={height}
                onChange={(e) => setHeight(e.target.value)}
                aria-invalid={!!hasError("height")}
              />
              {hasError("height") && <p className="apps-field-err">{validationErrors.height}</p>}
            </div>
            <div>
              <TextField
                label="FPS"
                type="number"
                mono
                value={fps}
                onChange={(e) => setFps(e.target.value)}
                aria-invalid={!!hasError("fps")}
              />
              {hasError("fps") && <p className="apps-field-err">{validationErrors.fps}</p>}
            </div>
          </div>
          <div className="apps-frow">
            <div>
              <TextField
                label="Bitrate (kbps)"
                type="number"
                mono
                value={bitrate}
                onChange={(e) => setBitrate(e.target.value)}
                hint="Codec-specific. A newer codec needs less for the same picture."
                aria-invalid={!!hasError("bitrate")}
              />
              {hasError("bitrate") && <p className="apps-field-err">{validationErrors.bitrate}</p>}
            </div>
            <div>
              <TextField
                label="ABR floor (kbps)"
                type="number"
                mono
                value={abrFloor}
                onChange={(e) => setAbrFloor(e.target.value)}
                hint="Adaptive bitrate will not drop below this."
                aria-invalid={!!hasError("abrFloor")}
              />
              {hasError("abrFloor") && <p className="apps-field-err">{validationErrors.abrFloor}</p>}
            </div>
          </div>
        </div>
      </div>

      {/* Requirements */}
      <div className="fsec">
        <div className="fs-label">
          <h4>Requirements</h4>
          <p>What a host and client need before this rung can be used.</p>
        </div>
        <div className="fs-fields">
          <Switch
            checked={hwRequired}
            onChange={setHwRequired}
            label="Requires a hardware encoder"
            id="sp-hw-required"
          />
          <SelectField
            label="Browser support"
            value={browserClient}
            onChange={(e) =>
              setBrowserClient(e.target.value as StreamProfile["browser_client"])
            }
            hint="Advisory only. The client decode probe decides at launch."
          >
            {BROWSER_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </SelectField>
        </div>
      </div>

      {/* Used by */}
      <div className="fsec">
        <div className="fs-label">
          <h4>Used by</h4>
          <p>Launch profiles listing this rung, and the sessions that ran on it.</p>
        </div>
        <div className="fs-fields">
          <div className="used-by">
            {usedBy.length === 0 && sessionCount === 0 ? (
              <span className="muted">Not used by any launch profile yet.</span>
            ) : (
              <>
                {usedBy.map((lp) => (
                  <Chip key={lp.id} variant="accent">{lp.display_name}</Chip>
                ))}
                {sessionCount > 0 && (
                  <Chip variant="neutral">
                    {sessionCount} {sessionCount === 1 ? "session" : "sessions"}
                  </Chip>
                )}
              </>
            )}
          </div>
          <span className="hint">
            {sessionCount > 0
              ? "Past sessions recorded this rung as the one they resolved to, so it can no longer be deleted. That history is what makes a session's settings explainable."
              : "A rung in use cannot be deleted."}
          </span>
        </div>
      </div>

      {error && <p className="form-error" style={{ marginTop: "var(--s4)" }}>{error}</p>}
    </Drawer>
  );
}
