/**
 * The manual update path for a target that cannot be updated from here.
 *
 * One pure function, keyed on the eligibility reason plus what is known about
 * the target, so the Releases page and host detail cannot show an operator two
 * different recipes for the same situation. The commands are the ones
 * `docs/upgrading.md` documents, verbatim — `manualUpdate.docs.test.ts` greps
 * the doc for the skeletons below, so the page and the guide cannot drift.
 *
 * NOTHING FROM THE ENVIRONMENT GOES IN A COMMAND: paths are relative to
 * `deploy/`, and no hostname, address or home directory is ever interpolated.
 * An operator copies these onto the host they are already logged into.
 */

import type { EligibilityReason, PlatformRelease } from "../../api/types";

/** One copyable block: what it does, and the literal text to run. */
export interface ManualCommand {
  label: string;
  /** May be several lines; rendered verbatim in a `<pre>`. */
  command: string;
}

export interface ManualUpdatePath {
  /** Why a manual path is the path — one sentence, not the reason's own text. */
  summary: string;
  /** In order. Empty when there is genuinely nothing to run (an offline host). */
  commands: ManualCommand[];
}

export interface ManualUpdateInputs {
  /** The target's `reason`; null when it is eligible. */
  reason: EligibilityReason | string | null;
  kind: "control_plane" | "host";
  /** `Host.install_mode`, when the caller knows it. */
  installMode?: "registry" | "source" | null;
  /** The host's GPU vendor, lowercased or not, when the caller knows it. The
   *  Releases page does not; host detail does. */
  gpuVendor?: string | null;
  /** The release the target was evaluated against — `available[0]`. */
  release?: PlatformRelease | null;
}

// ── the skeletons, shared with docs/upgrading.md ────────────────────────────

const COMPOSE = "docker compose -f deploy/docker-compose.yml";

/** Registry install, step 2 (`docs/upgrading.md` §Upgrading a registry install). */
export const REGISTRY_PULL_COMMAND = `${COMPOSE} pull quasar-control-plane quasar-node-agent`;
export const REGISTRY_RECREATE_COMMAND = `${COMPOSE} up -d --force-recreate --no-deps quasar-control-plane quasar-node-agent`;

/** The two `.env` lines an apply re-pins. */
export const CONTROL_IMAGE_VAR = "QUASAR_CONTROL_IMAGE";
export const AGENT_IMAGE_VAR = "QUASAR_AGENT_IMAGE";

/** Placeholders, used when the release carries no readable manifest (edge has
 *  none by design). They match the doc's, so the two read identically. */
const CONTROL_IMAGE_PLACEHOLDER =
  "ghcr.io/accreleus/quasar/quasar-control-plane@sha256:<control-plane digest>";
const AGENT_IMAGE_PLACEHOLDER =
  "ghcr.io/accreleus/quasar/quasar-node-agent@sha256:<node-agent digest>";

/** Adding the updater to an existing install (`docs/upgrading.md` §The updater). */
export const UPDATER_STACK_DIR_COMMAND = 'echo "QUASAR_STACK_DIR=$(cd deploy && pwd)" >> deploy/.env';
export const UPDATER_IMAGE_COMMAND =
  'echo "QUASAR_UPDATER_IMAGE=ghcr.io/accreleus/quasar/quasar-updater:latest" >> deploy/.env';
export const UPDATER_UP_COMMAND = `${COMPOSE} up -d --no-deps quasar-updater`;

/** The source path (`docs/upgrading.md` §A normal upgrade). */
export const REDEPLOY_SKELETON = "deploy/redeploy.sh <va|nvidia> <ref>";

// ── the pieces ─────────────────────────────────────────────────────────────

/** `va` for AMD/Intel, `nvidia` for NVIDIA, the doc's own placeholder when the
 *  caller does not know: a guessed profile builds the wrong image. */
export function redeployProfile(gpuVendor?: string | null): string {
  const v = (gpuVendor ?? "").trim().toLowerCase();
  if (v.includes("nvidia")) return "nvidia";
  if (v.includes("amd") || v.includes("intel")) return "va";
  return "<va|nvidia>";
}

/** The ref to redeploy: the version tag on stable, the commit on edge (which
 *  publishes no version by design), the doc's placeholder with no release. */
export function redeployRef(release?: PlatformRelease | null): string {
  if (!release) return "<ref>";
  if (release.version) return `v${release.version}`;
  return release.source_commit;
}

export function redeployCommand(inputs: ManualUpdateInputs): string {
  return `deploy/redeploy.sh ${redeployProfile(inputs.gpuVendor)} ${redeployRef(inputs.release)}`;
}

interface ManifestComponent {
  name?: unknown;
  image?: unknown;
  digest?: unknown;
}

/** The manifest's two components, in the contract's normative order:
 *  control-plane then node-agent. Anything else reads as no manifest at all —
 *  a half-understood manifest must not become a half-right command. */
