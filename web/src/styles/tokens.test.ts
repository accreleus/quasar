import { describe, expect, it } from "vitest";
import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

const stylesDir = __dirname;
const srcDir = resolve(__dirname, "..");

const css = readFileSync(resolve(stylesDir, "tokens.css"), "utf8");
const base = readFileSync(resolve(stylesDir, "base.css"), "utf8");

function escapeRegExp(literal: string): string {
  return literal.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * The body of the first `:root { … }` block, found by matching braces rather
 * than by slicing up to whatever selector happens to come next — the previous
 * version broke the moment a block was reordered.
 */
function rootBlock(sheet: string): string {
  const start = sheet.indexOf(":root");
  const open = sheet.indexOf("{", start);
  let depth = 0;
  for (let i = open; i < sheet.length; i++) {
    if (sheet[i] === "{") depth++;
    else if (sheet[i] === "}" && --depth === 0) return sheet.slice(open + 1, i);
  }
  throw new Error("tokens.css: unterminated :root block");
}

describe("v3 token contract", () => {
  it.each([
    ["--surf-canvas", "oklch(0.115 0.012 267)"],
    ["--surf-chrome", "oklch(0.163 0.018 268)"],
    ["--surf-panel", "oklch(0.208 0.020 268)"],
    ["--surf-raised", "oklch(0.252 0.022 267)"],
    ["--surf-control", "oklch(0.292 0.022 265)"],
    ["--surf-inset", "oklch(0.088 0.012 267)"],
    ["--accent", "oklch(0.5998 0.2179 273.21)"],
    ["--action", "oklch(0.5 0.19 273.21)"],
    ["--accent-text", "oklch(0.78 0.13 273.21)"],
    ["--teal", "oklch(0.78 0.11 196)"],
    ["--r-control", "4px"],
    ["--r-panel", "8px"],
    ["--r-feature", "12px"],
    ["--row-h", "44px"],
    ["--control-h", "34px"],
    ["--rail-w", "216px"],
    ["--topbar-h", "56px"],
  ])("defines %s as %s in :root", (name: string, value: string) => {
    // Anchored on the `;` so a longer value that merely starts with the
    // expected one cannot pass.
    const decl = new RegExp(`${escapeRegExp(name)}:\\s*${escapeRegExp(value)}(?=\\s*;)`);
    expect(rootBlock(css)).toMatch(decl);
  });

  it("collapses the retired spectrum onto the accent", () => {
    expect(css).toMatch(/--violet:\s*var\(--accent\)/);
    expect(css).toMatch(/--cyan:\s*var\(--accent\)/);
  });

  // The v3 ramp is authored in oklch end to end. A hex here is either a
  // survivor of the v1 palette or a colour picked outside the contract.
  it("is oklch-only — no hex colours", () => {
    expect(css).not.toMatch(/#[0-9a-f]{3,8}\b/i);
  });

  it("keeps the dense and collapsed variants", () => {
    expect(css).toMatch(/\[data-density="dense"\][^}]*--row-h:\s*34px/);
    expect(css).toMatch(/\[data-rail="collapsed"\][^}]*--rail-w:\s*60px/);
  });

  it("names the v3 font stacks", () => {
    expect(css).toMatch(/--font-brand:\s*'Michroma'/);
    expect(css).toMatch(/--font-display:\s*'IBM Plex Sans'/);
    expect(css).toMatch(/--font-mono:\s*'IBM Plex Mono'/);
    expect(base).toMatch(/@fontsource\/michroma/);
    expect(base).toMatch(/@fontsource\/ibm-plex-sans\/600\.css/);
    expect(base).not.toMatch(/space-grotesk|hanken|jetbrains/);
  });
});

/**
 * Custom properties that nothing defines. A `var(--gone)` with no fallback
 * collapses to nothing — the declaration is simply dropped, silently, which is
 * exactly the failure a token-layer rewrite causes and exactly the failure no
 * screenshot catches. These five predate the v3 port and live in styles.css.
 *
 * This list may shrink, never grow. Deleting a token whose last use is still
 * out there is the mistake it exists to catch.
 */
const KNOWN_DANGLING = ["--p", "--r", "--r2", "--r3", "--surface-1"];

function walk(dir: string, match: RegExp): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = resolve(dir, entry.name);
    if (entry.isDirectory()) return walk(full, match);
    return match.test(entry.name) ? [full] : [];
  });
}

describe("every var() in web/src resolves", () => {
  it("defines every custom property the stylesheets read", () => {
    const sheets = walk(srcDir, /\.css$/);
    // Components set a handful of properties from TSX (inline style objects,
    // setProperty) rather than from a stylesheet; those count as defined.
    // Test files are excluded — a name quoted in an assertion is not a
    // definition, and counting it would let this test satisfy itself.
    const code = walk(srcDir, /\.tsx?$/).filter((f) => !/\.test\.tsx?$/.test(f));

    const defined = new Set<string>();
    for (const file of sheets) {
      const text = readFileSync(file, "utf8");
      for (const [, name] of text.matchAll(/(?:^|[;{,)\s])(--[A-Za-z0-9_-]+)\s*:/g)) {
        defined.add(name);
      }
    }
    for (const file of code) {
      const text = readFileSync(file, "utf8");
      for (const [, name] of text.matchAll(/["'](--[A-Za-z0-9_-]+)["']/g)) defined.add(name);
    }

    const used = new Set<string>();
    for (const file of sheets) {
      const text = readFileSync(file, "utf8");
      for (const [, name] of text.matchAll(/var\(\s*(--[A-Za-z0-9_-]+)/g)) used.add(name);
    }

    const dangling = [...used].filter((name) => !defined.has(name)).sort();
    expect(dangling.filter((name) => !KNOWN_DANGLING.includes(name))).toEqual([]);
  });
});
