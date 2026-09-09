// Rendering one audit row as words. The server sends ids plus a `names` map
// (semantics: control-api.md "Audit-log names"); this file is the display of
// that — pure, no fetch, no React.
//
// The rule: an id is never shown without its name, and a name never replaces
// its id.

import type { AdminActivityItem } from "../../../api/admin";

/** Verb phrases for the actions the control plane actually emits.
 *
 *  Lowercase past tense, and WITHOUT the object noun — the sentence supplies
 *  that from the target type, so a verb that names it too reads "minted an
 *  invite invite 2fc39454".
 *
 *  Keys must match the server's emitted strings exactly; a stale one falls
 *  through to `humanise()` unnoticed. To re-derive the live set:
 *
 *    grep -rhoE '"[a-z_]+(\.[a-z_]+)+"' --include=*.go control-plane/internal
 *
 *  keeping those passed as an `action` argument to an audit recorder. */
const ACTION_VERBS: Record<string, string> = {
  "app.artwork.set": "set cover artwork for",
  "app.artwork.upload": "uploaded cover artwork for",
  "app.artwork.cleared": "cleared cover artwork for",
  "app.artwork.reresolve": "re-resolved cover artwork",
  "app.delete": "deleted",
  "app.entitlement.grant": "granted access to",
  "app.entitlement.revoke": "revoked access to",
  "app.entitlement.set_mode": "changed the entitlement mode for",
  "app.library.rule.set": "set a library rule on",
  "app.library.rule.delete": "removed a library rule from",
  "console.config.update": "updated console settings on",
  "host.delete": "forgot",
  "host.drain": "drained",
  "host.restart": "restarted the agent on",
  "host.settings.update": "updated settings on",
  "host.uncordon": "resumed scheduling on",
  "host_enrollment.minted": "minted",
  "host_enrollment.revoked": "revoked",
  "image.installed": "installed",
  "image.pinned": "pinned",
  "image.unpinned": "unpinned",
  "image.uninstalled": "uninstalled",
  "image.updated": "updated",
  "image.synced": "synced the image catalogue",
  "instance.secret.set": "set instance secret",
  "instance.secret.cleared": "cleared instance secret",
  "instance.settings.updated": "updated instance settings",
  "invite.minted": "minted",
  "invite.revoked": "revoked",
  "job.run": "ran",
  "job.update": "updated",
  "launch_profile.create": "created",
  "launch_profile.update": "updated",
  "launch_profile.delete": "deleted",
  "library.scan.force": "forced a library scan for",
  "platform.apply.run": "started a fleet update to",
  "platform.apply.cancel": "cancelled the fleet update to",
  "platform.apply.host": "applied a release to",
  "platform.revert.host": "reverted",
  "platform.release_webhook.tested": "sent a test release notification",
  "runtime_preset.create": "created",
  "runtime_preset.update": "updated",
  "runtime_preset.delete": "deleted",
  "session.capture": "captured diagnostics from",
  "session.failed": "recorded a failure for",
  "session.launched": "launched",
  "session.stop": "stopped",
  "storage.gc.confirm": "reclaimed storage on",
  "storage.home.tombstone": "marked a home for cleanup",
  "stream_profile.create": "created",
  "stream_profile.update": "updated",
  "stream_profile.delete": "deleted",
  "user.deleted": "deleted",
  "user.disabled": "disabled",
  "user.enabled": "enabled",
  "user.quota_changed": "changed the session quota for",
  "user.role_changed": "changed the role of",
};

/** The word a target type goes by in a sentence. `""` means the verb already
 *  names the object ("marked a home for cleanup 3f2a…", "set instance secret
 *  platform.release_webhook.secret"), so adding one would repeat it. */
const TYPE_NOUNS: Record<string, string> = {
  library: "app",
  platform: "release",
  host_enrollment: "enrollment",
  runtime_preset: "runtime preset",
  stream_profile: "stream profile",
  launch_profile: "launch profile",
  storage_home: "",
  secret: "",
  instance: "",
};

function typeNoun(targetType: string): string {
  return TYPE_NOUNS[targetType] ?? targetType.replace(/_/g, " ");
}

/** Unmapped action: "thing.some_verb" → "thing some verb", so a new server
 *  action still reads as words. */
function humanise(action: string): string {
  return action.replace(/[._]/g, " ").trim();
}

