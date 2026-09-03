/**
 * "Enroll host" (#12): mint a per-host enrollment token and compose the one-paste
 * enrollment string the second machine needs — the control plane's wss:// address,
 * the fingerprint of its certificate, and a single-use token that expires in an hour.
 *
 * The string is composed HERE. The server does not know its own reachable address;
 * this page was reached at one. The fingerprint is read from /v1/admin/access-check,
 * i.e. it is the certificate THIS browser session was served — which is why the
 * operator is told to compare it against the control plane's startup log: the value
 * is only as trustworthy as this session, and the comparison is what makes it more.
 *
 * From a plain-http page the string would carry ws://, the cleartext link this
 * exists to close, so it is refused rather than composed.
 */

import { useEffect, useState, type ReactNode } from "react";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AccessCheck } from "../../../api/types";
import {
  composeEnrollmentString,
  agentWssUrl,
  canMintFrom,
  HTTP_ORIGIN_REFUSAL,
} from "../../../lib/enrollmentString";

/** Where the release's own instructions for this live. */
const SECOND_HOST_DOCS = "https://accreleus.github.io/quasar/install/second-host/";

type CertState =
  | { kind: "loading" }
  | { kind: "self_signed"; fingerprint: string }
  | { kind: "public_ca" }
  | { kind: "proxied"; reason: string }
  | { kind: "error"; message: string };

function certStateOf(check: AccessCheck): CertState {
  const cert = check.certificate;
  if (!cert.in_use) {
    return {
      kind: "proxied",
      reason:
        cert.not_in_use_reason ??
        "A proxy in front of this control plane terminates TLS. The enrollment string carries no " +
          "pin for it: the agent verifies the proxy's certificate against public CAs. A proxy " +
          "presenting a self-signed certificate is not supported by this flow — use the manual " +
          "CONTROL_PLANE_FINGERPRINT path on that agent instead.",
    };
  }
  if (cert.info?.self_signed) return { kind: "self_signed", fingerprint: cert.info.fingerprint_sha256 };
  return { kind: "public_ca" };
}

