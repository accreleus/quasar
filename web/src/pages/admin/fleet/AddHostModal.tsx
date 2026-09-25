/**
 * Add host (#359; design_handoff_v3/screens/rh06 add-*.png, `rhAddHost` in
 * assets/pages-rh06.js). One single-use token, minted here, reaches the new
 * machine either through the one-line command (default) or through a seed-only
 * stack for Dockge or Arcane.
 *
 * Composed HERE, not on the server: the server does not know its own reachable
 * address, while this page was reached at one. The fingerprint comes from
 * /v1/admin/access-check — the certificate THIS browser session was served. The
 * images come from the served /enroll-host.sh, so the stack names exactly the
 * seed the one-liner starts. From a plain-http page the string would carry ws://,
 * so nothing is minted.
 */

import { useEffect, useRef, useState, type ReactNode } from "react";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import { IconCopy } from "../../../components/icons";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AccessCheck } from "../../../api/types";
import {
  composeEnrollmentString,
  composeInstallCommand,
  agentWssUrl,
  canMintFrom,
  installerScriptUrl,
  HTTP_ORIGIN_REFUSAL,
} from "../../../lib/enrollmentString";
import {
  composeSeedStack,
  EXPIRY_OPTIONS,
  readServedImages,
  tokenCaption,
  validNodeName,
  type ServedImages,
} from "../../../lib/addHost";

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

type ImagesState = { kind: "loading" } | { kind: "ok"; images: ServedImages } | { kind: "missing" } | { kind: "error"; message: string };

type Created = { enrollment: string; command: string | null; stack: string; expiresAt: string | null; nodeName: string | null };

type Tab = "command" | "stack";

/** `7A:3F:…:C2:19`: enough to compare by eye against the startup log's line. */
function shortFingerprint(fp: string): string {
  const parts = fp.split(":");
  return parts.length > 4 ? `${parts.slice(0, 2).join(":")}:…:${parts.slice(-2).join(":")}` : fp;
}

async function fetchServedScript(origin: string): Promise<string> {
  const url = installerScriptUrl(origin);
  if (!url) throw new Error("not https");
  const res = await fetch(url, { cache: "no-store" });
  if (!res.ok) throw new Error(`${url} answered ${res.status}`);
  return res.text();
}

