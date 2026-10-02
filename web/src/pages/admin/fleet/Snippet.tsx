/**
 * A copyable value in the handoff's `.snippet` block (the product's `.enroll-snippet`):
 * Add host's command and stack (#359), and the restore command of a failed migrating
 * update (#364). The text stays selectable, so a failed clipboard write costs nothing.
 */

import { useState } from "react";
import { Button } from "../../../components/Button";
import { IconCopy } from "../../../components/icons";

/** A copyable value. The text stays selectable, so a failed clipboard write costs
 *  nothing; "Copied" only follows a write that resolved. */
export function Snippet({
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
