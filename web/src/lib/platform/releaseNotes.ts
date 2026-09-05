/**
 * A GitHub Release body, turned into the rows the Releases tab draws.
 *
 * The publish workflow takes a release's body from the changelog section, so
 * the body is Keep-a-Changelog: `### Security | Added | Fixed | Removed |
 * Changed`, then one top-level bullet per change. The v3 Releases mock draws
 * that as a table of rows — a category tag, a one-line title, the issues the
 * change closed, and a disclosure holding the rest — so the markdown has to
 * become structure before it becomes markup. That parse is here, pure and
 * tested against the real rc.1 / rc.2 bodies, rather than inside the component.
 *
 * UNTRUSTED UPSTREAM TEXT (control-api.md): nothing here escapes or sanitises.
 * The title renders as a text node and the detail goes through <Markdown>,
 * which sanitises; this module only decides which characters are which field.
 */

/** The changelog headings the publish workflow emits. Anything else is kept
 *  verbatim as its own category rather than dropped — an unknown heading is a
 *  changelog we have not seen, not a reason to lose its bullets. */
export type Category = "Security" | "Added" | "Fixed" | "Removed" | "Changed";

/** The chip tone a tag is drawn in. Matches the Chip variants. */
export type NoteTone = "danger" | "success" | "info" | "neutral" | "warning";

export interface NoteIssue {
  number: number;
  /** "" when no repo is known, which the caller renders as plain text. */
  url: string;
}

export interface NoteEntry {
  /** The heading this bullet sat under, verbatim. */
  category: string;
  /** The short tag drawn in the row's gutter: SEC / NEW / FIX / GONE / CHG. */
  tag: string;
  tone: NoteTone;
  /** One line, plain text. Issue markers stripped. */
  title: string;
  /** The rest of the bullet, still markdown, for the disclosure body. */
  detail: string;
  issues: NoteIssue[];
}

export interface ParsedReleaseNotes {
  entries: NoteEntry[];
  /** Per category, in the order the headings appeared. */
  counts: Record<string, number>;
}

const KNOWN: Record<string, { tag: string; tone: NoteTone }> = {
  security: { tag: "SEC", tone: "danger" },
  added: { tag: "NEW", tone: "success" },
  fixed: { tag: "FIX", tone: "info" },
  removed: { tag: "GONE", tone: "neutral" },
  changed: { tag: "CHG", tone: "warning" },
};

/** An unknown heading still gets a gutter tag, from its own letters. */
function categoryTag(heading: string): { tag: string; tone: NoteTone } {
  const known = KNOWN[heading.trim().toLowerCase()];
  if (known) return known;
  const letters = heading.replace(/[^A-Za-z]/g, "").toUpperCase();
  return { tag: letters.slice(0, 4) || "NOTE", tone: "neutral" };
}

/** The issue marker a title ends with: a trailing parenthetical of nothing but
 *  issue numbers and separators — `(#117)`, `(#110, #111)`, `(#104; #105-#119)`. */
