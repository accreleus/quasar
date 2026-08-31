// Pure derivations for the Sources tab: the Steam row's meta
// counts, the manual-apps count, the "last scan" text and the scan-now
// toast copy, all computed from data the page already fetched or an API
// result already in hand, so SourcesTab.tsx stays declarative.

import type { AdminApp, ForceScanResult, LibraryUnpublishedItem } from "../../../api/types";
import { elapsedWords } from "../../../lib/format/relativeTime";

export interface SteamCounts {
  /** Derived tiles already published under the Steam provider app. */
  imported: number;
  /** Everything the scanner has seen for this provider: imported + still-pending. */
  discovered: number;
}

/** `steamProviderAppId` is the app row with `library_provider === "steam"`
 *  (never a derived tile's id) — null when no provider app is marked yet, in
 *  which case nothing has been discovered or imported regardless of `apps`. */
export function steamCounts(
  apps: AdminApp[],
  unpublished: LibraryUnpublishedItem[],
  steamProviderAppId: string | null,
): SteamCounts {
  if (!steamProviderAppId) return { imported: 0, discovered: 0 };
  const imported = apps.filter((a) => a.parent_app_id === steamProviderAppId).length;
  return { imported, discovered: imported + unpublished.length };
}

/** Apps an operator typed in by hand: no provider (`library_provider === ""`)
 *  and not a derived tile (`parent_app_id === null`) — excludes both the
 *  Steam provider app itself and everything it discovered. */
export function manualAppCount(apps: AdminApp[]): number {
  return apps.filter((a) => !a.parent_app_id && !a.library_provider).length;
}

/** The tail of "last scan …" (mock §A.15: "last scan 21 minutes ago"), or
 *  "never" before the first completed scan. Prose, so it takes the word form. */
export function lastScanText(lastScanCompletedAt: string | null): string {
  return lastScanCompletedAt ? `${elapsedWords(lastScanCompletedAt)} ago` : "never";
}

/** `inert_reason` is human-readable prose the control plane renders verbatim;
 *  only its presence is contractual (control-api.md), so the UI never maps it.
 *  The one liberty taken is a capital first letter, because both surfaces put
 *  it at the start of a sentence. */
export function inertReasonCopy(reason: string): string {
  return reason.length ? reason[0].toUpperCase() + reason.slice(1) : reason;
}

export interface ScanResultToast {
  variant: "success" | "info";
  title: string;
}

/** `queued: 0` means two different things depending on `inert_reason`/scope,
 *  never collapse them (control-api.md `POST /v1/admin/library/scan`). */
export function scanResultToast(res: ForceScanResult): ScanResultToast {
  if (res.inert_reason) return { variant: "info", title: res.inert_reason };
  if (res.queued > 0) {
    return {
      variant: "success",
      title:
        `Queued ${res.queued} scan${res.queued === 1 ? "" : "s"}` +
        (res.skipped > 0 ? ` (${res.skipped} already in progress, left alone)` : "") +
        ". The agent picks these up within about a minute; tiles appear once each scan reports back.",
    };
  }
  return {
    variant: "info",
    title: "Already in progress. Every eligible scan is already queued or being walked. No new work to do.",
  };
}
