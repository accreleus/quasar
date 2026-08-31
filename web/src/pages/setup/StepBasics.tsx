// Wizard step 2 — instance basics. Writes only registration_mode (PATCH
// /v1/admin/settings, LP-SEC-01); "Public URL" is a live display of
// window.location.origin and the TLS note is static copy — neither is stored.

import { useEffect, useState, type FormEvent } from "react";
import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import { useAuth } from "../../auth/context";
import type { RegistrationMode } from "../../api/types";
import { Button } from "../../components/Button";
import { SegmentedControl } from "../../components/SegmentedControl";
import { Field } from "../../components/TextField";

interface StepBasicsProps {
  onNext: () => void;
}

const REGISTRATION_OPTIONS: { value: RegistrationMode; label: string; hint: string }[] = [
  { value: "closed", label: "Closed", hint: "No new accounts. Admins provision users directly." },
  { value: "invite_only", label: "Invite only", hint: "New accounts require an admin-minted invite link." },
  { value: "open", label: "Open", hint: "Anyone who can reach this instance can register." },
];

export function StepBasics({ onNext }: StepBasicsProps) {
  const { token } = useAuth();
  const [mode, setMode] = useState<RegistrationMode | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loadAttempt, setLoadAttempt] = useState(0);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (!token) return;
    let cancelled = false;
    adminApi
      .getSettings(token)
      .then(({ settings }) => {
        if (cancelled) return;
        setLoadError(null);
        setMode(settings.registration_mode);
      })
      .catch((err) => {
        if (cancelled) return;
        // A value never loaded must never be submitted back: on a failed read
        // `mode` stays null (Continue disabled). A "sane default" here once
        // silently overwrote a real instance setting on a transient GET failure.
        setLoadError(err instanceof ApiError ? err.message : "Could not load current settings.");
      });
    return () => {
      cancelled = true;
    };
  }, [token, loadAttempt]);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (!token || !mode) return;
    setSaveError(null);
    setSaving(true);
    try {
      await adminApi.updateSettings(token, { registration_mode: mode });
      onNext();
    } catch (err) {
      setSaveError(err instanceof ApiError ? err.message : "Could not save settings.");
      setSaving(false);
    }
  }

  const publicUrl = typeof window !== "undefined" ? window.location.origin : "";
  const isHttps = publicUrl.startsWith("https://");

  return (
    <form
      className="card login-card"
      onSubmit={onSubmit}
      noValidate
      style={{ width: "100%", maxWidth: 560, display: "flex", flexDirection: "column", gap: "var(--s5)" }}
    >
      <div>
        <h2 style={{ margin: 0 }}>Instance basics</h2>
        <p className="sub" style={{ marginTop: 6 }}>
          A couple of settings before this instance is ready for other people.
        </p>
      </div>

      <Field label="Public URL" hint="Where users and hosts reach this control plane. Read-only here — set it at the reverse proxy / DNS layer.">
        <input className="input mono" value={publicUrl} readOnly disabled />
      </Field>

      <div className="field">
        <span className="label">TLS / network posture</span>
        <p className="field-hint" style={{ margin: 0 }}>
          {isHttps
            ? "Served over HTTPS. Media (WebRTC) is still LAN/VPN-only in this release — there is no STUN/TURN yet, so a session only connects when the client can reach a host directly."
            : "Served over plain HTTP. Media (WebRTC) is LAN/VPN-only in this release regardless — there is no STUN/TURN yet, so remote access needs a VPN either way."}
        </p>
      </div>

      {loadError && (
        <div
          className="login-error"
          role="alert"
          style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "var(--s3)" }}
        >
          <span>
            {loadError} The current registration mode is unknown — retry, or
            pick one below to set it deliberately.
          </span>
          <Button type="button" variant="ghost" size="sm" onClick={() => setLoadAttempt((n) => n + 1)}>
            Retry
          </Button>
        </div>
      )}

      <div className="field">
        <span className="label">Registration mode</span>
        {mode === null && !loadError && <span className="field-hint">Loading current settings…</span>}
        {(mode !== null || loadError) && (
          <SegmentedControl
            aria-label="Registration mode"
            value={mode}
            onChange={setMode}
            options={REGISTRATION_OPTIONS.map((o) => ({ value: o.value, label: o.label }))}
          />
        )}
        <span className="field-hint">
          {REGISTRATION_OPTIONS.find((o) => o.value === mode)?.hint}
        </span>
      </div>

      {saveError && (
        <p className="login-error" role="alert">
          {saveError}
        </p>
      )}

      <Button type="submit" variant="primary" size="lg" disabled={!mode || saving}>
        {saving ? "Saving…" : "Continue"}
      </Button>
    </form>
  );
}
