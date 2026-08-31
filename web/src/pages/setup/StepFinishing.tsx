// Wizard step 5 — "Finishing touches" (wizard v2 §S1/S2/S3/S7), the wizard's
// actual finish point. No mockup covers it; matches the other step cards.
//
// S1 artwork key: one GET /v1/admin/secrets envelope, the matching row handed
// to SecretField. Must be skippable — an air-gapped install has to finish, so
// nothing gates on this field.
//
// S2 first library scan on Finish: best-effort, never blocking. Two lifecycle
// rules that defeated S2 when broken (Alice PR #480):
//   1. Discovery must be positively `=== true` — `settings` is null both
//      mid-load and after failure; the Finish button is disabled only during
//      the FIRST load, and a load failure still unblocks it (fail open — an
//      operator must never be trapped because one GET failed).
//   2. The scan races SCAN_TIMEOUT_MS; a timeout lands in the same best-effort
//      failure path as a thrown error, so a stalled request can't hold Finish.
// `inert_reason` renders verbatim (already operator language).
//
// S3 .env backup warning: copy only — losing QUASAR_SECRET_KEY makes every
// stored secret permanently unreadable.
//
// S7 mic toggle (#474), also on Admin → Settings; copy kept consistent with
// webrtc/mic.ts `microphoneSupported`/`unsupportedMicError`.

import { useEffect, useRef, useState } from "react";
import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import { useAuth } from "../../auth/context";
import { ARTWORK_API_KEY_SECRET, type InstanceSettings, type SecretsResponse } from "../../api/types";
import { Button } from "../../components/Button";
import { SecretField } from "../../components/SecretField";
import { Switch } from "../../components/TextField";
import { reportBestEffortFailure } from "../../lib/reportBestEffortFailure";

interface StepFinishingProps {
  onFinish: () => void;
}

type Phase = "form" | "finishing" | "done";

/** Long enough that a real scan enqueue never trips it, short enough that a
 *  stalled request can't hold Finish hostage. Exported for the timeout test. */
export const SCAN_TIMEOUT_MS = 15_000;

/** `fetch`'s abort rejection — a plain object with `name === "AbortError"`,
 *  not necessarily a `DOMException` in every test/runtime, so this checks
 *  duck-typed rather than with `instanceof`. */
function isAbortError(err: unknown): boolean {
  return typeof err === "object" && err !== null && (err as { name?: unknown }).name === "AbortError";
}

