// launchPlan against ResolveDefaultProfile and the launch gate (launcher.go), row by row.

import { describe, expect, it } from "vitest";
import type { ProfilesResponse } from "../../../api/types";
import { optionsFor } from "../launchOptions";
import { launchPlan, type LaunchPlan } from "./launchPlan";

const profile = (id: string, height: number, eligibility: string) => {
  const width = Math.round((height * 16) / 9);
  const rung = (codec: string, position: number) => ({ id: `${id}-${codec}`, display_name: id, codec, width, height, fps: 60, nominal_bitrate_kbps: 8000, position, eligibility, reasons: [] });
  // HEVC first, so a browser without H.264 still decodes each profile and only one-click changes.
  return { id, display_name: id, description: "", nominal: { width, height, fps: 60, bitrate_kbps: 8000 }, eligibility, reasons: [], rungs: [rung("hevc", 1), rung("h264", 2)] };
};
/** p-gone is not in the menu: outside the app's allow-list. */
const menu = (recommended: string) =>
  ({
    recommended_id: recommended,
    confidence: "high",
    notes: [],
    profiles: [profile("p-rec", 1080, "eligible"), profile("p-ok", 720, "eligible"), profile("p-risky", 1440, "risky"), profile("p-bad", 2160, "ineligible")],
  }) as unknown as ProfilesResponse;

type Policy = "inherit" | "prefer" | "force";
interface Row {
  name: string;
  policy?: Policy;
  pin?: string | null;
  /** undefined: the read failed. */
  prefs?: { def?: string; global?: string; overrides?: boolean };
  rec?: string;
  admin?: boolean;
  h264?: boolean;
  remembered?: boolean;
  want: LaunchPlan;
}

const plan = (serverPick: string | null, oneClick: boolean, locked: LaunchPlan["locked"], seedId: string | null): LaunchPlan => ({
  serverPick,
  oneClick,
  locked,
  seedId,
});
const L = (def?: string, global?: string) => ({ def, global, overrides: true });
const Loff = (def?: string, global?: string) => ({ def, global, overrides: false });

