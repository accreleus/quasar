/**
 * fleet.css contract — the hosts drawer.
 *
 * `.qtable tbody td` is `white-space: nowrap` (primitives.css) so a collapsed
 * row's cells stay on one line. primitives lifts that only for content inside
 * `.exp-in`, but the readiness card renders in `.exp-readiness`, a sibling of
 * `.exp-in` (HostExpansion.tsx) — so every check summary and remediation line
 * inherited `nowrap` from the `<td>` and ran off the card's right edge and off
 * the viewport (#82). The reset belongs on the container: `white-space` is
 * inherited, so one declaration covers the whole card, and a descendant that
 * genuinely wants `nowrap` still says so itself.
 */
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "fleet.css"), "utf8");

/** The body of the first top-level rule for `sel`, anchored to a line start. */
const rule = (sel: string): string => {
  const m = new RegExp(`^${sel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s*[,{]`, "m").exec(css);
  expect(m, `missing rule ${sel}`).not.toBeNull();
  const i = m!.index;
  return css.slice(i, css.indexOf("}", i));
};

describe("hosts drawer", () => {
  it("lets readiness text wrap inside the drawer's table cell", () => {
    expect(rule(".hosts-page .exp-readiness")).toMatch(/white-space:\s*normal/);
  });

  it("breaks a long remediation token rather than overflowing the card", () => {
    expect(rule(".hosts-page .exp-readiness code")).toMatch(/overflow-wrap:\s*anywhere/);
  });
});
