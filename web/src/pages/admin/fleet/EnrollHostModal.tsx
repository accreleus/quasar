/**
 * "Enroll host" (#12, #100): mint a per-host enrollment token, compose the
 * one-paste enrollment string, and hand the operator the one-line installer that
 * consumes it on the new machine.
 *
 * Composed HERE, not on the server: the server does not know its own reachable
 * address, while this page was reached at one. The fingerprint comes from
 * /v1/admin/access-check — the certificate THIS browser session was served — so
 * it is shown for comparison against the control plane's startup log only when
 * the certificate is self-signed; a real-CA certificate is verified normally.
 * From a plain-http page the string would carry ws://, so it is refused.
 */

import { useEffect, useState, type ReactNode } from "react";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AccessCheck } from "../../../api/types";
import { SOURCE_REF } from "../../../lib/buildInfo";
import {
  composeEnrollmentString,
  composeInstallCommand,
  agentWssUrl,
  canMintFrom,
  HTTP_ORIGIN_REFUSAL,
} from "../../../lib/enrollmentString";

const SECOND_HOST_DOCS = "https://accreleus.github.io/quasar/install/second-host/";

type CertState =
  | { kind: "loading" }
  | { kind: "self_signed"; fingerprint: string; spkiPin: string }
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
        "A proxy terminates TLS in front of this control plane, so nothing is pinned: the agent " +
          "verifies the proxy's certificate against public CAs. A self-signed proxy certificate " +
          "needs the manual CONTROL_PLANE_FINGERPRINT path on the agent instead.",
    };
  }
  if (cert.info?.self_signed)
    return { kind: "self_signed", fingerprint: cert.info.fingerprint_sha256, spkiPin: cert.info.spki_sha256 };
  return { kind: "public_ca" };
}

type Minted = { value: string; command: string | null; expiresAt: string | null };

export function EnrollHostModal({
  open,
  onClose,
  origin = typeof window === "undefined" ? "" : window.location.origin,
  sourceRef = SOURCE_REF,
}: {
  open: boolean;
  onClose: () => void;
  /** Overridable for tests; the page's origin otherwise. */
  origin?: string;
  /** The ref this build was made from; overridable for tests. */
  sourceRef?: string;
}) {
  const { token } = useAuth();
  const wssUrl = agentWssUrl(origin);
  const [cert, setCert] = useState<CertState>({ kind: "loading" });
  const [minting, setMinting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Minted | null>(null);

  // HostsTab mounts this once and toggles `open`: a previous host's token must
  // not survive a close/reopen.
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
  const spkiPin = cert.kind === "self_signed" ? cert.spkiPin : null;
  const canMint =
    !!token && !!wssUrl && (cert.kind === "self_signed" || cert.kind === "public_ca" || cert.kind === "proxied") && !minting;

  async function mint() {
    if (!token) return;
    // Re-checked at the moment of spending: never burn the single-use token
    // when the string cannot be composed.
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
      setResult({
        value: composed.value,
        command: composeInstallCommand({ origin, enrollment: composed.value, ref: sourceRef, spkiPin }),
        expiresAt: enrollment.expires_at ?? null,
      });
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Could not mint an enrollment token.");
    } finally {
      setMinting(false);
    }
  }

  const expiry = result?.expiresAt ? `expires ${new Date(result.expiresAt).toLocaleTimeString()}` : "expires in an hour";

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
      {!wssUrl ? (
        <p className="note warn" style={{ margin: 0 }} data-testid="enroll-needs-https">
          <b>Open this page over HTTPS to enroll a remote host.</b> From an http:// page the string
          would tell the agent to dial <span className="mono">ws://</span>, which carries the enrollment
          token and node secret across the network in cleartext.
        </p>
      ) : (
        <div className="fs-fields">
          {!result && (
            <p className="sub" style={{ margin: 0 }}>
              One command enrolls a machine with Docker and a GPU. Mint a single-use string, run the
              command there as root, and the host appears in this table when its agent connects.
            </p>
          )}

          {cert.kind === "self_signed" && (
            <Fact
              label="Pinned certificate"
              value={
                <>
                  <span className="mono" data-testid="enroll-fingerprint">{cert.fingerprint}</span>
                  <br />
                  <span className="hint">
                    Matches the <span className="mono">fingerprint=</span> line in the control plane&apos;s
                    startup log. The agent pins it.
                  </span>
                </>
              }
            />
          )}
          {cert.kind === "proxied" && (
            <p className="hint" style={{ margin: 0 }}>
              {cert.reason}
            </p>
          )}
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

          {result && result.command && (
            <>
              <Snippet
                text={result.command}
                testId="enroll-command"
                label="Copy install command"
                caption={`Run on the new host as root — single use, ${expiry}`}
              />
              <p className="hint" style={{ margin: 0 }}>
                This control plane serves the script. The script checks the host, pins the agent image
                to this release, starts the agent and reports when it is enrolled; it never edits the
                firewall.
                {cert.kind === "self_signed" && (
                  <>
                    {" "}
                    <span className="mono">--pinnedpubkey</span> makes curl trust only the key above, so{" "}
                    <span className="mono">-k</span> here is not &quot;trust anything&quot;.
                  </>
                )}{" "}
                <a href={SECOND_HOST_DOCS} target="_blank" rel="noreferrer">
                  Add a second GPU host
                </a>
              </p>
              <details className="enroll-more">
                <summary>Show the enrollment string</summary>
                <BareString value={result.value} expiry={expiry} />
              </details>
            </>
          )}
          {result && !result.command && (
            <>
              <p className="note warn" style={{ margin: 0 }} data-testid="enroll-no-installer">
                The install command could not be composed for this certificate. Use the enrollment
                string with the agent&apos;s own Compose file — see{" "}
                <a href={SECOND_HOST_DOCS} target="_blank" rel="noreferrer">
                  Add a second GPU host
                </a>
                .
              </p>
              <BareString value={result.value} expiry={expiry} />
            </>
          )}
        </div>
      )}
    </Modal>
  );
}

function BareString({ value, expiry }: { value: string; expiry: string }) {
  return (
    <>
      <Snippet text={value} testId="enroll-string" label="Copy enrollment string" caption={`Enrollment string — single use, ${expiry}`} />
      <p className="hint" style={{ margin: 0 }}>
        Shown once. Set it as <span className="mono">QUASAR_ENROLLMENT</span> for the agent; after its
        first connection the pin is saved beside the node secret and the string can be removed.
      </p>
    </>
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

/** A copyable value. The text stays selectable, so a failed clipboard write costs
 *  nothing; "Copied" only follows a write that resolved. */
function Snippet({ text, testId, label, caption }: { text: string; testId: string; label: string; caption: string }) {
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
        <pre className="mono" data-testid={testId}>{text}</pre>
        <Button variant="ghost" size="sm" onClick={() => void copy()} aria-label={label}>
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </div>
  );
}
