/**
 * ResourceStates — the loading / error / empty lines a page renders above its
 * content, and nothing else.
 *
 * It deliberately takes NO children. A wrapper that owned the content would
 * need a slot for every page's arrangement (admin/SessionDetail spreads its
 * three states across 80 lines; the audit log keeps its table mounted while
 * loading; fleet/StorageTab hides content behind an error) and would degrade
 * into a
 * pass-through. Rendering only the states sidesteps all of that: pages keep
 * their content, below this, exactly as they do today — including the stale
 * rows that stay visible alongside an error.
 *
 * What it concentrates is small and real: the error markup, which had four
 * dialects across the admin pages (`form-error`, `form-error mt4`,
 * `apps-field-err`, an inline `var(--danger-text)` style), the `role="alert"`
 * that only 2 of ~11 error lines carried, and the `!loading && !error` gate in
 * front of the empty state, which one page had already dropped by hand.
 *
 * Pages whose table owns its own empty copy simply omit `empty`.
 */

import type { ReactNode } from "react";
import { EmptyState, LoadingState } from "./LoadingState";

export interface ResourceStatesProps {
  /** True only before first data — `useResource().loading`. */
  loading: boolean;
  /** `useResource().errorMessage`. */
  error?: string | null;
  /** Usually `items.length === 0`. */
  isEmpty?: boolean;
  /** Empty-state copy. Omit when a Table's `empty` prop already covers it. */
  empty?: ReactNode;
  /** Loading copy, when "Loading…" is not specific enough. */
  loadingLabel?: ReactNode;
}

export function ResourceStates({
  loading,
  error,
  isEmpty,
  empty,
  loadingLabel,
}: ResourceStatesProps) {
  return (
    <>
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      {loading && <LoadingState>{loadingLabel}</LoadingState>}
      {!loading && !error && isEmpty && empty != null && <EmptyState>{empty}</EmptyState>}
    </>
  );
}