export function EnrollHostModal({
  open,
  onClose,
  origin = typeof window === "undefined" ? "" : window.location.origin,
}: {
  open: boolean;
  onClose: () => void;
  /** Overridable for tests; the page's origin otherwise. */
  origin?: string;
}) {
  const { token } = useAuth();
  const wssUrl = agentWssUrl(origin);
  const [cert, setCert] = useState<CertState>({ kind: "loading" });
  const [minting, setMinting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<{ value: string; fingerprint: string | null; expiresAt: string | null } | null>(null);

  // HostsTab mounts this modal once and toggles `open`, so state from a
  // previous host (the plaintext token, an error, a stuck `minting` flag)
  // would otherwise survive a close/reopen — reset it on every open.
  useEffect(() => {
    if (!open) return;
    setResult(null);
    setError(null);
    setMinting(false);
  }, [open]);

  useEffect(() => {
    if (!open || !token || !wssUrl) return;
    let cancelled = false;
    setCert({ kind: "loading" });
    adminApi
      .accessCheck(token)
      .then((check) => {
        if (!cancelled) setCert(certStateOf(check));
      })
      .catch((e: unknown) => {
        if (!cancelled)
          setCert({ kind: "error", message: e instanceof ApiError ? e.message : "Could not read the served certificate." });
      });
    return () => {
      cancelled = true;
    };
  }, [open, token, wssUrl]);

  if (!open) return null;

  const fingerprint = cert.kind === "self_signed" ? cert.fingerprint : null;
  const canMint = !!token && !!wssUrl && (cert.kind === "self_signed" || cert.kind === "public_ca" || cert.kind === "proxied") && !minting;

  async function mint() {
    if (!token) return;
    // Re-checked here, not just relied on via `canMint`: never spend the
    // single-use token when the string cannot be composed.
    if (!canMintFrom(origin)) {
      setError(HTTP_ORIGIN_REFUSAL);
      return;
    }
    setMinting(true);
    setError(null);
    try {
      const { enrollment } = await adminApi.mintHostEnrollment(token, {});
      const composed = composeEnrollmentString({ origin, fingerprint, token: enrollment.token });
      if (!composed.ok) {
        setError(composed.reason);
        return;
      }
      setResult({ value: composed.value, fingerprint: composed.fingerprint, expiresAt: enrollment.expires_at ?? null });
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Could not mint an enrollment token.");
    } finally {
      setMinting(false);
    }
  }

  return (
    <Modal
      open
      onClose={onClose}
      title="Enroll host"
      maxWidth={620}
      footer={
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      }
    >
      <p className="sub" style={{ marginTop: 0 }}>
        A host enrolls itself. Paste one enrollment string into its <span className="mono">deploy/.env</span>{" "}
        as <span className="mono">QUASAR_ENROLLMENT</span>; once its node agent reaches this control plane
        with it, the host registers and appears in this table.
      </p>

      <div className="fsec">
        <div className="fs-label">
          <h4>1. Mint the enrollment string</h4>
          <p>
            One value carrying this control plane&apos;s address, its certificate fingerprint, and a
            token that is <b>single-use and expires in an hour</b>. Mint one per host.
          </p>
        </div>
        <div className="fs-fields">
          {!wssUrl ? (
            <p className="note warn" style={{ margin: 0 }} data-testid="enroll-needs-https">
              <b>Open this page over HTTPS to enroll a remote host.</b> From an http:// page the
              string would tell the agent to dial <span className="mono">ws://</span>, which carries the
              enrollment token and node secret across the network in cleartext.
            </p>
          ) : (
            <>
              <Fact label="Control plane address" value={<span className="mono">{wssUrl}</span>} />
              {cert.kind === "loading" && <Fact label="Certificate" value="Reading the served certificate…" />}
              {cert.kind === "self_signed" && (
                <Fact
                  label="Certificate fingerprint (pinned)"
                  value={
                    <>
                      <span className="mono" data-testid="enroll-fingerprint">{cert.fingerprint}</span>
                      <br />
                      <span className="hint">
                        Compare this against the <span className="mono">fingerprint=</span> line in the
                        control plane&apos;s startup log before you paste the string. The agent pins it.
                      </span>
                    </>
                  }
                />
              )}
              {cert.kind === "public_ca" && (
                <Fact
                  label="Certificate"
                  value="Chains to a public CA — the agent verifies it normally; nothing is pinned."
                />
              )}
              {cert.kind === "proxied" && <Fact label="Certificate" value={cert.reason} />}
              {cert.kind === "error" && (
                <p className="note warn" style={{ margin: 0 }}>
                  {cert.message}
                </p>
              )}
              {!result && (
                <div>
                  <Button variant="primary" disabled={!canMint} onClick={() => void mint()}>
                    {minting ? "Minting…" : "Mint enrollment string"}
                  </Button>
                </div>
              )}
              {error && (
                <p className="note warn" style={{ margin: 0 }} role="alert">
                  {error}
                </p>
              )}
              {result && (
                <>
                  <Snippet
                    text={result.value}
                    label="Copy enrollment string"
                    caption={
                      result.expiresAt
                        ? `Enrollment string — single use, expires ${new Date(result.expiresAt).toLocaleTimeString()}`
                        : "Enrollment string — single use"
                    }
                  />
                  <p className="hint" style={{ margin: 0 }}>
                    Shown once. Set it as <span className="mono">QUASAR_ENROLLMENT</span> in the new
                    host&apos;s <span className="mono">deploy/.env</span>. After its first connection the
                    agent saves the pin beside its node secret and the string can be removed.
                  </p>
                </>
              )}
            </>
          )}
        </div>
      </div>

      <div className="fsec">
        <div className="fs-label">
          <h4>2. Run the agent there</h4>
          <p>
            The machine runs the <span className="mono">quasar-node-agent</span> image at this
            release, with host networking, GPU and input access, persistent agent data and a
            managed-home root, and a stable <span className="mono">NODE_NAME</span>.
          </p>
        </div>
        <div className="fs-fields">
          <p className="note warn" style={{ margin: 0 }}>
            <b>There is no supported agent-only package yet.</b> The base Compose file&apos;s
            node-agent service defaults to a localhost control plane and depends on the local
            stack, so starting that one service on a second machine is not a second-host
            install. Packaging it is operator work today.
          </p>
          <p className="hint" style={{ margin: 0 }}>
            The full runtime contract is in{" "}
            <a href={SECOND_HOST_DOCS} target="_blank" rel="noreferrer">
              Add a second GPU host
            </a>{" "}
            and in <span className="mono">deploy/README.md</span> in the release you deployed.
          </p>
        </div>
      </div>

      <p className="hint">
        The host appears in this table within a few seconds of registering. If it does not, its
        agent log names the reason — an expired or already-used token, or a certificate that no
        longer matches the pin.
      </p>
    </Modal>
  );
}

function Fact({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="enroll-fact">
      <span className="eyebrow">{label}</span>
      <span>{value}</span>
    </div>
  );
}

/**
 * A copyable value. The text stays selectable, so a failed clipboard write
 * costs nothing — which is why the button flips to "Copied" only after a write
 * that resolved.
 */
function Snippet({ text, label, caption }: { text: string; label: string; caption: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    if (!navigator.clipboard) return;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      /* the value is still selectable */
    }
  };

  return (
    <div className="enroll-fact">
      <span className="eyebrow">{caption}</span>
      <div className="enroll-snippet">
        <pre className="mono" data-testid="enroll-string">{text}</pre>
        <Button variant="ghost" size="sm" onClick={() => void copy()} aria-label={label}>
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </div>
  );
}
