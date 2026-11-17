/**
 * hud.css geometry as text facts (jsdom does not evaluate media queries).
 * The numbers follow design_handoff_v3/screens/session-overlay-v3.html.
 */
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "hud.css"), "utf8");

/** Bodies of every `@media (max-width: <px>px) { ... }` block, braces matched. */
function mediaBodies(px: number): string[] {
  const out: string[] = [];
  const re = new RegExp(`@media \\(max-width: ${px}px\\)\\s*\\{`, "g");
  for (let m = re.exec(css); m; m = re.exec(css)) {
    let depth = 1;
    let i = m.index + m[0].length;
    const start = i;
    for (; i < css.length && depth > 0; i++) depth += css[i] === "{" ? 1 : css[i] === "}" ? -1 : 0;
    out.push(css.slice(start, i - 1));
  }
  return out;
}

describe("HUD Detailed stats grid narrows with the viewport (#1413)", () => {
  it("drops to two columns at 900px and one at 640px", () => {
    expect(mediaBodies(900).join("\n")).toMatch(/\.diag-grid\s*\{\s*grid-template-columns:\s*1fr 1fr;/);
    expect(mediaBodies(640).join("\n")).toMatch(/\.diag-grid\s*\{\s*grid-template-columns:\s*1fr;/);
  });
});
