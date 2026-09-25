/**
 * The design-lint rules, as pure functions over source text.
 *
 * No filesystem here: `design-lint.test.ts` walks `src/` and feeds each file in,
 * `design-lint.rules.test.ts` feeds hand-written strings. Everything a rule
 * needs from the tree (the token values in tokens.css) arrives as an argument.
 */
import ts from "typescript";

export const RULES = [
  "spacing-scale",
  "raw-colour-css",
  "inline-style",
  "raw-colour-tsx",
  "design-lint-allow",
] as const;
export type Rule = (typeof RULES)[number];

export interface Violation {
  path: string;
  line: number;
  rule: Rule;
  text: string;
  fix: string;
}

export interface LintOptions {
  /** Normalised colour value → token name, from tokens.css (`colourTokens`). */
  tokens?: Map<string, string>;
}

export const formatViolation = (v: Violation): string =>
  `${v.path}:${v.line}  ${v.rule}  ${v.text}  →  ${v.fix}`;

/* ---------------------------------------------------------------- spacing */

/** `--s1`…`--s10` in px, in token order. Mirrors tokens.css (checked by the test). */
export const SPACING_SCALE: readonly number[] = [4, 8, 12, 16, 20, 24, 32, 40, 48, 64];
const REM_PX = 16;

const tokenFor = (px: number): string => `var(--s${SPACING_SCALE.indexOf(px) + 1})`;

/** The fix for one spacing length: the nearest token, both when it is a tie. */
export function spacingFix(px: number): string {
  const sign = px < 0 ? -1 : 1;
  const abs = Math.abs(px);
  const wrap = (token: string) => (sign < 0 ? `calc(${token} * -1)` : token);
  const show = (n: number) => `${sign * n}px`;
  if (abs < SPACING_SCALE[0]) {
    return `0 or ${wrap(tokenFor(SPACING_SCALE[0]))} (${show(SPACING_SCALE[0])}), whichever keeps the intent`;
  }
  let below = SPACING_SCALE[0];
  let above = SPACING_SCALE[SPACING_SCALE.length - 1];
  for (const step of SPACING_SCALE) {
    if (step <= abs) below = step;
    if (step >= abs) {
      above = step;
      break;
    }
  }
  if (abs > above) above = below;
  const dBelow = abs - below;
  const dAbove = above - abs;
  if (dBelow === dAbove && below !== above) {
    return `${wrap(tokenFor(below))} ${show(below)} or ${wrap(tokenFor(above))} ${show(above)}`;
  }
  const nearest = dBelow < dAbove ? below : above;
  return `${wrap(tokenFor(nearest))} (${show(nearest)})`;
}

