// Placement preparation is determined by the server's immutable adopted image
// match. Catalog refs and digests can move at sync, so the browser never
// infers management from the mutable catalog entry.
import type { AppPlacement, CatalogImage } from "../../../../api/types";
import type { ChipVariant } from "../../../../components/Chip";
import { HOST_STATE_COPY } from "../../library/imageStatus";

export interface PlacementImageRef {
  ref: string;
  runtimePresetId: string;
}

export type PlacementImage =
  | { kind: "managed"; image: CatalogImage }
  | { kind: "unmanaged"; ref: string }
  | { kind: "unknown" };

export function placementImage(
  ref: PlacementImageRef | null,
  images: CatalogImage[] | undefined,
  placement: AppPlacement,
): PlacementImage {
  if (!images || placement.managed_image_id === undefined) return { kind: "unknown" };
  if (placement.managed_image_id === null) {
    return ref?.ref.trim() ? { kind: "unmanaged", ref: ref.ref.trim() } : { kind: "unknown" };
  }
  const adopted = images.find((i) => i.id === placement.managed_image_id && i.installed);
  return adopted ? { kind: "managed", image: adopted } : { kind: "unknown" };
}

export interface HostImageLine {
  variant: ChipVariant;
  text: string;
  error?: string;
  actionable: boolean;
}

export function hostImageLine(image: CatalogImage, hostId: string): HostImageLine {
  const s = (image.hosts ?? []).find((h) => h.host_id === hostId);
  if (!s || s.state === "absent") {
    return image.lazy
      ? { variant: "neutral", text: "Image not here yet; it downloads when a session is first placed here.", actionable: false }
      : { variant: "warning", text: "Image not on this host yet.", actionable: true };
  }
  const installed = image.installed_version;
  if (s.version && installed && s.version !== installed) {
    return {
      variant: "warning",
      text: `This host holds version ${s.version}, but version ${installed} is installed.`,
      actionable: true,
    };
  }
  if (s.state === "pulling" || s.state === "building") {
    const copy = HOST_STATE_COPY[s.state];
    return { variant: copy.variant, text: `Image: ${copy.label}`, actionable: false };
  }
  if (s.state === "failed") {
    return { variant: "danger", text: "Image failed", error: s.error ?? undefined, actionable: true };
  }
  return { variant: "success", text: "Image ready", actionable: false };
}

export const AWAITING_PREPARATION = "awaiting_preparation";
const REASON_COPY: Record<string, string> = {
  [AWAITING_PREPARATION]: "Selected, but not prepared for this app yet.",
  not_required: "This host is not selected for the app.",
  no_image: "This app has no image to prepare.",
  unmanaged_image: "This app uses an image outside the managed catalog.",
  on_demand: "The managed image downloads on first launch.",
  inventory_unknown: "This host has not reported image inventory.",
  preparation_failed: "Image preparation failed on this host.",
  preparing: "Image preparation is in progress.",
};
export function placementReason(reason: string): string {
  return REASON_COPY[reason] ?? reason;
}