export function StepFinishing({ onFinish }: StepFinishingProps) {
  const { token } = useAuth();

  const [settings, setSettings] = useState<InstanceSettings | null>(null);
  const [settingsError, setSettingsError] = useState<string | null>(null);
  // Gates Finish; tracks only the FIRST load — a load failure must not keep
  // the button disabled (header rule 1).
  const [initialSettingsLoading, setInitialSettingsLoading] = useState(true);
  const [secretsEnv, setSecretsEnv] = useState<SecretsResponse | null>(null);
  const [secretsError, setSecretsError] = useState<string | null>(null);
  const [micBusy, setMicBusy] = useState(false);
  const [micError, setMicError] = useState<string | null>(null);
  const [phase, setPhase] = useState<Phase>("form");
  const [scanMessage, setScanMessage] = useState<string | null>(null);

  // In-flight scan controller, aborted on unmount.
  const scanControllerRef = useRef<AbortController | null>(null);

  function loadSecrets(signal?: AbortSignal) {
    if (!token) return;
    adminApi
      .listSecrets(token, signal)
      .then((env) => {
        setSecretsEnv(env);
        setSecretsError(null);
      })
      .catch((err: unknown) => {
        if (isAbortError(err)) return;
        setSecretsError(err instanceof ApiError ? err.message : "Could not load credentials.");
      });
  }

  useEffect(() => {
    if (!token) return;
    // One controller for both initial loads, aborted together on unmount so
    // neither handler sets state on a torn-down component.
    const controller = new AbortController();

    adminApi
      .getSettings(token, controller.signal)
      .then(({ settings: s }) => {
        setSettings(s);
        setSettingsError(null);
      })
      .catch((err: unknown) => {
        if (isAbortError(err)) return;
        setSettingsError(err instanceof ApiError ? err.message : "Could not load instance settings.");
      })
      .finally(() => {
        if (!controller.signal.aborted) setInitialSettingsLoading(false);
      });

    loadSecrets(controller.signal);

    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token]);

  // Best-effort must not keep a request or abort timer past this step's lifetime.
  useEffect(() => {
    return () => {
      scanControllerRef.current?.abort();
    };
  }, []);

  async function toggleMic(next: boolean) {
    if (!token) return;
    setMicBusy(true);
    try {
      const { settings: s } = await adminApi.updateSettings(token, { mic_capture_enabled: next });
      setSettings(s);
      setMicError(null);
    } catch (err) {
      setMicError(err instanceof ApiError ? err.message : "Could not update the microphone setting.");
    } finally {
      setMicBusy(false);
    }
  }

  async function handleFinish() {
    // Discovery must be positively on (header rule 1); the button's own
    // `disabled` covers the loading window, this covers every other path.
    if (!token || settings?.library_discovery_enabled !== true) {
      onFinish();
      return;
    }
    setPhase("finishing");

    const controller = new AbortController();
    scanControllerRef.current = controller;
    const timeout = window.setTimeout(() => controller.abort(), SCAN_TIMEOUT_MS);

    try {
      const result = await adminApi.forceLibraryScan(token, {}, controller.signal);
      setScanMessage(
        result.inert_reason || "A library scan has started — new games appear as soon as it finishes.",
      );
    } catch (err) {
      // Best-effort and bounded: error and timeout land in the same message
      // (header rule 2).
      reportBestEffortFailure("console-warn", "setup: POST /v1/admin/library/scan", err);
      setScanMessage(
        "Could not start the initial library scan automatically. It will still run on its regular " +
          "schedule, or you can trigger it now from Admin → Images.",
      );
    } finally {
      window.clearTimeout(timeout);
      scanControllerRef.current = null;
    }
    setPhase("done");
  }

  const artworkSecret = secretsEnv?.secrets.find((s) => s.name === ARTWORK_API_KEY_SECRET) ?? null;

  return (
    <div
      className="card login-card"
      style={{ width: "100%", maxWidth: 640, display: "flex", flexDirection: "column", gap: "var(--s5)" }}
    >
      <div>
        <h2 style={{ margin: 0 }}>Finishing touches</h2>
        <p className="sub" style={{ marginTop: 6 }}>
          Last step. Everything below is optional and can be changed later from{" "}
          <strong>Admin → Settings</strong>.
        </p>
      </div>

      <div>
        <h3 style={{ margin: "0 0 6px" }}>Cover artwork</h3>
        <p className="field-hint" style={{ margin: "0 0 12px" }}>
          Without a key here, apps show a plain gradient tile instead of cover art. You can skip
          this — it can be set later from an app's Artwork panel, or from{" "}
          <strong>Admin → Settings</strong>.
        </p>
        {secretsError && (
          <p className="login-error" role="alert">
            {secretsError}
          </p>
        )}
        {secretsEnv && artworkSecret && token && (
          <SecretField
            secret={artworkSecret}
            masterKeyConfigured={secretsEnv.master_key_configured}
            token={token}
            onChange={loadSecrets}
          />
        )}

        {/* S3 stays directly under the artwork key — below the mic toggle it
            read as a microphone warning (operator review). */}
        <div className="note warn" role="status" style={{ marginTop: "var(--s4)" }}>
          <div>
            <b>Back up deploy/.env now.</b> It holds <code>QUASAR_SECRET_KEY</code> — lose it and
            every credential stored here, including any artwork key you just entered, becomes
            permanently unreadable.
          </div>
        </div>
      </div>

      <div>
        <h3 style={{ margin: "0 0 6px" }}>Voice chat</h3>
        <p className="field-hint" style={{ margin: "0 0 12px" }}>
          Lets users talk into a game through the browser microphone. The page has to be a secure
          context (HTTPS, or localhost) or the browser will not expose a microphone at all,
          whatever this setting says. Chrome also negotiates OPUS at 48&nbsp;kHz stereo and refuses
          other combinations itself — see Admin → Settings if it does not seem to work later.
        </p>
        {settingsError && (
          <p className="login-error" role="alert">
            {settingsError}
          </p>
        )}
        {micError && (
          <p className="login-error" role="alert">
            {micError}
          </p>
        )}
        <Switch
          id="wizard-mic-capture"
          checked={settings?.mic_capture_enabled ?? false}
          onChange={(v) => void toggleMic(v)}
          label={settings?.mic_capture_enabled ? "Enabled" : "Enable microphone capture"}
          disabled={micBusy || !settings}
        />
      </div>

      {phase === "done" && scanMessage && (
        <p className="field-hint" role="status">
          {scanMessage}
        </p>
      )}

      <Button
        type="button"
        variant="primary"
        size="lg"
        // Disabled only during the FIRST settings load, never after a failure
        // (header rule 1) — this just stops a click racing that response.
        disabled={phase === "finishing" || (phase === "form" && initialSettingsLoading)}
        onClick={() => (phase === "done" ? onFinish() : void handleFinish())}
      >
        {phase === "finishing"
          ? "Finishing…"
          : phase === "done"
            ? "Continue to Quasar"
            : initialSettingsLoading
              ? "Loading…"
              : "Finish setup"}
      </Button>
    </div>
  );
}
