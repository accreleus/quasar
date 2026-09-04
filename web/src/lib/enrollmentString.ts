/**
 * The one-paste enrollment string (#12): `qenr1.<FINGERPRINT>.<base64url(wss-url)>.<token>`.
 *
 * Composed HERE, in the admin console, never by the server: the server does not
 * know its own reachable address, while this page was reached at one. The
 * fingerprint comes first and verbatim (uppercase colon-separated SHA-256,
 * exactly as the control plane logs it) so the operator can compare it by eye
 * before pasting. An EMPTY fingerprint segment means "the certificate chains to
 * a real CA — verify it normally": pinning a Let's Encrypt leaf would break on
 * every renewal.
 *
 * The decoder lives in the node agent (`node-agent/src/enrollment.rs`); the
 * fixed vector in the tests here is shared with its tests so the two
 * implementations cannot drift apart silently.
 */

export const ENROLLMENT_PREFIX = "qenr1";

/** `AB:CD:…`, lowercase, bare hex or `sha256:`-prefixed → the control plane's
 *  uppercase colon form. `null` for anything that is not 32 bytes of hex. */
export function normalizeFingerprint(raw: string): string | null {
  const stripped = raw.trim().replace(/^sha256:/i, "");
  const hex = stripped.replace(/:/g, "").toUpperCase();
  if (!/^[0-9A-F]{64}$/.test(hex)) return null;
  return hex.match(/.{2}/g)!.join(":");
}

/** The websocket address the agent dials for the origin this page was opened
 *  at. `null` for a plain-http origin: that would be `ws://`, the cleartext
 *  link the enrollment string exists to close, so it is never emitted. */
export function agentWssUrl(origin: string): string | null {
  if (origin.startsWith("https://")) return `wss://${origin.slice("https://".length)}`;
  return null;
}

/** Refusal reason shared by the composer and by `mint()`'s pre-flight guard
 *  (EnrollHostModal) for a plain-http origin. */
export const HTTP_ORIGIN_REFUSAL =
  "Open this page over HTTPS to enroll a remote host. From an http:// page the " +
  "string would tell the agent to dial ws://, which carries the enrollment token " +
  "and node secret in cleartext.";

/** `mint()`'s defense-in-depth check: re-run right before a single-use token
 *  is spent, so the origin is confirmed to compose a wss:// URL independent
 *  of whatever gates the mint button in the UI. */
export function canMintFrom(origin: string): boolean {
  return agentWssUrl(origin) !== null;
}

/** RFC 4648 §5, no padding — the alphabet has no `.`, which is the separator. */
export function base64UrlNoPad(s: string): string {
  const bytes = new TextEncoder().encode(s);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export type EnrollmentStringResult =
  | { ok: true; value: string; url: string; fingerprint: string | null }
  | { ok: false; reason: string };

export function composeEnrollmentString(opts: {
  origin: string;
  /** The served certificate's SHA-256 when it is self-signed; `null` for a real CA. */
  fingerprint: string | null;
  token: string;
}): EnrollmentStringResult {
  const url = agentWssUrl(opts.origin);
  if (!url) {
    return { ok: false, reason: HTTP_ORIGIN_REFUSAL };
  }
  let fingerprint: string | null = null;
  if (opts.fingerprint !== null) {
    fingerprint = normalizeFingerprint(opts.fingerprint);
    if (!fingerprint) {
      return { ok: false, reason: "The served certificate's fingerprint is not a SHA-256." };
    }
  }
  const token = opts.token.trim();
  if (!token) return { ok: false, reason: "The minted token is empty." };
  return {
    ok: true,
    value: `${ENROLLMENT_PREFIX}.${fingerprint ?? ""}.${base64UrlNoPad(url)}.${token}`,
    url,
    fingerprint,
  };
}

/**
 * The one-line installer (#100). The control plane serves `deploy/enroll-host.sh`
 * itself at `/enroll-host.sh` (the SPA build copies it into the bundle), so the
 * script is by construction the one from the tree the control plane runs. A
 * self-signed control plane is fetched with `-k --pinnedpubkey 'sha256//<spki>'`:
 * curl then trusts nothing but that key — `-k` alone would hand anyone on the
 * path a root shell. A real-CA certificate gets neither flag. The enrollment
 * string travels as an environment variable on the `sh` side, never in the URL
 * (a token in a GET lands in proxy logs and browser history).
 */

/** A tag, branch or commit that is safe unquoted in a shell word. */
const SAFE_REF = /^[A-Za-z0-9][A-Za-z0-9._\/-]*$/;
/** The string's own alphabet: prefix, hex+colons, base64url, and a token. */
const SAFE_ENROLLMENT = /^qenr1\.[0-9A-F:]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9._~-]+$/;
/** base64 of a 32-byte hash, as the control plane emits `spki_sha256`. */
const SAFE_PIN = /^[A-Za-z0-9+/]{43}=$/;

export function installerScriptUrl(origin: string): string | null {
  if (!origin.startsWith("https://")) return null;
  return `${origin.replace(/\/+$/, "")}/enroll-host.sh`;
}

export function composeInstallCommand(opts: {
  origin: string;
  enrollment: string;
  /** The build's source ref; omitted from the command when unknown. */
  ref: string;
  /** `spki_sha256` of the served certificate when it is self-signed; `null` for a real CA. */
  spkiPin: string | null;
}): string | null {
  const url = installerScriptUrl(opts.origin);
  if (!url) return null;
  if (!SAFE_ENROLLMENT.test(opts.enrollment)) return null;
  const ref = opts.ref.trim();
  if (ref && !SAFE_REF.test(ref)) return null;
  let fetch = "curl -fsSL";
  if (opts.spkiPin !== null) {
    if (!SAFE_PIN.test(opts.spkiPin)) return null;
    fetch += ` -k --pinnedpubkey 'sha256//${opts.spkiPin}'`;
  }
  const env = `QUASAR_ENROLLMENT='${opts.enrollment}'${ref ? ` QUASAR_REF=${ref}` : ""}`;
  return `${fetch} ${url} | ${env} sh`;
}
