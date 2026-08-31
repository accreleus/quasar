// Runtime-spec helpers for the AdminApps page.
// Mirrors control-plane schema: image (string), args (string[]),
// env (Record<string,string>), mounts (string[]), gpu (bool).
//
// runtime_spec is documented (schema.md) as an agent-internal launch detail
// that will grow — the form only edits the known fields, so every key it does
// not recognize (e.g. no_new_privileges on GOW desktop images) must survive a
// parse → edit → serialize round-trip untouched. Dropping unknown keys here is
// what silently stripped no_new_privileges from the Tower catalog and broke
// every GOW desktop launch (sudo vs the hardened default).

export interface RuntimeSpec {
  image: string;
  args: string[];
  env: Record<string, string>;
  mounts: string[];
  gpu: boolean;
  /** Keys the form does not edit, preserved verbatim for re-submission. */
  extras: Record<string, unknown>;
}

const KNOWN_KEYS = new Set(["image", "args", "env", "mounts", "gpu"]);

export function emptySpec(): RuntimeSpec {
  return { image: "", args: [], env: {}, mounts: [], gpu: false, extras: {} };
}

/** Parse an untyped runtime_spec record into a typed RuntimeSpec with safe defaults. */
export function parseSpec(raw: Record<string, unknown>): RuntimeSpec {
  const extras: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(raw)) {
    if (!KNOWN_KEYS.has(k)) extras[k] = v;
  }
  return {
    image: typeof raw.image === "string" ? raw.image : "",
    args: Array.isArray(raw.args) ? (raw.args as string[]) : [],
    env:
      raw.env && typeof raw.env === "object" && !Array.isArray(raw.env)
        ? (raw.env as Record<string, string>)
        : {},
    mounts: Array.isArray(raw.mounts) ? (raw.mounts as string[]) : [],
    gpu: typeof raw.gpu === "boolean" ? raw.gpu : false,
    extras,
  };
}

/** Serialize a RuntimeSpec back to an untyped record for API submission. */
export function specToRecord(spec: RuntimeSpec): Record<string, unknown> {
  return {
    ...spec.extras,
    image: spec.image,
    args: spec.args,
    env: spec.env,
    mounts: spec.mounts,
    gpu: spec.gpu,
  };
}
