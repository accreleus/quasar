// The per-game compatibility tool (amendment 56, #1076): which Proton one Steam
// game runs on, for every user and host. The mapping is keyed by Steam appid,
// not by tile, and is a separate resource from the app, so the page saves it
// beside the app PATCH.

import type { CompatTool, CompatToolMapping } from "../../../../api/types";

/** `toolId` "" is Steam's default: no mapping. */
export interface CompatChoice {
  toolId: string;
  forced: boolean;
}

export const NO_TOOL: CompatChoice = { toolId: "", forced: false };

export function choiceOf(mapping: CompatToolMapping | undefined): CompatChoice {
  return mapping
    ? { toolId: mapping.tool_id, forced: mapping.forced }
    : NO_TOOL;
}

/** Force means nothing without a tool, so it is dropped with it. */
export function normalise(c: CompatChoice): CompatChoice {
  return c.toolId ? c : NO_TOOL;
}

export function sameChoice(a: CompatChoice, b: CompatChoice): boolean {
  const [x, y] = [normalise(a), normalise(b)];
  return x.toolId === y.toolId && x.forced === y.forced;
}

/** A tool the control plane would accept (`compat_tool_disabled` otherwise). */
const usable = (t: CompatTool) =>
  t.enabled && !t.name_conflict && !t.pin_conflict;

/** Installed tools, then Valve's builds, each in the catalogue's order. The tool
 *  already mapped stays listed even if it is no longer usable, so the select
 *  never shows a value it does not hold. */
export function toolGroups(tools: CompatTool[], current: string) {
  const pick = (family: CompatTool["family"]) =>
    tools.filter((t) => t.family === family && (usable(t) || t.id === current));
  return { installed: pick("ge"), valve: pick("valve") };
}
