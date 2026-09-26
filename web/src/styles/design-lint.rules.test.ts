/** Unit tests for the design-lint rules: strings in, violations out, no filesystem. */
import { describe, expect, it } from "vitest";
import {
  type Baseline,
  colourTokens,
  formatViolation,
  lintCss,
  lintTsx,
  ratchet,
  spacingFix,
  UPDATE_COMMAND,
  type Violation,
} from "./design-lint";

const css = (body: string, path = "src/styles/x.css") => lintCss(path, `.a {\n  ${body}\n}\n`);
const rules = (vs: Violation[]) => vs.map((v) => v.rule);
const tsx = (body: string, path = "src/X.tsx") => lintTsx(path, body);

describe("spacing-scale", () => {
  it("flags an off-scale length with the nearest token", () => {
    const vs = css("padding: 11px;");
    expect(vs).toHaveLength(1);
    expect(vs[0].rule).toBe("spacing-scale");
    expect(vs[0].fix).toContain("var(--s3)");
    expect(vs[0].line).toBe(2);
  });

  it.each(["padding: var(--s3);", "margin: 0 auto;", "gap: 1px;", "margin: calc(var(--s2) * -1);", "margin: -1px;", "padding: 0px;", "padding: 50%;"])(
    "passes %s",
    (decl) => expect(css(decl)).toEqual([]),
  );

  it("fixes each value of a shorthand separately", () => {
    const vs = css("padding: 10px 14px;");
    expect(vs.map((v) => v.fix)).toEqual([
      "10px → var(--s2) 8px or var(--s3) 12px",
      "14px → var(--s3) 12px or var(--s4) 16px",
    ]);
  });

  it("converts rem at 16px and snaps it", () => {
    const [v] = css("gap: 0.75rem;");
    expect(v.fix).toBe("0.75rem (12px) → var(--s3) (12px)");
  });

  it("flags a length inside calc() that is not a token", () => {
    expect(rules(css("margin: calc(var(--s2) + 3px);"))).toEqual(["spacing-scale"]);
  });

  it("negates the token for a negative length", () => {
    expect(css("margin-top: -12px;")[0].fix).toBe("-12px → calc(var(--s3) * -1) (-12px)");
  });

  it("offers 0 or the smallest step under 4px", () => {
    expect(spacingFix(2)).toMatch(/^0 or var\(--s1\)/);
  });

  it("ignores a value inside a comment", () => {
    expect(css("/* padding: 11px; */ color: var(--text);")).toEqual([]);
  });

  it("ignores non-spacing properties", () => {
    expect(css("top: 11px; font-size: 13px; border-radius: 6px;")).toEqual([]);
  });

  it("skips tokens.css", () => {
    expect(css("padding: 11px;", "src/styles/tokens.css")).toEqual([]);
  });
});

describe("raw-colour-css", () => {
  it.each([
    "color: #fff;",
    "box-shadow: 0 0 0 1px rgb(0 0 0 / 22%);",
    "background: linear-gradient(90deg, oklch(0.5 0.1 270), var(--accent));",
    "--glow: oklch(0.7 0.1 270);",
    "color: white;",
  ])("flags %s", (decl) => {
    expect(rules(css(decl))).toEqual(["raw-colour-css"]);
  });

  it.each(["color: var(--text);", "background: transparent;", "color: currentColor;", "color: inherit;", "font-family: Tan, sans-serif;", 'content: "#fff";'])(
    "passes %s",
    (decl) => expect(css(decl)).toEqual([]),
  );

  it("does not lint tokens.css", () => {
    expect(css("--x: #fff; color: rgb(0 0 0);", "src/styles/tokens.css")).toEqual([]);
  });

  it("names the token whose value matches exactly", () => {
    const tokens = colourTokens(":root { --line: oklch(0.9618 0.0086 247.91/.09); }");
    const [v] = lintCss("src/a.css", ".a { border-color: oklch(0.9618 0.0086 247.91 / .09); }", { tokens });
    expect(v.fix).toContain("var(--line)");
  });
});

describe("inline-style", () => {
  it("names the utility for a token margin", () => {
    const vs = tsx(`export const A = () => <div style={{ marginTop: "var(--s3)" }} />;`);
    expect(vs).toHaveLength(1);
    expect(vs[0].rule).toBe("inline-style");
    expect(vs[0].fix).toBe(`className "mt3"`);
  });

  it("suggests row for flex + centre, muted for text-3", () => {
    const vs = tsx(`const A = () => <div style={{ display: "flex", alignItems: "center", color: "var(--text-3)" }} />;`);
    expect(vs.map((v) => v.fix)).toEqual([`className "row"`, `className "row"`, `className "muted"`]);
  });

  it("falls back to the component stylesheet", () => {
    expect(tsx(`const A = () => <p style={{ fontSize: 13 }} />;`)[0].fix).toBe("move to the component's stylesheet");
  });

  it.each([
    "<div style={{ width: `${pct}%` }} />",
    `<div style={{ "--bar": x }} />`,
    `<div style={{ ["--bar" as string]: x }} />`,
    "<div style={on ? { opacity: 0.5 } : undefined} />",
  ])("passes %s", (jsx) => {
    expect(tsx(`const A = () => ${jsx};`)).toEqual([]);
  });

  it("flags only the disallowed key in a mixed object", () => {
    const vs = tsx(`const A = () => <div style={{ width: 10, gap: 4, left: 0 }} />;`);
    expect(vs.map((v) => v.text)).toEqual(["gap: 4"]);
  });

  it("flags a style it cannot read as an object literal", () => {
    const vs = tsx(`const A = () => <div style={styles} />;`);
    expect(vs).toHaveLength(1);
    expect(vs[0].text).toBe("style={styles}");
  });

  it("skips *.test.tsx", () => {
    expect(tsx(`const A = () => <div style={{ margin: 3 }} />;`, "src/X.test.tsx")).toEqual([]);
  });
});

