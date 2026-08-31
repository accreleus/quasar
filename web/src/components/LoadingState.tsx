/**
 * LoadingState / EmptyState — shared muted-text placeholders (UI component
 * consolidation, Task 4). LoadingState is a polite live region so screen
 * readers announce loading transitions — the a11y defect this task fixes
 * across every migrated site. EmptyState carries no live-region semantics.
 */
import type { ReactNode } from "react";

export function LoadingState({ children = "Loading…" }: { children?: ReactNode }) {
  return (
    <p className="muted" role="status" aria-live="polite">
      {children}
    </p>
  );
}

export function EmptyState({ children }: { children: ReactNode }) {
  return <p className="muted">{children}</p>;
}
