/**
 * primitives.css contract (v3).
 *
 * The shared primitives are styled by class name, not by component: the React
 * tree emits `.btn` / `.chip` / `.qtable` and this sheet decides what they look
 * like. That makes the geometry a text fact about one file, which is what these
 * assertions pin — the numbers all come from
 * design_handoff_v3/screens/assets/console-v3.css (see
 * docs/superpowers/specs/2026-08-28-ui-v3/handoff-v3-spec.md section F).
 */
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "primitives.css"), "utf8");

/**
 * The body of the first top-level rule for `sel`. Anchored to the start of a
 * line: an unanchored search matches `[data-theme="light"] .card {` and
 * `.u2 .bar {` too, so a light-theme or scoped rule could otherwise satisfy an
 * assertion about the dark default.
 */
const rule = (sel: string): string => {
  const m = new RegExp(`^${sel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s*\\{`, "m").exec(css);
  expect(m, `missing rule ${sel}`).not.toBeNull();
  const i = m!.index;
  return css.slice(i, css.indexOf("}", i));
};

describe("v3 primitives", () => {
  it("buttons use control height and 4px radius", () => {
    expect(rule(".btn")).toMatch(/height:\s*var\(--control-h\)/);
    expect(rule(".btn")).toMatch(/border-radius:\s*var\(--r-control\)/);
    expect(rule(".btn-primary")).toMatch(/background:\s*var\(--action\)/);
    expect(rule(".btn-sm")).toMatch(/height:\s*28px/);
  });

  // A `<td class="cell-actions">` that stays `display: flex` leaves the table
  // box model, and the row's border-bottom and hover draw a seam down the
  // column (HostRow, JobsTab both put the class on the cell).
  it("keeps an actions cell inside the table box model", () => {
    expect(rule(".cell-actions")).toMatch(/display:\s*flex/);
    const cell = rule("td.cell-actions");
    expect(cell).toMatch(/display:\s*table-cell/);
    expect(cell).toMatch(/text-align:\s*right/);
    expect(cell).toMatch(/white-space:\s*nowrap/);
  });

  it("chips are 21px mono uppercase", () => {
    const c = rule(".chip");
    expect(c).toMatch(/height:\s*21px/);
    expect(c).toMatch(/font-family:\s*var\(--font-mono\)/);
    expect(c).toMatch(/text-transform:\s*uppercase/);
  });

  it("tables, tabs, switches, segmented match the contract", () => {
    expect(rule(".qtable thead th")).toMatch(/height:\s*34px/);
    expect(rule(".qtable tbody td")).toMatch(/height:\s*var\(--row-h\)/);
    expect(rule(".tab")).toMatch(/height:\s*36px/);
    expect(rule(".switch")).toMatch(/width:\s*34px/);
    expect(rule(".switch")).toMatch(/height:\s*19px/);
    expect(rule(".segmented")).toMatch(/border-radius:\s*var\(--r-panel\)/);
  });

  it("capacity encodings read teal, not violet", () => {
    expect(rule(".bar")).toMatch(/--accent:\s*var\(--teal\)/);
    expect(rule(".gauge")).toMatch(/--accent:\s*var\(--teal\)/);
  });

  it("note has the 2px accent left rule and warn variant", () => {
    expect(rule(".note")).toMatch(/border-left:\s*2px solid var\(--accent\)/);
    expect(css).toMatch(/\.note\.warn\s*\{[^}]*border-left-color:\s*var\(--warning\)/);
  });

  // The depth pass is what gives a card an edge against the canvas; --shadow-md
  // stays the lighter global value for everything else.
  it("card carries the depth-pass shadow stack", () => {
    const c = rule(".card");
    expect(c).toMatch(/inset 0 1px 0 var\(--glass-highlight\)/);
    expect(c).toMatch(/0 2px 4px oklch\(0\.02 0\.01 267\/\.45\)/);
    expect(c).toMatch(/0 18px 44px -28px oklch\(0\.02 0\.01 267\/\.85\)/);
    expect(c).not.toMatch(/var\(--shadow-md\)/);
  });

  it("table head sits on the raised surface step", () => {
    expect(rule(".qtable thead th")).toMatch(/background:\s*color-mix\(in oklch,\s*var\(--surf-raised\)/);
  });

  // One owner per class. The shell chrome moved to shell.css when the v3
  // shell landed; a rule creeping back here would out-order (or be
  // out-ordered by) its twin depending on the import order in main.tsx.
  // `.icon-btn`, `.menu`, `.pal*`, `.scrim*` and `.search` stay: pages use them.
  it("no longer owns the shell chrome", () => {
    for (const sel of [".cmdk", ".user-btn", ".user-pop", ".up-item", ".mk", ".u-ava"]) {
      expect(css, `${sel} belongs to shell.css`).not.toMatch(
        new RegExp(`(^|,\\s*)\\${sel}[\\s,{:.]`, "m"),
      );
    }
    expect(css).toMatch(/^\.menu\s*\{/m);
    expect(css).toMatch(/^\.pal\s*\{/m);
  });

  it("offers the top-docked scrim the command palette needs", () => {
    const s = rule(".scrim-top");
    expect(s).toMatch(/place-items:\s*start center/);
    expect(s).toMatch(/padding-top:\s*12vh/);
  });

  it("ships the light appearance", () => {
    expect(css).toMatch(/\[data-theme="light"\]\s*\.card\s*\{/);
  });

  // The v3 ramp is authored in oklch end to end; a hex is a v1 survivor.
  it("is oklch-only — no hex colours", () => {
    expect(css).not.toMatch(/#[0-9a-f]{3,8}\b/i);
  });
});
