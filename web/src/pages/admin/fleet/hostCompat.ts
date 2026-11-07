// What the host detail "Compatibility tools" card derives from
// GET /v1/admin/hosts/{id}/compat-tools (control-api.md §Compatibility tools,
// "Per-host install state"): pure, so the copy is tested without a render.

import type { HostCompatTools } from "../../../api/types";
import type { ChipVariant } from "../../../components/Chip";
import { bytes } from "../../../lib/format/bytes";

export type HostTool = HostCompatTools["tools"][number];

/** The state chip: label and variant per `state`. A deferred-for-disk install
 *  reads "Not started" (mock #1067), not a bare "Waiting". */
export function toolChip(t: HostTool): { variant?: ChipVariant; label: string; dot?: boolean } {
  switch (t.state) {
    case "installed":
      return { variant: "success", label: "Installed", dot: true };
    case "installing":
      return { variant: "info", label: "Installing", dot: true };
    case "removing":
      return { variant: "info", label: "Removing", dot: true };
    case "failed":
      return { variant: "danger", label: "Failed" };
    case "unsupported":
      return { variant: "warning", label: "Unsupported" };
    case "waiting":
      return t.reason === "insufficient_disk" ? { variant: "warning", label: "Not started" } : { label: "Waiting" };
  }
}

// `summary.reason` is an open string; these are the ones the contract defines.
const REASON: Record<string, string> = {
  invalid_params: "The control plane sent a request this host refused.",
  download_failed: "The download from github.com failed. It is retried within the hour.",
  redirect_refused: "github.com sent the download somewhere it is not allowed to.",
  size_mismatch: "The download was not the size published for it.",
  checksum_mismatch: "The download did not match its checksum.",
  unsafe_archive: "The archive holds a file that is not safe to extract.",
  archive_too_large: "The archive unpacks larger than allowed.",
  vdf_invalid: "The archive's tool description could not be read.",
  name_reserved: "The tool's name is reserved.",
  name_mismatch: "The tool's name differs from the one installed on other hosts.",
  name_conflict: "The tool's name clashes with another tool.",
  io_error: "This host could not write the tool to disk. It is retried within the hour.",
  in_use: "A session on this host is using it. It is removed when that session ends.",
};

/** The line under the state chip; null when there is nothing to say. */
export function toolNote(t: HostTool, freeBytes: number | null): string | null {
  if (t.state === "unsupported") return "This host's agent does not offer compatibility tools. Update it from Fleet ▸ Releases.";
  if (t.state === "waiting" && t.reason === "insufficient_disk") {
    return `Not enough free disk to start.${freeBytes != null ? ` ${bytes(freeBytes)} free.` : ""}`;
  }
  if (t.state === "waiting" && t.reason == null) return "Installs when this host is ready.";
  if (t.reason) return REASON[t.reason] ?? `Reason: ${t.reason}.`;
  return null;
}

/** The "n GB used" head chip: what the installed tools hold on this host. */
export function usedBytes(tools: HostTool[]): number {
  return tools.reduce((sum, t) => sum + (t.state === "installed" ? (t.bytes ?? 0) : 0), 0);
}

/** The first column: a catalogue-less installed tool has no Steam name until
 *  the host reports one, so the tag stands in. */
export function toolName(t: HostTool): string {
  return t.steam_name ?? t.tag;
}