describe("raw-colour-tsx", () => {
  it("flags an SVG fill and a style colour", () => {
    expect(rules(tsx(`const A = () => <path fill="#8b5cf6" />;`))).toEqual(["raw-colour-tsx"]);
    expect(rules(tsx(`const s = { color: "#fff" };`))).toEqual(["raw-colour-tsx"]);
  });

  it("flags a colour carried in a longer literal", () => {
    expect(rules(tsx("const s = `0 0 4px rgba(0,0,0,${a})`;"))).toEqual(["raw-colour-tsx"]);
  });

  it("ignores comments and prose", () => {
    expect(tsx(`// "#123" is fine here\nconst s = "issue #12"; const t = "see #123";`)).toEqual([]);
  });
});

describe("allow comments", () => {
  it("suppresses exactly one violation of the named rule, on its line or the next", () => {
    const vs = lintCss(
      "src/a.css",
      ".a {\n  /* design-lint-allow spacing-scale: optical centre */\n  padding: 3px 3px;\n  color: #fff;\n}\n",
    );
    expect(rules(vs)).toEqual(["spacing-scale", "raw-colour-css"]);
  });

  it("does not suppress another rule", () => {
    const vs = lintCss("src/a.css", ".a { color: #000; /* design-lint-allow spacing-scale: nope */ }");
    expect(rules(vs)).toEqual(["raw-colour-css"]);
  });

  it("works as a // comment in TSX, and a JSX comment", () => {
    expect(tsx(`// design-lint-allow raw-colour-tsx: brand mark\nconst c = "#6A45F5";`)).toEqual([]);
    expect(
      tsx(`const A = () => (\n  <div>\n    {/* design-lint-allow inline-style: measured */}\n    <p style={{ margin: 3 }} />\n  </div>\n);`),
    ).toEqual([]);
  });

  it("reports an allow with no reason, and it suppresses nothing", () => {
    const vs = lintCss("src/a.css", ".a {\n  /* design-lint-allow spacing-scale: */\n  padding: 3px;\n}");
    expect(rules(vs)).toEqual(["design-lint-allow", "spacing-scale"]);
    expect(vs[0].fix).toMatch(/reason/);
  });

  it("reports an allow naming no known rule", () => {
    expect(rules(lintCss("src/a.css", "/* design-lint-allow spacing: why */"))).toEqual(["design-lint-allow"]);
  });
});

describe("ratchet", () => {
  const v = (path: string, line: number): Violation => ({
    path,
    line,
    rule: "spacing-scale",
    text: "padding: 3px",
    fix: "3px → var(--s1)",
  });
  const base: Baseline = { "src/a.css": { "spacing-scale": 2 } };

  it("passes at the baseline", () => {
    expect(ratchet(base, [v("src/a.css", 1), v("src/a.css", 2)], false).failures).toEqual([]);
  });

  it("fails above the baseline and lists the sites", () => {
    const { failures } = ratchet(base, [v("src/a.css", 1), v("src/a.css", 2), v("src/a.css", 9)], false);
    expect(failures).toHaveLength(1);
    expect(failures[0]).toContain("rose from 2 to 3");
    expect(failures[0]).toContain("src/a.css:9  spacing-scale");
  });

  it("fails below the baseline until it is locked in", () => {
    const { failures } = ratchet(base, [v("src/a.css", 1)], false);
    expect(failures).toEqual([`1 fixed in src/a.css (spacing-scale) — run \`${UPDATE_COMMAND}\` to lock it in`]);
  });

  it("fails a new file with violations", () => {
    const { failures } = ratchet(base, [v("src/a.css", 1), v("src/a.css", 2), v("src/b.css", 4)], false);
    expect(failures[0]).toMatch(/^src\/b\.css: new file/);
  });

  it("lowers counts in update mode and drops zeroes", () => {
    const r = ratchet({ ...base, "src/gone.css": { "spacing-scale": 1 } }, [v("src/a.css", 1)], true);
    expect(r.failures).toEqual([]);
    expect(r.next).toEqual({ "src/a.css": { "spacing-scale": 1 } });
  });

  it("refuses to raise a count in update mode", () => {
    const r = ratchet(base, [v("src/a.css", 1), v("src/a.css", 2), v("src/a.css", 3)], true);
    expect(r.failures[0]).toContain("refusing to raise");
    expect(r.next).toEqual(base);
  });

  it("seeds a missing baseline only in update mode", () => {
    expect(ratchet(null, [v("src/a.css", 1)], true).next).toEqual({ "src/a.css": { "spacing-scale": 1 } });
    expect(ratchet(null, [], false).failures).toHaveLength(1);
  });
});

describe("message shape", () => {
  it("has path:line, the rule, the text and a non-empty fix", () => {
    const all = [
      ...css("padding: 11px; color: #fff;"),
      ...tsx(`const A = () => <div style={{ gap: 3 }}><path fill="#abc" /></div>;`),
      ...lintCss("src/a.css", "/* design-lint-allow spacing-scale: */"),
    ];
    expect(new Set(rules(all))).toEqual(
      new Set(["spacing-scale", "raw-colour-css", "inline-style", "raw-colour-tsx", "design-lint-allow"]),
    );
    for (const x of all) {
      expect(formatViolation(x)).toMatch(new RegExp(`^${x.path}:\\d+  ${x.rule}  \\S.*  →  \\S`));
    }
  });
});
