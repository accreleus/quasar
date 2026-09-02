/**
 * components.css contract — the readiness rows.
 *
 * ReadinessCard renders each check into a grid item: `.host-setting-row` in the
 * list layout, `.readiness-check` in the grid one. A grid item's default
 * `min-width: auto` floors the track at the content's min-content width, so a
 * single unbreakable token in a remediation command widens the track past the
 * card and spills (#82). Both layouts need the floor removed, not just the grid
 * one.
 */
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "components.css"), "utf8");

/** The body of the first top-level rule for `sel`, anchored to a line start. */
const rule = (sel: string): string => {
  const m = new RegExp(`^${sel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s*[,{]`, "m").exec(css);
  expect(m, `missing rule ${sel}`).not.toBeNull();
  const i = m!.index;
  return css.slice(i, css.indexOf("}", i));
};

describe("readiness check rows", () => {
  it("removes the grid-item min-width floor in both layouts", () => {
    expect(rule(".host-setting-copy")).toMatch(/min-width:\s*0/);
  });

  it("breaks inside a long summary word rather than overflowing the row", () => {
    expect(rule(".host-setting-copy p")).toMatch(/overflow-wrap:\s*anywhere/);
  });
});
