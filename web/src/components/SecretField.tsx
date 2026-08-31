// SecretField — the reusable admin control for one encrypted operator secret,
// driven entirely by the server's own declaration: a future secret needs a Go
// Descriptor and nothing else. No mockup covers this surface; composed from
// existing primitives, zero new CSS.
//
// Fully controlled — fetches nothing: props come from the parent's one
// GET /v1/admin/secrets read (per-instance fetching once made Settings N+1);
// refresh after set/clear is the parent's job via `onChange`.
//
// Write-only: the input starts empty every render — the server never sends the
// value; only configured/origin and a masked last-4 hint (nothing for values
// shorter than 12) are shown.

import { useState } from "react";
import * as adminApi from "../api/admin";
import { ApiError } from "../api/client";
import type { SecretStatus } from "../api/types";
import { Button } from "./Button";
import { Chip } from "./Chip";
import { TextField } from "./TextField";
import { useToast } from "./Toast";

interface SecretFieldProps {
  /** From the caller's secrets envelope; carries the name, so there is no
   *  separate `name` prop to disagree with it. */
  secret: SecretStatus;
  /** `master_key_configured` from that same envelope. Without one, nothing can
   *  be stored, so the field is disabled and says why. */
  masterKeyConfigured: boolean;
  token: string;
  /** Called after a successful set/clear. The only refresh path — this
   *  component holds no server state — so required, not optional. */
  onChange: () => void;
  /** Field label, defaulting to `secret.label`. A caller already showing the
   *  secret's name elsewhere (a row header) can pass a plain "API key". */
  label?: string;
  /** Omits the origin/stored/unreadable chip row, for a caller rendering its
   *  own status chip beside the secret's name. */
  hideStatus?: boolean;
}

/** How an `origin` reads to an operator. */
const ORIGIN_LABEL: Record<string, string> = {
  database: "In use — set here in the admin UI",
  environment: "In use — from this server's environment",
  none: "Not configured",
};

export function SecretField({
  secret,
  masterKeyConfigured,
  token,
  onChange,
  label,
  hideStatus = false,
}: SecretFieldProps) {
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [value, setValue] = useState("");

  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    try {
      await fn();
      // Clear first: the value is handed over; keep it out of the DOM.
      setValue("");
      setError(null);
      toast.addToast({ variant: "success", title: ok });
      onChange();
    } catch (e: unknown) {
      const msg = e instanceof ApiError ? e.message : "that did not work";
      setError(msg);
      toast.addToast({ variant: "danger", title: msg });
    } finally {
      setBusy(false);
    }
  };

  const origin = secret.origin;
  const stored = secret.configured;
  const unreadable = stored && !secret.readable;

  const effectiveLabel = label ?? secret.label;

  return (
    <div>
      {!hideStatus && (
        <div className="row gap3 center wrap">
          <Chip variant={origin === "none" ? "neutral" : "success"}>
            {ORIGIN_LABEL[origin] ?? origin}
          </Chip>
          {stored && secret.hint && (
            <span className="muted mono" style={{ fontSize: "var(--t-sm)" }}>
              stored key ends ····{secret.hint}
            </span>
          )}
          {stored && !secret.hint && <Chip variant="neutral">A value is stored</Chip>}
          {unreadable && <Chip variant="danger">Cannot be decrypted</Chip>}
        </div>
      )}

      {/* Both sources present: say which won and that the other remains. */}
      {origin === "database" && secret.env_set && secret.env_var && (
        <div className="note mt2">
          <div>
            <code>{secret.env_var}</code> is also set on this server, but the key stored here
            takes precedence. Clear the key below to fall back to the environment variable.
          </div>
        </div>
      )}

      {/* The two key-management failures, kept distinct. */}
      {unreadable && secret.problem && (
        <div className="note mt2">
          <div>{secret.problem}</div>
        </div>
      )}
      {!masterKeyConfigured && (
        <div className="note mt2">
          <div>
            No master key is configured on this control plane, so credentials cannot be stored
            here. Set <code>QUASAR_SECRET_KEY</code> to a base64 32-byte value
            (<code>openssl rand -base64 32</code>) and restart.
            {secret.env_var && (
              <>
                {" "}
                Until then, <code>{secret.env_var}</code> still works.
              </>
            )}{" "}
            Losing that key makes anything stored here unrecoverable — back it up with your
            deployment secrets.
          </div>
        </div>
      )}

      {error && <p className="apps-field-err">{error}</p>}

      <TextField
        label={effectiveLabel}
        aria-label={effectiveLabel}
        type="password"
        autoComplete="off"
        spellCheck={false}
        value={value}
        placeholder={stored ? "Enter a new value to replace the stored one" : "Paste the key"}
        disabled={busy || !masterKeyConfigured}
        onChange={(e) => setValue(e.target.value)}
        hint={secret.description}
        mono
      />
      <div className="row gap3 wrap mt2">
        <Button
          disabled={busy || !masterKeyConfigured || !value.trim()}
          onClick={() =>
            void run(
              () => adminApi.setSecret(token, secret.name, value.trim()),
              stored ? "Key replaced." : "Key saved — it takes effect immediately.",
            )
          }
        >
          {stored ? "Replace key" : "Save key"}
        </Button>
        {stored && (
          <Button
            variant="ghost"
            disabled={busy}
            onClick={() =>
              void run(
                () => adminApi.clearSecret(token, secret.name),
                secret.env_set
                  ? "Key cleared — falling back to the environment variable."
                  : "Key cleared.",
              )
            }
          >
            Clear key
          </Button>
        )}
        {secret.docs_url && (
          <a
            className="hint"
            href={secret.docs_url}
            target="_blank"
            rel="noreferrer noopener"
          >
            Where to get one
          </a>
        )}
      </div>
      <p className="hint mt2">
        Stored encrypted and never shown again — this server can use it, but cannot display it.
        {secret.env_var && (
          <>
            {" "}
            <code>{secret.env_var}</code> remains supported as a fallback.
          </>
        )}
      </p>
    </div>
  );
}
