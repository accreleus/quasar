/**
 * The host's "WebTransport" card (mock #1037, host detail): how browsers reach the
 * agent's WebTransport listener, the certificate, and the admin's public URL for an
 * edge proxy. Authorization is server-enforced (`admin(...)` on both routes); this
 * card is UX only (invariant #6).
 */

import { useState, type ReactNode } from "react";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { HostWebTransport } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { Chip, type ChipVariant } from "../../../../components/Chip";
import { useToast } from "../../../../components/Toast";
import { useResource } from "../../../../lib/resource/react";
import { certificateFact, unusableReason, webTransportMode } from "../webTransport";

const POLL_MS = 10_000;

export function WebTransportCard({ hostId, now }: { hostId: string; now: number }) {
  const { addToast } = useToast();
  const res = useResource<HostWebTransport>(
    { label: "WebTransport", pollMs: POLL_MS, fetch: (ctx) => adminApi.getHostWebTransport(ctx.token, hostId, ctx.signal) },
    [hostId],
  );
  const wt = res.data;
  const [draft, setDraft] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (!wt) {
    return res.errorMessage ? (
      <div className="card host-services">
        <div className="panel-head">
          <span className="panel-title">WebTransport</span>
        </div>
        <div className="host-services-note">
          <p className="note warn">{res.errorMessage}</p>
        </div>
      </div>
    ) : null;
  }

  const saved = wt.override?.public_url ?? "";
  const value = draft ?? saved;

  // Inline error rather than a toast (the server's message names what is wrong with the URL),
  // so this calls `mutate` directly instead of `useAdminAction`.
  const write = async (publicUrl: string | null) => {
    setSaving(true);
    setError(null);
    try {
      await res.mutate(
        (ctx) =>
          adminApi.setHostWebTransport(ctx.token, hostId, {
            public_url: publicUrl,
            certificate: wt.override?.certificate ?? "public",
          }),
        (_data, next) => next,
      );
      setDraft(null);
      addToast({ variant: "success", title: publicUrl ? "Public URL saved" : "Public URL cleared" });
    } catch (e: unknown) {
      setError(e instanceof ApiError ? e.message : "Could not save the public URL.");
    } finally {
      setSaving(false);
    }
  };

  const reported = wt.reported;
  const cert = certificateFact(wt, now);
  const mode = webTransportMode(wt);
  const chip: { variant: ChipVariant; label: string } = !reported
    ? { variant: "neutral", label: "not available" }
    : cert.warn
      ? { variant: "warning", label: "renewal failed" }
      : !wt.usable
        ? { variant: "danger", label: "unavailable" }
        : { variant: "success", label: mode === "edge" ? "edge proxy" : "direct" };

  return (
    <div className="card host-services host-webtransport">
      <div className="panel-head">
        <div>
          <span className="panel-title">WebTransport</span>
          <div className="hint host-services-hint">
            {reported
              ? mode === "edge"
                ? "How browsers reach this host’s WebTransport listener."
                : "How browsers reach this host’s WebTransport listener. Open the UDP port to your users."
              : "Versions and the WebTransport listener appear once this host’s agent reports."}
          </div>
        </div>
        <div className="acts">
          <Chip variant={chip.variant} dot>
            {chip.label}
          </Chip>
        </div>
      </div>

      {!reported && (
        <div className="host-services-note">
          <p className="note">
            This host’s agent does not offer WebTransport yet. Update it from Fleet ▸ Releases. Sessions on this
            host use WebRTC.
          </p>
        </div>
      )}
      {reported && cert.warn && (
        <div className="host-services-note">
          <p className="note warn">
            <b>Certificate renewal failed.</b>{" "}
            {cert.expired ? "The certificate has expired." : "The certificate expires in under 24 hours."} New
            WebTransport sessions on this host {cert.expired ? "fall back" : "will fall back"} to WebRTC
            {cert.expired ? "" : " once it expires"}.
          </p>
        </div>
      )}
      {reported && !wt.usable && !cert.warn && (
        <div className="host-services-note">
          <p className="note warn">{unusableReason(wt.reason)}</p>
        </div>
      )}

      {reported && (
        <div className="table-wrap host-facts">
          <table className="qtable">
            <tbody>
              <Fact
                label="Mode"
                value={mode === "edge" ? "Edge proxy" : "Direct"}
                sub={
                  mode === "edge"
                    ? "Browsers connect to your proxy, which forwards to the host"
                    : "Browsers connect to the host itself with a short-lived self-signed certificate"
                }
              />
              <Fact
                label="Detected address"
                mono
                value={
                  reported.addresses.length
                    ? `${reported.addresses[0]}${reported.addresses.length > 1 ? ` +${reported.addresses.length - 1}` : ""} · UDP ${reported.port}`
                    : `none reported · UDP ${reported.port}`
                }
                sub={
                  mode === "edge"
                    ? "Reported by the agent. Your proxy forwards to it. The port is read-only here; it is set by QUASAR_WT_PORT."
                    : "Reported by the agent. The port is read-only here; it is set by QUASAR_WT_PORT."
                }
              />
              <Fact label="Certificate" value={cert.headline} sub={cert.sub} warn={cert.warn} />
              {cert.fingerprint && (
                <Fact
                  label="Fingerprint"
                  mono
                  value={<span title={cert.fingerprint}>{`${cert.fingerprint.slice(0, 8)} … ${cert.fingerprint.slice(-4)}`}</span>}
                />
              )}
            </tbody>
          </table>
        </div>
      )}

      {(reported || wt.override) && (
        <div className="card-pad host-webtransport-url">
          <label className="label" htmlFor={`wt-url-${hostId}`}>
            Public URL
          </label>
          <div className="row gap3 host-webtransport-url-row">
            <input
              id={`wt-url-${hostId}`}
              className="input mono"
              value={value}
              placeholder="https://play.example.com/webtransport/host"
              maxLength={2048}
              disabled={saving}
              aria-invalid={error ? true : undefined}
              onChange={(e) => {
                setDraft(e.target.value);
                setError(null);
              }}
            />
            <Button disabled={saving || !value.trim() || value === saved} onClick={() => void write(value.trim())}>
              Save
            </Button>
            <Button disabled={saving || !wt.override} onClick={() => void write(null)}>
              Clear
            </Button>
          </div>
          {error && (
            <div className="apps-field-err" role="alert">
              {error}
            </div>
          )}
          <p className="hint">
            Set this when browsers reach this host through your reverse proxy. Leave it empty to use the detected
            address.
          </p>
        </div>
      )}
    </div>
  );
}

function Fact({
  label,
  value,
  sub,
  mono,
  warn,
}: {
  label: string;
  value: ReactNode;
  sub?: string;
  mono?: boolean;
  warn?: boolean;
}) {
  return (
    <tr>
      <td className="fact-key">{label}</td>
      <td className="right">
        <div className="stack">
          <span className={["primary", mono ? "mono" : "", warn ? "tone-warning" : ""].filter(Boolean).join(" ")}>
            {value}
          </span>
          {sub && <span className="sub">{sub}</span>}
        </div>
      </td>
    </tr>
  );
}