const rows: Row[] = [
  // Preferences unknown: Change off, Play lets the server pick, whatever the pin.
  { name: "unknown, inherit", want: plan(null, true, "prefs", null) },
  { name: "unknown, prefer pin", policy: "prefer", pin: "p-ok", want: plan(null, true, "prefs", null) },
  { name: "unknown, force pin", policy: "force", pin: "p-ok", want: plan(null, true, "prefs", null) },
  { name: "unknown, risky recommendation", rec: "p-risky", want: plan(null, true, "prefs", null) },

  // No server pick: one-click when the recommendation is eligible and H.264 decodes, else the band.
  { name: "none, eligible recommendation", prefs: L(), want: plan(null, true, null, null) },
  { name: "none, risky recommendation", prefs: L(), rec: "p-risky", want: plan(null, false, null, null) },
  { name: "none, no H.264", prefs: L(), h264: false, want: plan(null, false, null, null) },
  { name: "inherit ignores the app's default", policy: "inherit", pin: "p-bad", prefs: L(), want: plan(null, true, null, null) },
  { name: "none, remembered", prefs: L(), remembered: true, want: plan(null, false, null, null) },

  // A server pick that streams here: one-click, and Change opens on it.
  { name: "account default, eligible", prefs: L("p-ok"), want: plan("p-ok", true, null, "p-ok") },
  { name: "account default, eligible, risky recommendation", prefs: L("p-ok"), rec: "p-risky", want: plan("p-ok", true, null, "p-ok") },
  { name: "global default, eligible", prefs: L(undefined, "p-ok"), want: plan("p-ok", true, null, "p-ok") },
  { name: "account default outside the menu falls to the global", prefs: L("p-gone", "p-ok"), want: plan("p-ok", true, null, "p-ok") },
  { name: "prefer pin beats the account default", policy: "prefer", pin: "p-ok", prefs: L("p-risky"), want: plan("p-ok", true, null, "p-ok") },
  { name: "prefer with no pin falls to the account default", policy: "prefer", pin: null, prefs: L("p-ok"), want: plan("p-ok", true, null, "p-ok") },
  { name: "force pin, eligible", policy: "force", pin: "p-ok", prefs: L(), want: plan("p-ok", true, null, "p-ok") },

  // Risky, or no H.264: the band's request, on the server's pick.
  { name: "account default, risky", prefs: L("p-risky"), want: plan("p-risky", false, null, "p-risky") },
  { name: "account default, no H.264", prefs: L("p-ok"), h264: false, want: plan("p-ok", false, null, "p-ok") },
  { name: "force pin, risky", policy: "force", pin: "p-risky", prefs: L(), want: plan("p-risky", false, null, "p-risky") },

  // A server pick that cannot stream here, overrides allowed: the band's request on the recommendation.
  { name: "account default, ineligible", prefs: L("p-bad"), want: plan("p-bad", false, null, null) },
  { name: "global default, ineligible", prefs: L(undefined, "p-bad"), want: plan("p-bad", false, null, null) },
  { name: "prefer pin, ineligible", policy: "prefer", pin: "p-bad", prefs: L("p-ok"), want: plan("p-bad", false, null, null) },
  { name: "prefer pin outside the menu (never skipped)", policy: "prefer", pin: "p-gone", prefs: L("p-ok"), want: plan("p-gone", false, null, null) },
  { name: "admin, account default ineligible", admin: true, prefs: L("p-bad"), want: plan("p-bad", false, null, null) },

  // Overrides off: Play lets the server pick, even where that 409s (#721 for the account default).
  { name: "overrides off, none, risky recommendation", prefs: Loff(), rec: "p-risky", want: plan(null, true, "overrides", null) },
  { name: "overrides off, account default eligible", prefs: Loff("p-ok"), want: plan("p-ok", true, "overrides", "p-ok") },
  { name: "overrides off, account default ineligible", prefs: Loff("p-bad"), want: plan("p-bad", true, "overrides", null) },
  { name: "overrides off, prefer pin", policy: "prefer", pin: "p-ok", prefs: Loff(), want: plan("p-ok", true, "overrides", "p-ok") },
  { name: "overrides off, remembered", prefs: Loff(), remembered: true, want: plan(null, true, "overrides", null) },
  // A force pin the menu offers keeps Change (its codec), and Play still sends nothing.
  { name: "overrides off, force pin risky", policy: "force", pin: "p-risky", prefs: Loff(), want: plan("p-risky", true, null, "p-risky") },
  { name: "overrides off, admin, account default ineligible", admin: true, prefs: Loff("p-bad"), want: plan("p-bad", false, null, null) },

  // A force pin the menu cannot offer: per-game lock.
  { name: "force pin, ineligible", policy: "force", pin: "p-bad", prefs: L(), want: plan("p-bad", true, "forced", null) },
  { name: "force pin outside the menu", policy: "force", pin: "p-gone", prefs: L(), want: plan("p-gone", true, "forced", null) },
  { name: "force with no pin, account default", policy: "force", pin: null, prefs: L("p-ok"), want: plan("p-ok", true, "forced", "p-ok") },
  { name: "force pin ineligible, overrides off", policy: "force", pin: "p-bad", prefs: Loff(), want: plan("p-bad", true, "forced", null) },
  { name: "admin, force pin ineligible", admin: true, policy: "force", pin: "p-bad", prefs: L(), want: plan("p-bad", false, null, null) },
  { name: "admin, force pin eligible", admin: true, policy: "force", pin: "p-ok", prefs: L(), want: plan("p-ok", true, null, "p-ok") },
];

describe("launchPlan", () => {
  it.each(rows)("$name", (r) => {
    const h264 = r.h264 ?? true;
    const app = { profile_policy: r.policy ?? "inherit", default_profile_id: r.pin === undefined ? null : r.pin } as never;
    const data = menu(r.rec ?? "p-rec");
    const model = optionsFor({ app, data, caps: { h264, hevc: true, av1: false, vp9: false }, isAdmin: r.admin ?? false });
    const prefs = r.prefs
      ? { default_profile_id: r.prefs.def ?? null, global_default_profile_id: r.prefs.global ?? null, user_overrides_allowed: r.prefs.overrides ?? true }
      : null;
    const got = launchPlan({ app, model, recommendedId: data.recommended_id, prefs, isAdmin: r.admin ?? false, h264, remembered: r.remembered ?? false });
    expect(got).toEqual(r.want);
  });
});
