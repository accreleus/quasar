// Fuzzy game search: the one matcher behind the topbar filter, its results
// popover and the Ctrl+K palette. Pure; the hooks live in useFuzzy.ts.
//
// Word-wise: every query word must land on some word of the name, scored
//   exact word 3 > word prefix 2 > inside the joined name 1.5 > typo ≤ k 1–0.5
// where k (allowed edits, Damerau/OSA) is 0 under 4 letters, 1 for 4–7, 2 for 8+.
// A typo is measured against the name word's prefix of about the same length, so
// a half-typed, misspelled word ("ciies sky" → "Cities: Skylines") still lands.

export interface Matchable {
  id: string;
  name: string;
  description?: string | null;
  favourite?: boolean;
}

/** Last-played time (ms) by app id, for ties. */
export type Recency = (id: string) => number;

const norm = (s: string) =>
  s.normalize("NFD").replace(/[̀-ͯ]/g, "").replace(/['’]/g, "").toLowerCase();
const words = (s: string) => norm(s).split(/[^\p{L}\p{N}]+/u).filter(Boolean);

interface Prepared {
  words: string[];
  joined: string;
  full: string;
}
const cache = new Map<string, Prepared>();
function prep(name: string): Prepared {
  let p = cache.get(name);
  if (!p) {
    const w = words(name);
    p = { words: w, joined: w.join(""), full: w.join(" ") };
    cache.set(name, p);
  }
  return p;
}

/** Optimal-string-alignment distance (Levenshtein + adjacent swaps). */
export function osa(a: string, b: string): number {
  const m = a.length;
  const n = b.length;
  const d: number[][] = Array.from({ length: m + 1 }, (_, i) => [i, ...Array<number>(n).fill(0)]);
  for (let j = 1; j <= n; j++) d[0][j] = j;
  for (let i = 1; i <= m; i++)
    for (let j = 1; j <= n; j++) {
      const c = a[i - 1] === b[j - 1] ? 0 : 1;
      d[i][j] = Math.min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + c);
      if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1]) d[i][j] = Math.min(d[i][j], d[i - 2][j - 2] + 1);
    }
  return d[m][n];
}

const allowed = (q: string) => (q.length >= 8 ? 2 : q.length >= 4 ? 1 : 0);

function wordScore(q: string, p: Prepared): number {
  let best = 0;
  for (const w of p.words) {
    if (w === q) return 3;
    if (w.startsWith(q)) best = 2;
  }
  if (best) return best;
  if (q.length >= 3 && p.joined.includes(q)) return 1.5;
  const k = allowed(q);
  if (!k) return 0;
  for (const w of p.words) {
    if (w.length < q.length - k) continue;
    for (let len = q.length - 1; len <= q.length + 1; len++) {
      if (len > w.length) break;
      const e = osa(q, w.slice(0, len));
      if (e <= k) best = Math.max(best, 1 - e * 0.25);
    }
  }
  return best;
}

/** 0 = no match. Higher is better; exact name > name prefix > word matches > typos. */
export function score(app: Matchable, query: string): number {
  const qw = words(query);
  if (!qw.length) return 0;
  const p = prep(app.name);
  let s = 0;
  for (const q of qw) {
    const w = wordScore(q, p);
    if (!w) {
      // The shipped filter also matched descriptions; keep that, ranked last.
      const d = app.description ? norm(app.description) : "";
      return d.includes(norm(query.trim())) ? 0.1 : 0;
    }
    s += w;
  }
  const q = qw.join(" ");
  if (p.full === q) s += 20;
  else if (p.full.startsWith(q)) s += 8;
  // A name is remembered by its first word: "portl" means Portal before Portia.
  const first = p.words[0] ? wordScore(qw[0], { words: [p.words[0]], joined: p.words[0], full: p.words[0] }) : 0;
  s += first >= 2 ? 1 : first > 0 ? 0.5 : 0;
  return s;
}

/** Every match, best first; ties by recency, then favourite, then name. */
export function rank<T extends Matchable>(apps: readonly T[], query: string, recency: Recency = () => 0): T[] {
  const hits: { a: T; s: number }[] = [];
  for (const a of apps) {
    const s = score(a, query);
    if (s > 0) hits.push({ a, s });
  }
  // A typo match only counts next to hits of its own quality. Once something
  // matches exactly or by prefix, near-misses are noise ("gris" → 21 apps).
  const top = Math.max(0, ...hits.map((h) => h.s));
  return hits
    .filter((h) => h.s >= top * 0.5)
    .sort(
      (x, y) =>
        y.s - x.s ||
        recency(y.a.id) - recency(x.a.id) ||
        Number(Boolean(y.a.favourite)) - Number(Boolean(x.a.favourite)) ||
        x.a.name.localeCompare(y.a.name),
    )
    .map((h) => h.a);
}
