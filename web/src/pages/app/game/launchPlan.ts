// What the game page's Play sends, from what the server starts for a launch with
// no profile (ResolveDefaultProfile, control-plane/internal/session/profile_preferences.go).

import type { App, ProfilePreferencesResponse } from "../../../api/types";
import type { LaunchOptionsModel } from "../launchOptions";

export interface LaunchPlan {
  /** The profile a launch with no profile starts, or null when it falls to the recommendation (or is unknown). */
  serverPick: string | null;
  /** Play sends `{app_id, mic}` and the server picks; otherwise it sends the selection, as the band does. */
  oneClick: boolean;
  /** Why Change is off: the preferences did not load, a force pin this menu cannot offer, or overrides are off. */
  locked: "prefs" | "forced" | "overrides" | null;
  /** The profile the selection opens on; null keeps optionsFor's seed (the recommendation, or a force pin). */
  seedId: string | null;
}

export function launchPlan(i: {
  app: Pick<App, "profile_policy" | "default_profile_id">;
  model: LaunchOptionsModel;
  recommendedId: string;
  /** null: the read failed. The page does not guess, it lets the server pick. */
  prefs: ProfilePreferencesResponse | null;
  isAdmin: boolean;
  h264: boolean;
  /** This game's own last settings (gameData) are what Play uses. */
  remembered: boolean;
}): LaunchPlan {
  const { app, model, prefs, isAdmin } = i;
  if (!prefs) return { serverPick: null, oneClick: true, locked: "prefs", seedId: null };
  const auto = model.space.entriesByCodec.get("auto") ?? [];
  const row = (id: string | null | undefined) => (id ? auto.find((e) => e.profileId === id) : undefined);

  // The pin is never skipped; a default outside the app's menu (its allow-list) is.
  // Eligibility is not checked, and the account default wins even with overrides off (#721).
  const pin = app.profile_policy !== "inherit" ? app.default_profile_id || null : null;
  const serverPick = pin ?? [prefs.default_profile_id, prefs.global_default_profile_id].find((id) => row(id)) ?? null;
  const pick = row(serverPick);

  const overridesOff = !isAdmin && prefs.user_overrides_allowed === false;
  // The server refuses every profile but a force pin (launcher.go), so Change could only 409.
  const locked =
    isAdmin || model.pinnedProfileId ? null : app.profile_policy === "force" ? "forced" : overridesOff ? "overrides" : null;
  const target = serverPick ? pick : row(i.recommendedId);
  const oneClick = locked !== null || overridesOff || (!i.remembered && i.h264 && target?.eligibility === "eligible");
  return { serverPick, oneClick, locked, seedId: pick && pick.eligibility !== "ineligible" ? serverPick : null };
}
