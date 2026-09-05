/**
 * A command an operator is meant to run, shown verbatim with a Copy button.
 *
 * Extracted from the readiness card so remediation lines and the platform
 * releases page cannot present a command two different ways. Multi-line
 * commands render as typed — the whole block is one copy.
 */

import { useState } from "react";
import { Button } from "./Button";

export interface CopyableCommandProps {
  /** The literal text: shown, and what Copy writes. */
  text: string;
  /** Small label above the block, when the command needs naming. */
  label?: string;
}

export function CopyableCommand({ text, label }: CopyableCommandProps) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    // Guard availability explicitly rather than optional-chain into
    // `writeText` — `clipboard?.writeText(...)` on an absent API awaits
    // `undefined`, which resolves (not rejects), so the button would say
    // "Copied" though nothing was written. Only a real, successful write
    // may flip it.
    if (!navigator.clipboard) return;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Best-effort — the text is still visible and selectable even if the
      // write itself failed (e.g. a permission denial).
    }
  };

  return (
    <>
      {label && (
        <div className="hint" style={{ marginTop: 6 }}>
          {label}
        </div>
      )}
      <div className="row gap2" style={{ marginTop: 4, alignItems: "center" }}>
        <code
          className="mono"
          style={{
            flex: 1,
            overflowWrap: "anywhere",
            whiteSpace: "pre-wrap",
            fontSize: "var(--t-xs)",
          }}
        >
          {text}
        </code>
        <Button type="button" variant="ghost" size="sm" onClick={() => void copy()}>
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </>
  );
}