function pinnedImages(release?: PlatformRelease | null): [string, string] | null {
  const manifest = release?.manifest as { components?: unknown } | null | undefined;
  const components = manifest?.components;
  if (!Array.isArray(components) || components.length !== 2) return null;
  const pins = (components as ManifestComponent[]).map((c) =>
    typeof c?.image === "string" && typeof c?.digest === "string" ? `${c.image}@${c.digest}` : null,
  );
  if (pins[0] == null || pins[1] == null) return null;
  return [pins[0], pins[1]];
}

/** A digest to go BACK to, and the repository it belongs to: what an attempt
 *  recorded as previous. */
export interface PreviousAgentImage {
  image?: string | null;
  digest: string;
}

/** The registry-install recipe: re-pin the digests, pull, recreate.
 *  With `previous`, only the node agent moves — a revert never touches the
 *  control plane's pin (ADR 0002). */
export function registryCommands(
  release?: PlatformRelease | null,
  previous?: PreviousAgentImage | null,
): ManualCommand[] {
  const pins = pinnedImages(release);
  const control = pins ? pins[0] : CONTROL_IMAGE_PLACEHOLDER;
  const agent = pins ? pins[1] : AGENT_IMAGE_PLACEHOLDER;
  if (previous) {
    const repo = previous.image?.trim() || "ghcr.io/accreleus/quasar/quasar-node-agent";
    return [
      {
        label: "1. Pin the previous node-agent digest in deploy/.env",
        command: `${AGENT_IMAGE_VAR}=${repo}@${previous.digest}`,
      },
      {
        label: "2. Pull the pinned image and recreate the agent",
        command: `${REGISTRY_PULL_COMMAND}\n${REGISTRY_RECREATE_COMMAND}`,
      },
    ];
  }
  return [
    {
      label: "1. Pin the release's digests in deploy/.env",
      command: `${CONTROL_IMAGE_VAR}=${control}\n${AGENT_IMAGE_VAR}=${agent}`,
    },
    {
      label: "2. Pull the pinned images and recreate those two services",
      command: `${REGISTRY_PULL_COMMAND}\n${REGISTRY_RECREATE_COMMAND}`,
    },
  ];
}

// ── the one entry point ────────────────────────────────────────────────────

/**
 * The manual path for this target, or null when there is none to show: an
 * eligible target, a target already on the release, and every "wait for
 * something else" reason have no command an operator should run.
 */
export function manualUpdatePath(inputs: ManualUpdateInputs): ManualUpdatePath | null {
  const { reason, release } = inputs;
  switch (reason) {
    case null:
    case undefined:
    case "up_to_date":
    case "no_release":
    case "release_above_control_plane":
    case "control_plane_not_first":
    case "attempt_in_flight":
    case "run_active":
      return null;

    case "host_offline":
      // Nothing to run: whatever an operator would type needs the host back
      // first, and a command shown next to an unreachable host invites it.
      return {
        summary: "Bring this host's agent back online first — there is nothing to run until it reconnects.",
        commands: [],
      };

    case "install_mode_source":
      return {
        summary:
          inputs.kind === "control_plane"
            ? "This control plane runs an image it built itself. The published one is a different build and cannot replace it in place, so it updates with the redeploy script."
            : "This host builds its own images, so it updates with the redeploy script rather than by pinning digests.",
        commands: [
          {
            label: `Rebuild and restart this ${inputs.kind === "control_plane" ? "stack" : "host"} at ${redeployRef(release)}`,
            command: redeployCommand(inputs),
          },
        ],
      };

    case "updater_absent":
      return {
        summary:
          "No updater sits beside this host's stack, so nothing on it can recreate its own containers. Add one once, then update by hand or from here.",
        commands: [
          {
            label: "Add the updater to this host, once",
            command: [UPDATER_STACK_DIR_COMMAND, UPDATER_IMAGE_COMMAND, UPDATER_UP_COMMAND].join("\n"),
          },
          ...registryCommands(release),
        ],
      };

    case "identity_unknown":
      // The agent predates identity reporting, so nothing here knows what it
      // is running; upgrading it is what makes every other answer meaningful.
      return {
        summary:
          inputs.kind === "control_plane"
            ? "Nothing could say how this control plane was installed, and it is never updated on a guess: the published image is a different build from a source one. Update it by hand."
            : "This agent predates identity reporting and has not said what it is running — redeploy or upgrade the agent first, then this page can tell you the rest.",
        commands:
          inputs.installMode === "source" || inputs.kind === "control_plane"
            ? [{ label: "Rebuild and restart this stack", command: redeployCommand(inputs) }]
            : registryCommands(release),
      };

    default:
      // An identifier this build does not know (amendment 2 appends to the
      // vocabulary): say nothing rather than the wrong thing.
      return null;
  }
}
