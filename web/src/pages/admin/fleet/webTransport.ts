/**
 * What the host WebTransport card says, derived from `GET /v1/admin/hosts/{id}/webtransport`
 * (control-api.md §WebTransport endpoint). Pure: the component only draws it.
 */

import type { HostWebTransport } from "../../../api/types";
import { relativeTimeFuture } from "../../../lib/format/relativeTime";

/** The agent starts presenting its next certificate at 24 h left
 *  (transport-webtransport.md §Certificates) and reports both from 72 h. */
const RENEWAL_DEADLINE_MS = 24 * 60 * 60 * 1000;

export type WebTransportMode = "direct" | "edge";

export interface CertificateFact {
  /** The row's value. */
  headline: string;
  /** The line under it. */
  sub: string;
  /** Renewal failed and the certificate is nearly out: amber, and the card warns. */
  warn: boolean;
  /** Already past `not_after`. */
  expired?: boolean;
  /** Base64 SHA-256 of the current self-signed certificate, when there is one to show. */
  fingerprint: string | null;
}

/** Edge only for a public URL on a publicly trusted certificate, which is how the control
 *  plane resolves it (`wtendpoint.Resolve`); an `agent` override is a direct endpoint. */
export function webTransportMode(wt: HostWebTransport): WebTransportMode {
  return wt.override?.certificate === "public" ? "edge" : "direct";
}

const UNKNOWN_EXPIRY: CertificateFact = { headline: "Expiry unknown", sub: "", warn: false, fingerprint: null };

/** The agent reports every certificate it may present, and two only during a rotation. The
 *  current one is the earliest that has not expired. Under 24 h from expiry with no successor
 *  reported means the agent's renewal did not happen; a host that is offline reports nothing
 *  new, so its last report proves nothing. */
export function certificateFact(wt: HostWebTransport, now: number): CertificateFact {
  if (wt.override?.certificate === "public") {
    return { headline: "Managed by your proxy", sub: "Quasar does not see its expiry", warn: false, fingerprint: null };
  }
  const certs = (wt.reported?.certificates ?? []).map((c) => ({ ...c, end: Date.parse(c.not_after) }));
  if (certs.length === 0) {
    return { headline: "Supplied by you", sub: "Quasar does not see its expiry", warn: false, fingerprint: null };
  }
  const dated = certs.filter((c) => Number.isFinite(c.end));
  if (dated.length === 0) return { ...UNKNOWN_EXPIRY, fingerprint: certs[0].sha256 };
  const live = dated.filter((c) => c.end > now).sort((x, y) => x.end - y.end);
  const failed = wt.reason !== "host_offline";
  if (live.length === 0) {
    const last = dated.reduce((x, y) => (y.end > x.end ? y : x));
    const on = clock(last.end);
    return { headline: "Expired", sub: failed ? `${on} · automatic renewal failed` : on, warn: failed, expired: true, fingerprint: last.sha256 };
  }
  const current = live[0];
  const on = clock(current.end);
  const headline = `Expires ${relativeTimeFuture(current.end, now)}`;
  if (current.end - now < RENEWAL_DEADLINE_MS && live.length < 2) {
    return { headline, sub: failed ? `${on} · automatic renewal failed` : on, warn: failed, fingerprint: current.sha256 };
  }
  return { headline, sub: `${on} · replaced automatically before it expires`, warn: false, fingerprint: current.sha256 };
}

function clock(ms: number): string {
  return new Date(ms).toLocaleString("en", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
}

/** The reasons `usable: false` carries (an open string). */
export function unusableReason(reason: string | null): string {
  switch (reason) {
    case "host_offline":
      return "The host is offline, so new sessions on it use WebRTC.";
    case "no_address":
      return "The agent reported no address and there is no public URL. Set one below.";
    case "certificate_expired":
      return "The certificate has expired, so new sessions on this host use WebRTC.";
    default:
      return "WebTransport is not usable on this host, so its sessions use WebRTC.";
  }
}
