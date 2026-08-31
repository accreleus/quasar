// Minimal --flag value CLI parsing for runner.mjs. No deps.
export function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i += 1) {
    const tok = argv[i];
    if (!tok.startsWith("--")) continue;
    const key = tok.slice(2);
    const next = argv[i + 1];
    if (next === undefined || next.startsWith("--")) {
      out[key] = true;
    } else {
      out[key] = next;
      i += 1;
    }
  }
  return out;
}

export function requireArg(args, name) {
  const key = name.replace(/^--/, "");
  const value = args[key];
  if (value === undefined || value === true) {
    throw new Error(`missing required --${key}`);
  }
  return value;
}
