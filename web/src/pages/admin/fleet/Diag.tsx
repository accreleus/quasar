/**
 * The closed "Details" disclosure of the RH-06 surfaces (fleet-rh06-v3.html `.diag`):
 * the one place wire identifiers (check ids, reasons, labels) appear.
 */

import { IconChevronRight } from "../../../components/icons";

export function Diag({ lines, open }: { lines: string[]; open?: boolean }) {
  return (
    <details className="rh-diag" open={open}>
      <summary>
        <IconChevronRight />
        Details
      </summary>
      <pre>{lines.join("\n")}</pre>
    </details>
  );
}
