// One shared driver resolution for StepHosts + the admin host surfaces, so they
// cannot disagree. Must mirror control-plane's internal/storage
// resolveDriver exactly — the two change together or this file is a lie.
// No admin endpoint returns the resolved driver, hence the client-side mirror.
// "volume" is gone entirely (#473; migration 0068 coerced stored rows): every
// provider value needs a storage root and is misconfigured without one.

import type { StorageProvider } from "../api/types";

export type ResolvedHomeDriver = "local" | "misconfigured";

export interface ResolvedHome {
  driver: ResolvedHomeDriver;
  /** The effective root that WOULD be used if the driver is/were "local".
   *  Empty string when no host has ever reported or been given one. */
  root: string;
}

/** effectiveHomeRoot is `effective?.home_root` (the agent's reported value),
 *  never `resolved.home_root` (a display value blind to agent env).
 *  `provider` is accepted unused — every value resolves purely off the root. */
export function resolveHomeDriver(
  _provider: StorageProvider | undefined,
  effectiveHomeRoot: string | null | undefined,
): ResolvedHome {
  const root = (effectiveHomeRoot ?? "").trim();
  return { driver: root ? "local" : "misconfigured", root };
}

/** Names only the one remedy — the root is the control, so mentioning the
 *  provider setting would point at the legacy driver as if it were a fix. */
export const MISCONFIGURED_LOCAL_IMPACT =
  "This host has no storage root, so its games have nowhere to keep save data and " +
  "sessions placed on it cannot start. Set a storage root below.";