const TRAILING_MARKER = /[\s,;:]*\(\s*#\d[\d\s,;#\u2013\u2014-]*\)[\s.,;:]*$/;
/** Any issue reference anywhere in a bullet. */
const ISSUE_REF = /#(\d+)\b/g;

/** A `###`-or-deeper heading opens a category; a `#`/`##` heading CLOSES the
 *  changelog — the publish workflow appends an "## Install or upgrade" section
 *  that is instructions, not changes. */
const HEADING = /^(#{1,6})\s+(.*)$/;
/** A bullet at column 0. Nested bullets are indented and belong to the detail. */
const TOP_BULLET = /^[-*]\s+(.*)$/;

interface RawBullet {
  category: string;
  lines: string[];
}

/** Split the changelog part of a body into (category, bullet-text) pairs. */
function bullets(markdown: string): RawBullet[] {
  const out: RawBullet[] = [];
  let category = "";
  let current: RawBullet | null = null;

  for (const raw of markdown.replace(/\r\n?/g, "\n").split("\n")) {
    const heading = HEADING.exec(raw);
    if (heading) {
      current = null;
      if (heading[1].length <= 2) break; // the changelog section ends here
      category = heading[2].trim().replace(/\s*#+\s*$/, "");
      continue;
    }
    const bullet = raw.startsWith(" ") || raw.startsWith("\t") ? null : TOP_BULLET.exec(raw);
    if (bullet) {
      current = { category, lines: [bullet[1]] };
      out.push(current);
      continue;
    }
    if (!current) continue;
    if (raw.trim() === "") {
      // A blank line may be inside a multi-paragraph bullet or after it; the
      // next line decides, so keep it and trim at the end.
      current.lines.push("");
      continue;
    }
    if (raw.startsWith("  ") || raw.startsWith("\t")) {
      current.lines.push(raw);
      continue;
    }
    // Un-indented prose at column 0 is not part of the bullet.
    current = null;
  }
  return out;
}

/** Drop the two-space continuation indent so the detail is markdown again. */
function dedent(lines: string[]): string {
  const body = lines.map((l) => (l.startsWith("  ") ? l.slice(2) : l.replace(/^\t/, "")));
  while (body.length && body[body.length - 1].trim() === "") body.pop();
  return body.join("\n");
}

function collapse(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

/** The title's markdown emphasis, stripped: the row draws plain text. Inline
 *  code keeps its content, not its backticks — `QUASAR_APP_MOUNT_ALLOW` reads
 *  as a name in a one-line title, not as a code span. */
function plainTitle(markdown: string): string {
  return collapse(
    markdown
      .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
      .replace(/`+/g, "")
      .replace(/\*\*|__/g, ""),
  );
}

/** The leading `**bold**` run of a bullet, and what follows it. Returns null
 *  when the bullet does not open with one. The bold run may wrap lines. */
function leadingBold(text: string): { bold: string; rest: string } | null {
  const t = text.replace(/^\s+/, "");
  if (!t.startsWith("**")) return null;
  const end = t.indexOf("**", 2);
  if (end < 0) return null;
  return { bold: t.slice(2, end), rest: t.slice(end + 2) };
}

/** A bullet with no bold run is titled by its first sentence — the shape the
 *  older, un-templated changelog entries have. A sentence ends at `. ` (not
 *  inside a code span), and the whole bullet is the title when there is none. */
function firstSentence(text: string): { head: string; rest: string } {
  let ticks = false;
  for (let i = 0; i < text.length; i++) {
    if (text[i] === "`") ticks = !ticks;
    if (ticks) continue;
    if (text[i] === "." && (i + 1 === text.length || /\s/.test(text[i + 1]))) {
      return { head: text.slice(0, i + 1), rest: text.slice(i + 1) };
    }
  }
  return { head: text, rest: "" };
}

function issuesOf(text: string, repo: string): NoteIssue[] {
  const seen = new Set<number>();
  const out: NoteIssue[] = [];
  for (const m of text.matchAll(ISSUE_REF)) {
    const n = Number(m[1]);
    if (!Number.isFinite(n) || seen.has(n)) continue;
    seen.add(n);
    out.push({ number: n, url: repo ? `https://github.com/${repo}/issues/${n}` : "" });
  }
  return out;
}

/**
 * Parse a release body into rows.
 *
 * `repo` is the view's `source_repo` (`owner/name`); "" makes every issue a
 * plain `#NNN` rather than a link to a repository we are guessing at.
 */
export function parseReleaseNotes(markdown: string, repo: string): ParsedReleaseNotes {
  const entries: NoteEntry[] = [];
  const counts: Record<string, number> = {};

  for (const bullet of bullets(markdown ?? "")) {
    const whole = dedent(bullet.lines);
    if (whole.trim() === "") continue;

    let title: string;
    let detail: string;
    const bold = leadingBold(whole);
    if (bold) {
      title = plainTitle(bold.bold).replace(TRAILING_MARKER, "").replace(/[.,;:]+$/, "");
      detail = bold.rest.replace(/^[\s,;:.]+/, "");
    } else {
      const { head, rest } = firstSentence(whole);
      title = plainTitle(head).replace(TRAILING_MARKER, "").replace(/[.,;:]+$/, "");
      detail = rest.replace(/^[\s,;:.]+/, "");
    }
    if (title === "") continue;

    const { tag, tone } = categoryTag(bullet.category);
    entries.push({
      category: bullet.category,
      tag,
      tone,
      title,
      detail: detail.trim(),
      issues: issuesOf(whole, repo),
    });
    counts[bullet.category] = (counts[bullet.category] ?? 0) + 1;
  }

  return { entries, counts };
}

/** "3 security · 2 added · 2 fixed · 1 removed" — the release card's meta. */
export function releaseCountsLine(counts: Record<string, number>): string {
  return Object.entries(counts)
    .map(([category, n]) => `${n} ${category.toLowerCase()}`)
    .join(" · ");
}
