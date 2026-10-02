/**
 * The design system, checked by machine rather than by review.
 *
 * An inline style type-checks and renders, and `padding: 11px` copied from a
 * mock looks fine, so nothing told anyone they had left the system. Each rule
 * here fails with the fix: the token or class to use instead.
 *
 * - `spacing-scale`: padding/margin/gap use `--s1`…`--s10`. `0`, `auto`,
 *   `var()`, token-only `calc()` and the 1px hairline pass.
 * - `raw-colour-css`: no hex, `rgb()`/`oklch()`/… or named colour outside
 *   tokens.css, gradients and custom-property definitions included.
 * - `inline-style`: `style={{…}}` carries only values a stylesheet cannot know
 *   in advance (`INLINE_STYLE_ALLOWLIST` and `--custom-properties`).
 * - `raw-colour-tsx`: no colour literal in a component, SVG `fill` included.
 *
 * The rules are pure functions in `design-lint.ts`, unit-tested in
 * `design-lint.rules.test.ts`; `npm run lint:design` runs both.
 *
 * `/* design-lint-allow <rule>: <reason> *\/` (or `//` in TSX) on the same or
 * preceding line suppresses exactly one violation of that rule. An allow with
 * no reason is itself a violation.
 *
 * `design-lint.baseline.json` holds the existing debt per file per rule, and a
 * count only goes down: above it fails with the new sites, below it fails until
 * `UPDATE_DESIGN_BASELINE=1 npm run lint:design` locks the drop in, and a file
 * with no entry must be clean. The update never raises a count.
 */
import { describe, expect, it } from "vitest";
import { existsSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import {
  type Baseline,
  colourTokens,
  cssDeclarations,
  formatViolation,
  lintCss,
  lintTsx,
  ratchet,
  SPACING_SCALE,
  UTILITIES,
} from "./design-lint";

const WEB = resolve(__dirname, "../..");
const SRC = resolve(__dirname, "..");
const BASELINE = resolve(__dirname, "design-lint.baseline.json");

function filesUnder(dir: string, match: RegExp, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) filesUnder(path, match, out);
    else if (match.test(path)) out.push(path);
  }
  return out.sort();
}

const tokensCss = readFileSync(resolve(__dirname, "tokens.css"), "utf8");
const tokens = colourTokens(tokensCss);
const rel = (path: string) => relative(WEB, path).split("\\").join("/");

const violations = [
  ...filesUnder(SRC, /\.css$/).flatMap((path) => lintCss(rel(path), readFileSync(path, "utf8"), { tokens })),
  ...filesUnder(SRC, /\.tsx$/).flatMap((path) => lintTsx(rel(path), readFileSync(path, "utf8"))),
];

describe("design lint", () => {
  it("keeps every file at or below its baseline", () => {
    const update = process.env.UPDATE_DESIGN_BASELINE === "1";
    const recorded: Baseline | null = existsSync(BASELINE)
      ? JSON.parse(readFileSync(BASELINE, "utf8"))
      : null;
    const { failures, next } = ratchet(recorded, violations, update);
    if (update) writeFileSync(BASELINE, `${JSON.stringify(next, null, 2)}\n`);
    expect(failures.join("\n\n")).toBe("");
  });

  it("prints every violation as path:line  rule  text  →  fix", () => {
    for (const v of violations) {
      expect(formatViolation(v)).toMatch(/^src\/\S+:\d+ {2}[a-z-]+ {2}\S.* {2}→ {2}\S/);
    }
  });
});

describe("the lint's own copy of the design system", () => {
  it("matches the spacing scale in tokens.css", () => {
    const scale = cssDeclarations(tokensCss)
      .filter((d) => /^--s\d+$/.test(d.prop))
      .map((d) => [Number(d.prop.slice(3)), parseFloat(d.value)] as const)
      .sort((a, b) => a[0] - b[0])
      .map(([, px]) => px);
    expect(scale).toEqual(SPACING_SCALE);
  });

  it.each([...UTILITIES].map(([decl, cls]) => [cls, decl]))(
    "suggests .%s only while components.css still declares %s",
    (cls, decl) => {
      const components = readFileSync(resolve(__dirname, "components.css"), "utf8");
      const rule = new RegExp(`\\.${cls}\\s*\\{([^}]*)\\}`).exec(components);
      expect(rule, `.${cls} missing from components.css`).not.toBeNull();
      const body = cssDeclarations(rule![1]).map((d) => `${d.prop}:${d.value.replace(/\s+/g, "")}`);
      if (decl === "color:var(--muted)") expect(body).toContain("color:var(--text-3)");
      else expect(body).toContain(decl);
    },
  );
});
