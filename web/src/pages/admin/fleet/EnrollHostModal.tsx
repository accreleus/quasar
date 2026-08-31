/**
 * "Enroll host" — instructions, not an action (spec §5.8, §9). A host joins by
 * running the node agent against this control plane's URL and token; there is
 * no enroll endpoint and no supported agent-only package to point at yet.
 */

import { useState, type ReactNode } from "react";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";

// The base Compose file's node-agent service defaults to a localhost control
// plane and depends on the local stack, so `up -d quasar-node-agent` on a
// second machine is not an install — the docs warn against exactly that, and
// this modal must not print it.

/** Where the release's own instructions for this live. */
const SECOND_HOST_DOCS = "https://accreleus.github.io/quasar/install/second-host/";

/**
 * The agent connects over the signaling WebSocket, so the URL it needs is this
 * page's origin with the scheme swapped (https -> wss). Reading it from the
 * browser names the address the operator already reaches this instance at,
 * including a non-default port.
 */
export function agentControlPlaneUrl(origin: string): string {
  if (origin.startsWith("https://")) return `wss://${origin.slice("https://".length)}`;
  if (origin.startsWith("http://")) return `ws://${origin.slice("http://".length)}`;
  return origin;
}

export function EnrollHostModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const controlUrl = agentControlPlaneUrl(
    typeof window === "undefined" ? "" : window.location.origin,
  );

  if (!open) return null;

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
        A host enrolls itself. Once its node agent reaches this control plane with a valid
        enrollment token, it registers and appears in this table.
      </p>

      <div className="fsec">
        <div className="fs-label">
          <h4>1. What the agent needs</h4>
          <p>
            Three values, set on the GPU machine. The token is the{" "}
            <span className="mono">ENROLLMENT_TOKEN</span> in{" "}
            <span className="mono">deploy/.env</span> on this control plane, so read it there
            rather than from a screen.
          </p>
        </div>
        <div className="fs-fields">
          <Snippet text={controlUrl} label="Copy control plane URL" caption="Control plane URL" />
          <Fact
            label="Enrollment token"
            value={
              <>
                <span className="mono">ENROLLMENT_TOKEN</span> from{" "}
                <span className="mono">deploy/.env</span>
              </>
            }
          />
          <Fact
            label="Node name"
            value={
              <>
                a stable <span className="mono">NODE_NAME</span> for the new host
              </>
            }
          />
        </div>
      </div>

      <div className="fsec">
        <div className="fs-label">
          <h4>2. Run the agent there</h4>
          <p>
            The machine runs the <span className="mono">quasar-node-agent</span> image at this
            release, with host networking, GPU and input access, persistent agent data and a
            managed-home root.
          </p>
        </div>
        <div className="fs-fields">
          <p className="note warn" style={{ margin: 0 }}>
            <b>There is no supported agent-only package yet.</b> The base Compose file's
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
        agent log names the reason, usually a token that does not match.
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
        <pre className="mono">{text}</pre>
        <Button variant="ghost" size="sm" onClick={() => void copy()} aria-label={label}>
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </div>
  );
}