export function AddHostModal({
  open,
  onClose,
  connectedNodeNames = [],
  origin = typeof window === "undefined" ? "" : window.location.origin,
  fetchScript = fetchServedScript,
}: {
  open: boolean;
  onClose: () => void;
  /** Node names whose agent is connected now: a command bound to one would be refused. */
  connectedNodeNames?: readonly string[];
  /** Overridable for tests; the page's origin otherwise. */
  origin?: string;
  /** Reads the served /enroll-host.sh; overridable for tests. */
  fetchScript?: (origin: string) => Promise<string>;
}) {
  const { token } = useAuth();
  const wssUrl = agentWssUrl(origin);
  // Read through a ref: a caller's inline function must not re-run the load.
  const fetchScriptRef = useRef(fetchScript);
  fetchScriptRef.current = fetchScript;
  const [tab, setTab] = useState<Tab>("command");
  const [nodeName, setNodeName] = useState("");
  const [expiry, setExpiry] = useState(0);
  const [cert, setCert] = useState<CertState>({ kind: "loading" });
  const [images, setImages] = useState<ImagesState>({ kind: "loading" });
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<ReactNode | null>(null);
  const [created, setCreated] = useState<Created | null>(null);

  // HostsTab mounts this once and toggles `open`: a previous host's token must
  // not survive a close/reopen.
  useEffect(() => {
    if (!open) return;
    setCreated(null);
    setError(null);
    setCreating(false);
    setNodeName("");
    setExpiry(0);
    setTab("command");
  }, [open]);

  useEffect(() => {
    if (!open || !token || !wssUrl) return;
    let cancelled = false;
    setCert({ kind: "loading" });
    setImages({ kind: "loading" });
    adminApi
      .accessCheck(token)
      .then((check) => {
        if (!cancelled) setCert(certStateOf(check));
      })
      .catch((e: unknown) => {
        if (!cancelled)
          setCert({ kind: "error", message: e instanceof ApiError ? e.message : "Could not read the served certificate." });
      });
    fetchScriptRef.current(origin)
      .then((text) => {
        if (cancelled) return;
        const served = readServedImages(text);
        setImages(served ? { kind: "ok", images: served } : { kind: "missing" });
      })
      .catch((e: unknown) => {
        if (!cancelled)
          setImages({ kind: "error", message: e instanceof Error ? e.message : "Could not read /enroll-host.sh." });
      });
    return () => {
      cancelled = true;
    };
  }, [open, token, wssUrl, origin]);

  if (!open) return null;

  const fingerprint = cert.kind === "self_signed" ? cert.fingerprint : null;
  const spkiPin = cert.kind === "self_signed" ? cert.spkiPin : null;
  const certReady = cert.kind === "self_signed" || cert.kind === "public_ca" || cert.kind === "proxied";
  const canCreate = !!token && !!wssUrl && certReady && images.kind === "ok" && !creating;

  async function create() {
    if (!token || images.kind !== "ok") return;
    // Re-checked at the moment of spending: never burn the single-use token
    // when the command cannot be composed or would be refused.
    if (!canMintFrom(origin)) {
      setError(HTTP_ORIGIN_REFUSAL);
      return;
    }
    const name = nodeName.trim();
    if (name && !validNodeName(name)) {
      setError(<>A node name is 1 to 253 letters, digits, <span className="mono">-</span>, <span className="mono">_</span> and <span className="mono">.</span>.</>);
      return;
    }
    if (name && connectedNodeNames.includes(name)) {
      setError(
        <>
          <strong>Could not create the command.</strong> {name} is connected right now, so a command bound to its
          name would be refused. Choose another name, or leave it empty.
        </>,
      );
      return;
    }
    setCreating(true);
    setError(null);
    try {
      const { enrollment } = await adminApi.mintHostEnrollment(token, {
        max_uses: 1,
        expires_at: new Date(Date.now() + EXPIRY_OPTIONS[expiry].ms).toISOString(),
        ...(name ? { node_name: name } : {}),
      });
      const composed = composeEnrollmentString({ origin, fingerprint, token: enrollment.token });
      if (!composed.ok) {
        setError(composed.reason);
        return;
      }
      setCreated({
        enrollment: composed.value,
        command: composeInstallCommand({ origin, enrollment: composed.value, nodeName: name || null, spkiPin }),
        stack: composeSeedStack({ ...images.images, enrollment: composed.value, nodeName: name || null }),
        expiresAt: enrollment.expires_at ?? null,
        nodeName: name || null,
      });
    } catch (e) {
      setError(
        <>
          <strong>Could not create the command.</strong>{" "}
          {e instanceof ApiError ? e.message : "The control plane did not mint an enrollment token."}
        </>,
      );
    } finally {
      setCreating(false);
    }
  }

  const caption = created ? tokenCaption(created.expiresAt, created.nodeName) : "";

  const tabs = (
    <nav className="tabs addhost-tabs" role="tablist" aria-label="Ways to add a host">
      {(
        [
          ["command", "One-line command"],
          ["stack", "Dockge or Arcane"],
        ] as const
      ).map(([id, label]) => (
        <button
          key={id}
          type="button"
          role="tab"
          aria-selected={tab === id}
          className={tab === id ? "tab active" : "tab"}
          onClick={() => setTab(id)}
        >
          {label}
        </button>
      ))}
    </nav>
  );

  const options = (
    <div className="addhost-options">
      <div className="field">
        <label className="label" htmlFor="addhost-node-name">
          Node name <span className="hint addhost-optional">optional</span>
        </label>
        <input
          id="addhost-node-name"
          className="input"
          value={created ? (created.nodeName ?? "") : nodeName}
          placeholder="any name"
          disabled={!!created}
          onChange={(e) => setNodeName(e.target.value)}
        />
        <span className="hint">
          Binds the command to this name. Use an existing host&apos;s name to re-enroll it and keep its history.
        </span>
      </div>
      <div className="field">
        <label className="label" htmlFor="addhost-expiry">
          Expires
        </label>
        <select
          id="addhost-expiry"
          className="select"
          value={expiry}
          disabled={!!created}
          onChange={(e) => setExpiry(Number(e.target.value))}
        >
          {EXPIRY_OPTIONS.map((o, i) => (
            <option key={o.label} value={i}>
              {o.label}
            </option>
          ))}
        </select>
      </div>
    </div>
  );

  const facts = (
    <>
      {(cert.kind === "self_signed" || cert.kind === "loading") && (
        <div className="ae-facts addhost-facts">
          <div className="ae-fact">
            <span>Pinned certificate</span>
            {cert.kind === "self_signed" ? (
              <span className="mono addhost-fp" data-testid="enroll-fingerprint" title={cert.fingerprint}>
                SHA256 {shortFingerprint(cert.fingerprint)}
              </span>
            ) : (
              <span className="hint">reading this control plane&apos;s certificate…</span>
            )}
          </div>
        </div>
      )}
      {cert.kind === "proxied" && <p className="hint addhost-intro">{cert.reason}</p>}
      {cert.kind === "error" && (
        <div className="note warn addhost-note" role="alert">
          {cert.message}
        </div>
      )}
      {images.kind === "missing" && (
        <div className="note warn addhost-note" role="alert" data-testid="addhost-no-images">
          <strong>Could not create the command.</strong> This control plane names no seed and node-agent image to
          install. Set <span className="mono">QUASAR_ENROLL_SEED_IMAGE</span> and{" "}
          <span className="mono">QUASAR_ENROLL_AGENT_IMAGE</span> on it, by digest (docs/configuration.md, &quot;Add
          host&quot;).
        </div>
      )}
      {images.kind === "error" && (
        <div className="note warn addhost-note" role="alert">
          <strong>Could not create the command.</strong> The installer this control plane serves could not be read (
          {images.message}).
        </div>
      )}
      {error && (
        <div className="note warn addhost-note" role="alert">
          {error}
        </div>
      )}
      <Button variant="primary" disabled={!canCreate} onClick={() => void create()}>
        {creating ? "Creating…" : tab === "command" ? "Create command" : "Create stack"}
      </Button>
    </>
  );

  let body: ReactNode;
  if (!wssUrl) {
    body = (
      <div className="note warn" data-testid="enroll-needs-https">
        <strong>Open this page over HTTPS to add a host.</strong> From an http:// page the command would tell the new
        host to connect without TLS, sending its enrollment string and node secret across the network in the clear.
      </div>
    );
  } else if (tab === "command") {
    body = (
      <>
        {!created && (
          <p className="hint addhost-intro">
            One command adds a machine with Docker and a GPU. Create it here, run it on that machine as root, and the
            host appears in the table when it has enrolled.
          </p>
        )}
        {options}
        {!created && facts}
        {created && created.command && (
          <>
            <Snippet
              caption="Run on the new host as root"
              sub={caption}
              text={created.command}
              testId="enroll-command"
              label="Copy install command"
            />
            <p className="hint addhost-explain">
              The command checks the host — render node, virtual input, user namespaces, the app-container AppArmor
              profile — and offers to fix what is missing. Then it starts Quasar&apos;s seed and waits until the host
              is enrolled. It writes no compose file, no environment file and no install directory; running it again
              on an enrolled machine changes nothing.
              {spkiPin && (
                <>
                  {" "}
                  <span className="mono addhost-nowrap">--pinnedpubkey</span> makes curl trust only this control
                  plane&apos;s key, so <span className="mono addhost-nowrap">-k</span> here is not &ldquo;trust
                  anything&rdquo;.
                </>
              )}{" "}
              <a href={SECOND_HOST_DOCS} target="_blank" rel="noreferrer">
                Add a second GPU host
              </a>
            </p>
            <details className="enroll-more addhost-more">
              <summary>Show the enrollment string</summary>
              <Snippet text={created.enrollment} testId="enroll-string" label="Copy enrollment string" />
            </details>
          </>
        )}
        {created && !created.command && (
          <>
            <div className="note warn addhost-note" data-testid="enroll-no-installer">
              The install command could not be composed for this certificate. Use the stack on the Dockge or Arcane
              tab instead, or see{" "}
              <a href={SECOND_HOST_DOCS} target="_blank" rel="noreferrer">
                Add a second GPU host
              </a>
              .
            </div>
            <Snippet caption="Enrollment string" sub={caption} text={created.enrollment} testId="enroll-string" label="Copy enrollment string" />
          </>
        )}
      </>
    );
  } else {
    body = (
      <>
        <h3 className="addhost-lead">Using Dockge or Arcane? Paste this stack instead.</h3>
        <p className="hint addhost-intro">
          The stack holds only the seed. Quasar&apos;s own services start beside it as separate containers; redeploying
          or removing the stack never touches them.
        </p>
        {options}
        {!created && facts}
        {created && (
          <>
            <Snippet caption="Stack file" sub={caption} text={created.stack} testId="addhost-stack" label="Copy stack file" />
            <div className="note warn addhost-prep">
              <strong>Preparing the host is then your job.</strong> The one-line command checks and fixes the render
              node, virtual input, user namespaces and the app-container AppArmor profile; a stack cannot. Once the host
              enrolls, its readiness card lists anything that is missing.
            </div>
          </>
        )}
      </>
    );
  }

  return (
    <Modal
      open
      onClose={onClose}
      title="Add host"
      maxWidth={640}
      footer={
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      }
    >
      {tabs}
      {body}
    </Modal>
  );
}

/** A copyable value. The text stays selectable, so a failed clipboard write costs
 *  nothing; "Copied" only follows a write that resolved. */
function Snippet({
  caption,
  sub,
  text,
  testId,
  label,
}: {
  caption?: string;
  sub?: string;
  text: string;
  testId: string;
  label: string;
}) {
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
    <div className="addhost-snippet">
      {caption && (
        <div>
          <div className="eyebrow">{caption}</div>
          {sub && <div className="hint addhost-sub">{sub}</div>}
        </div>
      )}
      <div className="enroll-snippet">
        <pre className="mono" data-testid={testId}>
          {text}
        </pre>
        <Button variant="ghost" size="sm" onClick={() => void copy()} aria-label={label}>
          <IconCopy />
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </div>
  );
}
