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

export function webTransportMode(wt: HostWebTransport): WebTransportMode {
  return wt.endpoint?.mode ?? (wt.override ? "edge" : "direct");
}

/** The agent reports every certificate it may present, current first, and two only
 *  during a rotation. A current certificate under 24 h from expiry with no successor
 *  reported means the agent's renewal did not happen. */
export function certificateFact(wt: HostWebTransport, now: number): CertificateFact {
  if (wt.override?.certificate === "public") {
    return { headline: "Managed by your proxy", sub: "Quasar does not see its expiry", warn: false, fingerprint: null };
  }
  const certs = wt.reported?.certificates ?? [];
  const current = certs[0];
  if (!current) {
    return { headline: "Supplied by you", sub: "Quasar does not see its expiry", warn: false, fingerprint: null };
  }
  const left = Date.parse(current.not_after) - now;
  const on = new Date(current.not_after).toLocaleString("en", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
  const fingerprint = current.sha256;
  if (left <= 0) {
    return { headline: "Expired", sub: `${on} · automatic renewal failed`, warn: true, expired: true, fingerprint };
  }
  const headline = `Expires ${relativeTimeFuture(current.not_after, now)}`;
  if (left < RENEWAL_DEADLINE_MS && certs.length < 2) {
    return { headline, sub: `${on} · automatic renewal failed`, warn: true, fingerprint };
  }
  return { headline, sub: `${on} · replaced automatically before it expires`, warn: false, fingerprint };
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