const SPACING_PROP = /^(?:padding|margin)(?:-[a-z-]+)?$|^(?:row-|column-)?gap$/;
const LENGTH = /(?<![-\w.#])(-?(?:\d+\.?\d*|\.\d+))(px|rem|em)\b/g;

/* ---------------------------------------------------------------- colours */

const NAMED_COLOURS = new Set(
  (
    "aliceblue antiquewhite aqua aquamarine azure beige bisque black blanchedalmond blue " +
    "blueviolet brown burlywood cadetblue chartreuse chocolate coral cornflowerblue cornsilk " +
    "crimson cyan darkblue darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki " +
    "darkmagenta darkolivegreen darkorange darkorchid darkred darksalmon darkseagreen " +
    "darkslateblue darkslategray darkslategrey darkturquoise darkviolet deeppink deepskyblue " +
    "dimgray dimgrey dodgerblue firebrick floralwhite forestgreen fuchsia gainsboro ghostwhite " +
    "gold goldenrod gray green greenyellow grey honeydew hotpink indianred indigo ivory khaki " +
    "lavender lavenderblush lawngreen lemonchiffon lightblue lightcoral lightcyan " +
    "lightgoldenrodyellow lightgray lightgreen lightgrey lightpink lightsalmon lightseagreen " +
    "lightskyblue lightslategray lightslategrey lightsteelblue lightyellow lime limegreen linen " +
    "magenta maroon mediumaquamarine mediumblue mediumorchid mediumpurple mediumseagreen " +
    "mediumslateblue mediumspringgreen mediumturquoise mediumvioletred midnightblue mintcream " +
    "mistyrose moccasin navajowhite navy oldlace olive olivedrab orange orangered orchid " +
    "palegoldenrod palegreen paleturquoise palevioletred papayawhip peachpuff peru pink plum " +
    "powderblue purple rebeccapurple red rosybrown royalblue saddlebrown salmon sandybrown " +
    "seagreen seashell sienna silver skyblue slateblue slategray slategrey snow springgreen " +
    "steelblue tan teal thistle tomato turquoise violet wheat white whitesmoke yellow yellowgreen"
  ).split(" "),
);

const COLOUR_FN = /\b(?:rgba?|hsla?|hwb|oklch|oklab|lab|lch|color)\(/i;
const HEX = /#(?:[0-9a-f]{8}|[0-9a-f]{6}|[0-9a-f]{3,4})(?![0-9a-z_-])/gi;

/** Properties whose identifiers are names, not colours (`font-family: Tan`). */
const NON_COLOUR_PROP =
  /^(?:font-family|font|grid-area|grid-template-areas|grid-template|grid-row|grid-column|animation|animation-name|transition|transition-property|will-change|content|counter-reset|counter-increment|list-style|list-style-type|container|container-name|view-transition-name)$/;

const normaliseColour = (value: string): string =>
  value.toLowerCase().replace(/\s+/g, " ").replace(/\s*([(),/])\s*/g, "$1").trim();

/**
 * Colour-valued custom properties declared in tokens.css, keyed by normalised
 * value. The first declaration of a value wins, so `:root` beats the light
 * block. Feeds the "matches --x exactly" half of the raw-colour fix.
 */
export function colourTokens(tokensCss: string): Map<string, string> {
  const map = new Map<string, string>();
  for (const decl of cssDeclarations(tokensCss)) {
    if (!decl.prop.startsWith("--")) continue;
    const key = normaliseColour(decl.value);
    if (!map.has(key)) map.set(key, decl.prop);
  }
  return map;
}

const colourFix = (literal: string, tokens?: Map<string, string>): string => {
  const exact = tokens?.get(normaliseColour(literal));
  return exact
    ? `use var(${exact}) — it has exactly this value (src/styles/tokens.css)`
    : "use a token from src/styles/tokens.css";
};

/** Colour literals in a CSS value: hex, colour functions, named colours. */
function colourLiterals(value: string, prop: string): { text: string; index: number }[] {
  const found: { text: string; index: number }[] = [];
  // Quoted strings and url(...) are not colours, whatever they spell.
  const masked = value.replace(/"[^"]*"|'[^']*'|url\([^)]*\)/gi, (m) => " ".repeat(m.length));
  for (const m of masked.matchAll(HEX)) found.push({ text: m[0], index: m.index! });
  for (const m of masked.matchAll(/\b(rgba?|hsla?|hwb|oklch|oklab|lab|lch|color)\(/gi)) {
    const close = matchingParen(masked, m.index! + m[0].length - 1);
    found.push({ text: masked.slice(m.index!, close + 1), index: m.index! });
  }
  if (!NON_COLOUR_PROP.test(prop)) {
    for (const m of masked.matchAll(/(?<![-\w.#(])([a-z]+)(?![-\w(])/gi)) {
      // `in oklab` inside color-mix() names a space, and is not in the list anyway.
      if (NAMED_COLOURS.has(m[1].toLowerCase())) found.push({ text: m[1], index: m.index! });
    }
  }
  return found.sort((a, b) => a.index - b.index);
}

function matchingParen(s: string, open: number): number {
  let depth = 0;
  for (let i = open; i < s.length; i++) {
    if (s[i] === "(") depth++;
    else if (s[i] === ")" && --depth === 0) return i;
  }
  return s.length - 1;
}

/* ---------------------------------------------------------------- allows */

interface Allow {
  line: number;
  rule: string;
  reason: string;
  text: string;
}

const ALLOW = /design-lint-allow\b[ \t]*([\w-]*)[ \t]*(?::[ \t]*(.*?))?[ \t]*(?:\*\/|$)/;

/** Allow comments, located by the line they sit on. `comments` are [offset, text]. */
function allowsIn(source: string, comments: [number, string][]): Allow[] {
  const allows: Allow[] = [];
  for (const [offset, body] of comments) {
    const m = ALLOW.exec(body);
    if (!m) continue;
    const at = offset + m.index;
    allows.push({
      line: lineAt(source, at),
      rule: m[1],
      reason: (m[2] ?? "").trim(),
      text: m[0].replace(/\s*\*\/$/, "").trim(),
    });
  }
  return allows;
}

/**
 * Apply allows: each well-formed allow removes exactly one violation of its
 * rule on its own line or the next. A malformed allow suppresses nothing and
 * is itself reported.
 */
function applyAllows(path: string, violations: Violation[], allows: Allow[]): Violation[] {
  const sorted = [...violations].sort((a, b) => a.line - b.line);
  const suppressed = new Set<Violation>();
  const out: Violation[] = [];
  for (const allow of allows) {
    const known = (RULES as readonly string[]).includes(allow.rule) && allow.rule !== "design-lint-allow";
    if (!known || !allow.reason) {
      out.push({
        path,
        line: allow.line,
        rule: "design-lint-allow",
        text: allow.text,
        fix: !known
          ? `name one rule: ${RULES.filter((r) => r !== "design-lint-allow").join(", ")}`
          : `give a reason: design-lint-allow ${allow.rule}: <why this must stay>`,
      });
      continue;
    }
    const target = sorted.find(
      (v) =>
        !suppressed.has(v) &&
        v.rule === allow.rule &&
        (v.line === allow.line || v.line === allow.line + 1),
    );
    if (target) suppressed.add(target);
  }
  return [...sorted.filter((v) => !suppressed.has(v)), ...out].sort((a, b) => a.line - b.line);
}

const lineAt = (source: string, offset: number): number => {
  let line = 1;
  for (let i = 0; i < offset && i < source.length; i++) if (source.charCodeAt(i) === 10) line++;
  return line;
};

/* ---------------------------------------------------------------- CSS */

export interface CssDeclaration {
  prop: string;
  value: string;
  /** Offset of the value's first character in the original source. */
  valueOffset: number;
}

/** Replace comments with spaces, keeping every offset (and newline) in place. */
const blankComments = (css: string): string =>
  css.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "));

/**
 * Declarations in a stylesheet. A segment is split at `{`, `}` and `;` outside
 * parentheses and strings; one ended by `{` is a selector or at-rule prelude,
 * anything else with a `prop:` head is a declaration.
 */
export function cssDeclarations(css: string): CssDeclaration[] {
  const src = blankComments(css);
  const decls: CssDeclaration[] = [];
  let start = 0;
  let depth = 0;
  let quote: string | null = null;
  for (let i = 0; i <= src.length; i++) {
    const c = src[i];
    if (quote) {
      if (c === "\\") i++;
      else if (c === quote) quote = null;
      continue;
    }
    if (c === '"' || c === "'") quote = c;
    else if (c === "(") depth++;
    else if (c === ")") depth = Math.max(0, depth - 1);
    else if (depth === 0 && (c === "{" || c === "}" || c === ";" || c === undefined)) {
      if (c !== "{") {
        const segment = src.slice(start, i);
        const m = /^(\s*)(--[-\w]+|-?[a-zA-Z][-\w]*)\s*:/.exec(segment);
        if (m) {
          const head = m[0].length;
          const lead = segment.slice(head).match(/^\s*/)![0].length;
          decls.push({
            prop: m[2].toLowerCase().startsWith("--") ? m[2] : m[2].toLowerCase(),
            value: segment.slice(head + lead).trimEnd(),
            valueOffset: start + head + lead,
          });
        }
      }
      start = i + 1;
    }
  }
  return decls;
}

const cssComments = (css: string): [number, string][] =>
  [...css.matchAll(/\/\*[\s\S]*?\*\//g)].map((m) => [m.index!, m[0]]);

/** `spacing-scale` and `raw-colour-css` over one stylesheet. */
export function lintCss(path: string, css: string, options: LintOptions = {}): Violation[] {
  if (/(?:^|\/)tokens\.css$/.test(path)) return [];
  const violations: Violation[] = [];
  for (const decl of cssDeclarations(css)) {
    const shown = `${decl.prop}: ${decl.value.replace(/\s+/g, " ")}`;
    if (SPACING_PROP.test(decl.prop)) {
      for (const m of decl.value.matchAll(LENGTH)) {
        const n = parseFloat(m[1]);
        const px = m[2] === "px" ? n : n * REM_PX;
        if (px === 0 || Math.abs(px) === 1) continue;
        violations.push({
          path,
          line: lineAt(css, decl.valueOffset + m.index!),
          rule: "spacing-scale",
          text: shown,
          fix: `${m[0]}${m[2] === "px" ? "" : ` (${px}px)`} → ${spacingFix(px)}`,
        });
      }
    }
    for (const colour of colourLiterals(decl.value, decl.prop)) {
      violations.push({
        path,
        line: lineAt(css, decl.valueOffset + colour.index),
        rule: "raw-colour-css",
        text: `${decl.prop}: ${colour.text}`,
        fix: colourFix(colour.text, options.tokens),
      });
    }
  }
  return applyAllows(path, violations, allowsIn(css, cssComments(css)));
}

/* ---------------------------------------------------------------- TSX */

/** Keys an inline style may carry: values a stylesheet cannot know in advance. */
export const INLINE_STYLE_ALLOWLIST: ReadonlySet<string> = new Set([
  "width",
  "height",
  "minWidth",
  "minHeight",
  "maxWidth",
  "maxHeight",
  "left",
  "top",
  "right",
  "bottom",
  "transform",
  "opacity",
  "gridTemplateColumns",
]);

/**
 * The single-declaration utilities in components.css, keyed `cssProp:value`.
 * The scan test asserts every entry is still declared there.
 */
export const UTILITIES: ReadonlyMap<string, string> = new Map([
  ["flex-direction:column", "col"],
  ["justify-content:space-between", "between"],
  ["flex-wrap:wrap", "wrap"],
  ["flex:1", "grow"],
  ["align-items:center", "center"],
  ["gap:var(--s2)", "gap2"],
  ["gap:var(--s3)", "gap3"],
  ["gap:var(--s4)", "gap4"],
  ["gap:var(--s5)", "gap5"],
  ["gap:var(--s6)", "gap6"],
  ["margin-top:var(--s2)", "mt2"],
  ["margin-top:var(--s3)", "mt3"],
  ["margin-top:var(--s4)", "mt4"],
  ["margin-top:var(--s5)", "mt5"],
  ["margin-bottom:var(--s3)", "mb3"],
  ["margin-bottom:var(--s4)", "mb4"],
  ["margin-bottom:var(--s5)", "mb5"],
  ["margin-bottom:var(--s6)", "mb6"],
  ["white-space:nowrap", "nowrap"],
  ["text-align:right", "right"],
  ["color:var(--text-3)", "muted"],
  ["color:var(--muted)", "muted"],
  ["color:var(--text-2)", "sec"],
]);

const UNITLESS = new Set(["flex", "flexGrow", "flexShrink", "fontWeight", "lineHeight", "zIndex", "order", "opacity"]);

const kebab = (key: string): string => key.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`);

/** A style value as CSS text, or undefined when it is not a literal. */
function literalCss(key: string, init: ts.Expression): string | undefined {
  if (ts.isStringLiteral(init) || ts.isNoSubstitutionTemplateLiteral(init)) return init.text.trim();
  if (ts.isNumericLiteral(init)) return UNITLESS.has(key) || init.text === "0" ? init.text : `${init.text}px`;
  if (ts.isPrefixUnaryExpression(init) && init.operator === ts.SyntaxKind.MinusToken && ts.isNumericLiteral(init.operand)) {
    return `-${init.operand.text}px`;
  }
  return undefined;
}

/** A spacing literal already on the scale reads as its token (`12px` → `var(--s3)`). */
const asToken = (value: string): string => {
  const m = /^(\d+)px$/.exec(value);
  return m && SPACING_SCALE.includes(Number(m[1])) ? tokenFor(Number(m[1])) : value.replace(/\s+/g, "");
};

function inlineStyleFix(key: string, init: ts.Expression, siblings: Map<string, string | undefined>): string {
  const value = literalCss(key, init);
  if (value !== undefined) {
    if (key === "display" && value === "flex") {
      return siblings.get("flexDirection") === "column" ? `className "col"` : `className "row"`;
    }
    if (key === "alignItems" && value === "center" && siblings.get("display") === "flex") {
      return `className "row"`;
    }
    const utility = UTILITIES.get(`${kebab(key)}:${asToken(value)}`);
    if (utility) return `className "${utility}"`;
  }
  return "move to the component's stylesheet";
}

function propName(name: ts.PropertyName): string | undefined {
  if (ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNumericLiteral(name)) return name.text;
  // `["--x" as string]`, the usual way to type a custom property key.
  if (ts.isComputedPropertyName(name)) {
    let e = name.expression;
    while (ts.isAsExpression(e) || ts.isParenthesizedExpression(e)) e = e.expression;
    if (ts.isStringLiteral(e) || ts.isNoSubstitutionTemplateLiteral(e)) return e.text;
  }
  return undefined;
}

const COLOUR_KEY = /^(?:color|fill|stroke|stopColor|floodColor|lightingColor|background|backgroundColor|borderColor|border(?:Top|Right|Bottom|Left)Color|outlineColor|caretColor|accentColor|textDecorationColor)$/;
const COLOUR_ATTR = /^(?:color|fill|stroke|stop-color|stopColor|flood-color|floodColor|lighting-color|lightingColor)$/;

/** Is this literal text a CSS colour, or does it carry one (a gradient, a shadow)? */
function literalColour(text: string, colourContext: boolean): string | undefined {
  const t = text.trim();
  if (/^#(?:[0-9a-f]{3,4}|[0-9a-f]{6}|[0-9a-f]{8})$/i.test(t)) return t;
  if (COLOUR_FN.test(t)) return t;
  for (const m of t.matchAll(HEX)) {
    const hex = m[0];
    const before = t[m.index! - 1];
    // A bare number after `#` in prose ("issue #123") is not a colour; one with
    // a hex letter, or six/eight digits in CSS-ish position, is.
    const cssish = before === undefined || /[\s(,:]/.test(before);
    if (cssish && (/[a-f]/i.test(hex) || hex.length > 5)) return hex;
  }
  if (colourContext && NAMED_COLOURS.has(t.toLowerCase())) return t;
  return undefined;
}

/** `inline-style` and `raw-colour-tsx` over one component file. */
export function lintTsx(path: string, source: string): Violation[] {
  if (/\.test\.tsx$/.test(path)) return [];
  const file = ts.createSourceFile(path, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const violations: Violation[] = [];
  const line = (node: ts.Node) => file.getLineAndCharacterOfPosition(node.getStart(file)).line + 1;
  const clip = (s: string) => {
    const flat = s.replace(/\s+/g, " ");
    return flat.length > 60 ? `${flat.slice(0, 57)}...` : flat;
  };

  const checkStyle = (expr: ts.Expression) => {
    let inner = expr;
    while (ts.isParenthesizedExpression(inner) || ts.isAsExpression(inner) || ts.isSatisfiesExpression(inner)) {
      inner = inner.expression;
    }
    // `cond ? { … } : undefined` is still a literal on each branch.
    if (ts.isConditionalExpression(inner)) {
      for (const branch of [inner.whenTrue, inner.whenFalse]) {
        const empty = branch.kind === ts.SyntaxKind.NullKeyword || (ts.isIdentifier(branch) && branch.text === "undefined");
        if (!empty) checkStyle(branch);
      }
      return;
    }
    if (!ts.isObjectLiteralExpression(inner)) {
      violations.push({
        path,
        line: line(expr),
        rule: "inline-style",
        text: `style={${clip(expr.getText(file))}}`,
        fix: "not an object literal the lint can read — use classes, or a literal of allowlisted keys",
      });
      return;
    }
    const siblings = new Map<string, string | undefined>();
    for (const p of inner.properties) {
      if (ts.isPropertyAssignment(p)) {
        const k = propName(p.name);
        if (k) siblings.set(k, literalCss(k, p.initializer));
      }
    }
    for (const p of inner.properties) {
      const key =
        ts.isPropertyAssignment(p) || ts.isShorthandPropertyAssignment(p) ? propName(p.name) : undefined;
      if (key === undefined) {
        violations.push({
          path,
          line: line(p),
          rule: "inline-style",
          text: clip(p.getText(file)),
          fix: "not a key the lint can read — spell the keys out, allowlisted ones only",
        });
        continue;
      }
      if (INLINE_STYLE_ALLOWLIST.has(key) || key.startsWith("--")) continue;
      violations.push({
        path,
        line: line(p),
        rule: "inline-style",
        text: clip(p.getText(file)),
        fix: ts.isPropertyAssignment(p)
          ? inlineStyleFix(key, p.initializer, siblings)
          : "move to the component's stylesheet",
      });
    }
  };

  const colourContextOf = (node: ts.Node): boolean => {
    const parent = node.parent;
    if (ts.isPropertyAssignment(parent) && parent.initializer === node) {
      const k = propName(parent.name);
      return k !== undefined && COLOUR_KEY.test(k);
    }
    if (ts.isJsxAttribute(parent)) return COLOUR_ATTR.test(parent.name.getText(file));
    return false;
  };

  const checkLiteral = (node: ts.Node, text: string) => {
    const colour = literalColour(text, colourContextOf(node));
    if (colour === undefined) return;
    violations.push({
      path,
      line: line(node),
      rule: "raw-colour-tsx",
      text: clip(node.getText(file)),
      fix: "use a token from src/styles/tokens.css (var(--…) in a class, or a --custom-property)",
    });
  };

  const visit = (node: ts.Node) => {
    if (ts.isJsxAttribute(node) && node.name.getText(file) === "style" && node.initializer) {
      if (ts.isJsxExpression(node.initializer) && node.initializer.expression) {
        checkStyle(node.initializer.expression);
      } else {
        violations.push({
          path,
          line: line(node),
          rule: "inline-style",
          text: clip(node.getText(file)),
          fix: "not an object literal the lint can read — use classes",
        });
      }
    }
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) {
      checkLiteral(node, node.text);
    } else if (ts.isTemplateExpression(node)) {
      const parts = [node.head.text, ...node.templateSpans.map((s) => s.literal.text)].join(" ${} ");
      checkLiteral(node, parts);
    }
    ts.forEachChild(node, visit);
  };
  visit(file);

  const comments: [number, string][] = [];
  // Comments are trivia; ask the file for each node's leading and trailing ranges.
  const seen = new Set<number>();
  const collect = (node: ts.Node) => {
    for (const ranges of [
      ts.getLeadingCommentRanges(source, node.getFullStart()),
      ts.getTrailingCommentRanges(source, node.getEnd()),
    ]) {
      for (const r of ranges ?? []) {
        if (seen.has(r.pos)) continue;
        seen.add(r.pos);
        comments.push([r.pos, source.slice(r.pos, r.end)]);
      }
    }
    ts.forEachChild(node, collect);
  };
  collect(file);
  // JSX comments (`{/* … */}`) are an empty expression's trivia and are not
  // reached above; pick them up from the raw text.
  for (const m of source.matchAll(/\{\s*\/\*[\s\S]*?\*\/\s*\}/g)) {
    const at = source.indexOf("/*", m.index!);
    if (!seen.has(at)) comments.push([at, m[0]]);
  }
  return applyAllows(path, violations, allowsIn(source, comments));
}

/* ---------------------------------------------------------------- ratchet */

/** Violation counts per file, per rule. Zero counts are never stored. */
export type Baseline = Record<string, Partial<Record<Rule, number>>>;

export const UPDATE_COMMAND = "UPDATE_DESIGN_BASELINE=1 npm run lint:design";

export interface RatchetResult {
  failures: string[];
  /** The baseline to write in update mode: counts lowered, never raised. */
  next: Baseline;
}

/**
 * Compare this run's violations with the recorded baseline.
 *
 * `baseline === null` means no baseline file exists yet: in update mode that
 * seeds one from the current counts (the only way a count is ever set rather
 * than lowered); otherwise it is a failure.
 */
export function ratchet(baseline: Baseline | null, violations: Violation[], update: boolean): RatchetResult {
  const counts: Baseline = {};
  const sites = new Map<string, Violation[]>();
  for (const v of violations) {
    const perFile = (counts[v.path] ??= {});
    perFile[v.rule] = (perFile[v.rule] ?? 0) + 1;
    const key = `${v.path}\0${v.rule}`;
    if (!sites.has(key)) sites.set(key, []);
    sites.get(key)!.push(v);
  }
  if (baseline === null) {
    return update
      ? { failures: [], next: sortBaseline(counts) }
      : { failures: [`no baseline file — run ${UPDATE_COMMAND} to create it`], next: {} };
  }

  const failures: string[] = [];
  const next: Baseline = {};
  const files = [...new Set([...Object.keys(baseline), ...Object.keys(counts)])].sort();
  for (const file of files) {
    const recorded = baseline[file];
    for (const rule of RULES) {
      const was = recorded?.[rule] ?? 0;
      const now = counts[file]?.[rule] ?? 0;
      const list = () =>
        (sites.get(`${file}\0${rule}`) ?? []).map((v) => `    ${formatViolation(v)}`).join("\n");
      let keep = was;
      if (now > was) {
        const head = !recorded
          ? `${file}: new file with ${now} ${rule} violation(s) — fix them, or add a reasoned design-lint-allow`
          : `${file}: ${rule} rose from ${was} to ${now} — fix the new site(s), or add a reasoned design-lint-allow`;
        const tail = update ? `\n  ${UPDATE_COMMAND} only lowers counts; refusing to raise this one.` : "";
        failures.push(`${head}${tail}\n${list()}`);
      } else if (now < was) {
        if (update) keep = now;
        else failures.push(`${was - now} fixed in ${file} (${rule}) — run \`${UPDATE_COMMAND}\` to lock it in`);
      }
      if (keep > 0) (next[file] ??= {})[rule] = keep;
    }
  }
  return { failures, next: sortBaseline(next) };
}

function sortBaseline(b: Baseline): Baseline {
  const out: Baseline = {};
  for (const file of Object.keys(b).sort()) {
    const rules: Partial<Record<Rule, number>> = {};
    for (const rule of RULES) if (b[file][rule]) rules[rule] = b[file][rule];
    if (Object.keys(rules).length) out[file] = rules;
  }
  return out;
}