export function actionVerb(action: string): string {
  return ACTION_VERBS[action] ?? humanise(action);
}

/** The name the server resolved for `id`. Absence is the miss — there is no
 *  sentinel — and a present name may be current or as-of-the-event; the two
 *  cannot be told apart. */
export function nameFor(item: AdminActivityItem, id: string | null | undefined): string | undefined {
  if (!id) return undefined;
  const name = item.names?.[id];
  return name ? name : undefined;
}

/** Short id for a narrow column; the readout and the CSV carry it in full. */
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

/** The Target column, shared with the Overview card so both surfaces label the
 *  same row identically: the name, else type + short id, else the bare type. */
export function targetLabel(item: AdminActivityItem): string {
  if (!item.target_id) return item.target_type;
  const name = nameFor(item, item.target_id);
  return name ?? `${item.target_type} ${shortId(item.target_id)}`;
}

/** Who did what to which thing — the line the Detail pane opens with. */
export function actionSentence(item: AdminActivityItem): string {
  // A row with no actor is the system acting on its own; a bare "system" reads
  // as a username here.
  const actor = item.actor_username ?? "The system";
  const verb = actionVerb(item.action);
  // Not sentence-cased: a username is a case-sensitive identifier, and
  // "salty2011" must not be rendered as "Salty2011".
  if (!item.target_id) return `${actor} ${verb}`;
  const noun = typeNoun(item.target_type);
  const name = nameFor(item, item.target_id) ?? shortId(item.target_id);
  return `${actor} ${verb} ${noun ? `${noun} ` : ""}${name}`;
}

/** Scalars as themselves, anything structured as compact JSON — so an unknown
 *  key still shows its value rather than "[object Object]". */
function renderValue(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

/** `app_id` → `app`: once the value is a name, the `_id` suffix is wrong. */
function nameKey(key: string): string {
  return key.endsWith("_id") ? key.slice(0, -3) : key;
}

function detailEntries(item: AdminActivityItem): [string, unknown][] {
  const details = item.details;
  if (!details || typeof details !== "object" || Array.isArray(details)) return [];
  return Object.entries(details as Record<string, unknown>);
}

const SUMMARY_MAX = 120;

/** The Detail column's one-line `key=value` summary, as the v3 mock renders it
 *  (`app=Blender host=quasar-node-1`): names, not ids, in one line's width.
 *  A row with no details names its target instead, so an action whose whole
 *  story is "this happened to that thing" still says something. */
export function summaryLine(item: AdminActivityItem): string {
  const parts: string[] = [];
  for (const [key, value] of detailEntries(item)) {
    if (typeof value === "string") {
      const name = nameFor(item, value);
      if (name) {
        parts.push(`${nameKey(key)}=${name}`);
        continue;
      }
    }
    const rendered = renderValue(value);
    if (rendered !== "") parts.push(`${key}=${rendered}`);
  }
  if (parts.length === 0 && item.target_id) {
    parts.push(`${item.target_type}=${targetLabel(item)}`);
  }
  const line = parts.join(" ");
  return line.length > SUMMARY_MAX ? `${line.slice(0, SUMMARY_MAX - 1)}…` : line;
}

/** The expanded console readout: the sentence, then aligned `key  value` lines
 *  with ids in full, each annotated `app_id  8b1116c8-… (Steam)`. The
 *  action/actor/target lines are the only place the full target id appears. */
export function detailReadout(item: AdminActivityItem): string {
  const rows: [string, string][] = [
    ["action", item.action],
    ["actor", item.actor_username ?? "system"],
  ];
  if (item.target_id) {
    const name = nameFor(item, item.target_id);
    rows.push([
      "target",
      `${item.target_type} ${item.target_id}${name ? ` (${name})` : ""}`,
    ]);
  } else {
    rows.push(["target", item.target_type]);
  }
  for (const [key, value] of detailEntries(item)) {
    const rendered = renderValue(value);
    const name = typeof value === "string" ? nameFor(item, value) : undefined;
    rows.push([key, name ? `${rendered} (${name})` : rendered]);
  }
  const width = Math.max(...rows.map(([key]) => key.length));
  const lines = rows.map(([key, value]) => `${key.padEnd(width)}  ${value}`.trimEnd());
  return `${actionSentence(item)}\n\n${lines.join("\n")}`;
}
