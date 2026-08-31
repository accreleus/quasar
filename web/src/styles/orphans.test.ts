/**
 * No rule outlives the markup that emitted it.
 *
 * The pre-v3 sheets accumulated blocks whose components were deleted years
 * apart, and dead CSS is invisible: nothing breaks, the file just grows and the
 * next reader cannot tell which half is live. So every class named in a
 * selector in these sheets must appear as a bare token in some `.ts`/`.tsx`
 * under `src/`.
 *
 * The scan is deliberately generous — any occurrence of the token counts, in a
 * `className`, a template literal, a string constant or a comment — because the
 * cost of a false orphan (deleting live styling) is far higher than the cost of
 * a class that survives one release too long. A class assembled at runtime
 * (`` `sdot ${status}` ``) therefore needs its literal name to appear
 * somewhere in source; none currently does not.
 *
 * The v3 sheets (primitives, shell, home, hud, login, account, styleguide) have
 * their own contract tests and are not orphan-scanned here — but the second
 * describe below covers every stylesheet in the tree.
 */
import { describe, expect, it } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, resolve } from "node:path";

const STYLES = __dirname;
const SRC = resolve(__dirname, "..");

const SHEETS = [
  "../styles.css",
  "components.css",
  "admin.css",
  "session.css",
  "admin/audit.css",
  "admin/editor.css",
  "admin/fleet.css",
];

function filesUnder(dir: string, match: RegExp, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) filesUnder(path, match, out);
    else if (match.test(path)) out.push(path);
  }
  return out;
}

const sources = filesUnder(SRC, /\.tsx?$/)
  .map((path) => readFileSync(path, "utf8"))
  .join("\n");

/**
 * Class tokens named in top-level and nested selectors. Everything before a `{`
 * is a selector prelude unless it starts with `@`, which skips at-rule
 * preludes and keyframe stops while still reaching the rules inside them.
 */
function classesIn(css: string): Set<string> {
  const found = new Set<string>();
  for (const [, prelude] of css.replace(/\/\*[\s\S]*?\*\//g, "").matchAll(/([^{}]+)\{/g)) {
    const selector = prelude.trim().replace(/^[};]+/, "").trim();
    if (!selector || selector.startsWith("@")) continue;
    for (const [, name] of selector.matchAll(/\.(-?[_A-Za-z][-\w]*)/g)) found.add(name);
  }
  return found;
}

const used = (name: string): boolean =>
  new RegExp(`(?<![-\\w])${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?![-\\w])`).test(sources);

describe("orphaned CSS", () => {
  it.each(SHEETS)("%s styles only classes the app emits", (sheet) => {
    const orphans = [...classesIn(readFileSync(resolve(STYLES, sheet), "utf8"))]
      .filter((name) => !used(name))
      .sort()
      .map((name) => `.${name}`);
    expect(orphans, `${sheet}: ${orphans.length} orphaned class(es)`).toEqual([]);
  });
});

/**
 * A `var(--x)` naming a property nothing defines resolves to nothing, and the
 * declaration is dropped: a background disappears, a radius goes square, and no
 * tool says a word. That is how the route-error card and the post-session
 * summary shipped transparent — they still referenced `--surface-1` / `--r3`
 * after the token rename to `--surf-*` / `--r-control|panel|feature`.
 *
 * Only the no-fallback form is checked. `var(--ovprev-k, 1)` is how a component
 * hands a measured value to CSS, and its fallback is the contract for the case
 * where the component has not set it yet.
 */
const STYLESHEETS = filesUnder(SRC, /\.css$/);

const declared = new Set(
  STYLESHEETS.flatMap((path) => [
    ...readFileSync(path, "utf8").matchAll(/(?:^|[;{\s])(--[-\w]+)\s*:/g),
  ].map(([, name]) => name)),
);

describe("custom properties", () => {
  it.each(STYLESHEETS.map((path) => [path.slice(SRC.length + 1), path]))(
    "%s references only properties some stylesheet defines",
    (_label, path) => {
      const css = readFileSync(path, "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
      const undefined_ = [
        ...new Set(
          [...css.matchAll(/var\(\s*(--[-\w]+)\s*\)/g)]
            .map(([, name]) => name)
            .filter((name) => !declared.has(name)),
        ),
      ].sort();
      expect(undefined_).toEqual([]);
    },
  );
});
