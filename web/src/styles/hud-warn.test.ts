/**
 * #1136: `.diag-warn` rows read amber. The bare class in styles.css loses to the
 * HUD's cell colour rules on specificity, so the winning rule has to live in
 * hud.css after them (jsdom ignores specificity, so this is a text fact).
 */
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "hud.css"), "utf8");

describe("HUD diag-warn rows (#1136)", () => {
  it("colours both tables' warn cells --warning-text, after their cell rules", () => {
    const m = /\.diag-grid td\.diag-warn,\s*\.diag-extra \.diag-table td\.diag-warn\s*\{\s*color:\s*var\(--warning-text\);\s*\}/.exec(css);
    expect(m).not.toBeNull();
    for (const cell of [".diag-grid td:first-child", ".diag-grid td:last-child", ".diag-extra .diag-table td:first-child", ".diag-extra .diag-table td:last-child"]) {
      expect(css.lastIndexOf(cell), cell).toBeLessThan(m!.index);
    }
  });
});
