/**
 * Engine profiles: whether a (platform, container engine, engine mode) combination is
 * supported, experimental or unsupported, and why.
 *
 * The table is testdata/engine-profiles/profiles.json, not a copy: the node agent's
 * runtime_engine check is held to the same file by a Rust test, so the badge the quick
 * start shows is the verdict the host's readiness card will give.
 */
import TABLE from '../../../testdata/engine-profiles/profiles.json' with { type: 'json' };

export const STATUSES = TABLE.statuses;
export const ENGINES = TABLE.engines;
export const MODES = TABLE.modes;
export const PLATFORMS = TABLE.platforms;

function row(platform, engine, mode) {
  return TABLE.profiles.find((r) => r.platform === platform && r.engine === engine && r.mode === mode);
}

/**
 * The profile for one combination. An unknown platform reads as "other" (the table's
 * catch-all); an engine or mode outside the table is the unknown engine, which is
 * unsupported. Alternatives come back with their platform filled in and their own status.
 */
export function profileFor(platform, engine, mode) {
  const p = Object.hasOwn(PLATFORMS, platform) ? platform : 'other';
  const found = row(p, engine, mode);
  const base = found ?? TABLE.unknownEngine;
  return {
    platform: p,
    engine,
    mode,
    status: base.status,
    reason: base.reason,
    alternatives: base.alternatives.map((alt) => {
      const to = alt.platform ?? p;
      return { platform: to, engine: alt.engine, mode: alt.mode, status: row(to, alt.engine, alt.mode).status };
    }),
  };
}
