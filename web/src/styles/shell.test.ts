/**
 * shell.css contract (v3).
 *
 * The shell is styled by class name, not by component: ConsoleShell/HomeShell
 * emit `.app` / `.topbar` / `.rail-item` and this sheet decides what they look
 * like. The numbers come from design_handoff_v3/screens/assets/console-v3.css
 * (console) and assets/quasar.css + home-v3.css (home topbar); see
 * docs/superpowers/specs/2026-08-28-ui-v3/handoff-v3-spec.md §A.0/§B/§F.
 */
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "shell.css"), "utf8");

/** Body of the first top-level rule for `sel` (anchored, so a light-theme or
 *  media-query-scoped twin cannot satisfy an assertion about the default). */
const rule = (sel: string): string => {
  const m = new RegExp(`^${sel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s*\\{`, "m").exec(css);
  expect(m, `missing rule ${sel}`).not.toBeNull();
  const i = m!.index;
  return css.slice(i, css.indexOf("}", i));
};

describe("v3 shell", () => {
  it("lays the console out as a rail x page grid under the topbar", () => {
    const app = rule(".app");
    expect(app).toMatch(/grid-template-columns:\s*var\(--rail-w\) 1fr/);
    expect(app).toMatch(/grid-template-rows:\s*var\(--topbar-h\) 1fr/);
    expect(app).toMatch(/height:\s*100vh/);
  });

  it("gives the home shell a 64px sticky topbar instead", () => {
    expect(rule(".topbar.home")).toMatch(/height:\s*64px/);
    expect(rule(".topbar.home")).toMatch(/position:\s*sticky/);
  });

  it("sizes the console topbar from the rail track", () => {
    expect(rule(".topbar")).toMatch(/grid-template-columns:\s*var\(--rail-w\) minmax\(0, 1fr\) auto/);
  });

  it("renders the wordmark in the brand face and hides it when collapsed", () => {
    expect(rule(".wordmark")).toMatch(/font-family:\s*var\(--font-brand\)/);
    expect(rule(".wordmark")).toMatch(/letter-spacing:\s*0\.2em/);
    expect(css).toMatch(/\[data-rail="collapsed"\] \.wordmark\s*\{[^}]*display:\s*none/);
  });

  it("gives rail rows the 36px height and the 2px accent edge bar", () => {
    expect(rule(".rail-item")).toMatch(/height:\s*36px/);
    expect(rule(".rail-item.active")).toMatch(/background:\s*var\(--accent-soft\)/);
    const edge = rule(".rail-item.active::before");
    expect(edge).toMatch(/left:\s*-10px/);
    expect(edge).toMatch(/width:\s*2px/);
  });

  it("hides labels, markers and section headings on a collapsed rail", () => {
    expect(css).toMatch(
      /\[data-rail="collapsed"\] \.rail-item \.lbl,\s*\[data-rail="collapsed"\] \.rail-item \.mk,\s*\[data-rail="collapsed"\] \.rail-sec\s*\{[^}]*display:\s*none/,
    );
  });

  it("pads the page column with the mock's rhythm", () => {
    expect(rule(".main")).toMatch(/padding:\s*var\(--s6\) var\(--page-pad\) var\(--s10\)/);
  });

  // Load-bearing, not decoration: `visibility: hidden` is what keeps every
  // .up-item (Sign out included) out of the Tab order while the menu is closed.
  it("hides the user popover until it is open", () => {
    expect(css).toMatch(/^\.user-pop\s*\{[^}]*visibility:\s*hidden/m);
    expect(css).toMatch(/^\.user-pop\s*\{[^}]*pointer-events:\s*none/m);
    expect(css).toMatch(/^\.user-pop\.open\s*\{[^}]*visibility:\s*visible/m);
    expect(css).toMatch(/^\.user-pop\.open\s*\{[^}]*pointer-events:\s*auto/m);
  });

  it("centres the home search and widens it on focus", () => {
    const s = rule(".topbar.home .search");
    expect(s).toMatch(/position:\s*absolute/);
    expect(s).toMatch(/width:\s*280px/);
    expect(rule(".topbar.home .search:focus-within")).toMatch(/width:\s*340px/);
  });

  it("swaps the pill row for the tab bar below 820px, never both", () => {
    expect(rule(".tabbar")).toMatch(/display:\s*none/);
    expect(css).toMatch(/@media \(max-width: 820px\)/);
    expect(css).toMatch(/\.topbar\.home \.nav\s*\{\s*display:\s*none/);
  });

  // web/ ships no autoprefixer: an unpaired backdrop-filter silently loses its
  // blur on the iPad class of device Quasar targets.
  it("pairs every backdrop-filter with its -webkit- twin", () => {
    const plain = (css.match(/[^-]backdrop-filter:/g) ?? []).length;
    const prefixed = (css.match(/-webkit-backdrop-filter:/g) ?? []).length;
    expect(prefixed).toBe(plain);
  });

  // The v3 ramp is authored in oklch end to end; a hex is a v1 survivor.
  it("is oklch-only — no hex colours", () => {
    expect(css).not.toMatch(/#[0-9a-f]{3,8}\b/i);
  });
});
