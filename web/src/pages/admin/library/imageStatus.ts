// Shared "is this catalog image mid-install" state machine (#454). Split from
// the Images tab so /setup pages can import it without dragging the whole
// route-split Images bundle into their chunk.

import type { CatalogImage, ImageHostState } from "../../../api/types";
import type { ChipVariant } from "../../../components/Chip";

// First-run-experience §S4: "Downloading…" and "Building…" must stay distinct —
// a multi-GB pull and a stuck build are different problems with different fixes.
export const HOST_STATE_COPY: Record<ImageHostState["state"], { label: string; variant: ChipVariant }> = {
  absent: { label: "Absent", variant: "neutral" },
  pulling: { label: "Downloading…", variant: "info" },
  building: { label: "Building…", variant: "info" },
  ready: { label: "Ready", variant: "success" },
  failed: { label: "Failed", variant: "danger" },
};

export const HOST_IN_FLIGHT_STATES: ImageHostState["state"][] = ["pulling", "building"];

export function hostsInFlight(img: CatalogImage): boolean {
  return (img.hosts ?? []).some((h) => HOST_IN_FLIGHT_STATES.includes(h.state));
}

/** Aggregate-chip state: "pulling" beats "building" (the slow download is the
 *  more actionable thing to surface); null when nothing is in flight. */
export function dominantInFlightState(img: CatalogImage): "pulling" | "building" | null {
  const hosts = img.hosts ?? [];
  if (hosts.some((h) => h.state === "pulling")) return "pulling";
  if (hosts.some((h) => h.state === "building")) return "building";
  return null;
}

// Mid-install ⇒ keep polling. A pulling/building host row is the only in-flight
// signal GET /v1/admin/images exposes: just after adoption, `installed=true`
// with `hosts: []` is indistinguishable from steady state (ensure dispatches
// asynchronously, internal/images/actions.go) — a real but sub-second window.
export function isImageInFlight(img: CatalogImage): boolean {
  return hostsInFlight(img);
}
