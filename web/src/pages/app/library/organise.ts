// Sort, genre and A–Z for the library section: the pure parts, kept apart for
// the test. State lives in useOrganised.

import type { App } from "../../../api/types";
import { hoursLabel, relativeDay, type ExtrasMap } from "./libraryExtras";

export type SortMode = "az" | "recent" | "most" | "added";
export const SORTS: { value: SortMode; label: string }[] = [
  { value: "az", label: "A–Z" },
  { value: "recent", label: "Recently played" },
  { value: "most", label: "Most played" },
  { value: "added", label: "Recently added" },
];
export const LETTERS = [..."ABCDEFGHIJKLMNOPQRSTUVWXYZ", "#"];

/** Below this many visible titles A–Z is one plain grid (no letter sections, no
 *  index): 40 favourites read as a shelf, not 26 one-tile sections. */
export const LETTER_MIN = 60;
/** A genre earns a menu entry with at least this many titles. */
const GENRE_MIN = 15;

/** Whether an A–Z list of `n` titles is split into letter sections. */
export function lettered(sort: SortMode, n: number): boolean {
  return sort === "az" && n >= LETTER_MIN;
}

/** "B" for "Borderlands", "E" for "Évoland", "S" for "ßtest" (uppercases to
 *  "SS"), "#" for "7 Days" or a CJK title. Always an index letter or "#". */
export function letterOf(name: string): string {
  const c = name.trim().normalize("NFD").charAt(0).toUpperCase().charAt(0);
  return c >= "A" && c <= "Z" ? c : "#";
}

/** Sections A–Z, "#" last, matching the index; names by locale inside one. The
 *  letter decides first, not the locale, so a letter is never split in two
 *  (Swedish sorts "Ä" after "Z"). Favourites are not floated: an index needs
 *  one unbroken alphabet, and Favourites has its own filter. */
function byName(a: App, b: App): number {
  const la = letterOf(a.name);
  const lb = letterOf(b.name);
  return la === lb ? a.name.localeCompare(b.name) : la === "#" ? 1 : lb === "#" ? -1 : la < lb ? -1 : 1;
}

export function arrangeApps(apps: readonly App[], sort: SortMode, genre: string | null, extras: ExtrasMap | null): App[] {
  const x = extras ?? {};
  const list = genre ? apps.filter((a) => x[a.id]?.genres.includes(genre)) : [...apps];
  const key = (a: App): number => {
    const e = x[a.id];
    if (sort === "recent") return e?.play ? Date.parse(e.play.last_played_at) : -Infinity;
    if (sort === "most") return e?.play?.total_seconds ?? -Infinity;
    if (sort === "added") return e?.added_at ? Date.parse(e.added_at) : -Infinity;
    return 0;
  };
  return list.sort((a, b) => key(b) - key(a) || byName(a, b));
}

export function topGenres(extras: ExtrasMap | null): string[] {
  const n = new Map<string, number>();
  for (const e of Object.values(extras ?? {})) for (const g of e.genres) n.set(g, (n.get(g) ?? 0) + 1);
  return [...n].filter(([, c]) => c >= GENRE_MIN).sort((a, b) => b[1] - a[1]).map(([g]) => g);
}

/** Genres in the menu: alphabetical (a menu is scanned, not ranked), each with
 *  how many of `apps` (the list before the genre filter) it would show. A genre
 *  with nothing to show is left out unless it is the one chosen. */
export function genreOptions(genres: readonly string[], apps: readonly App[], extras: ExtrasMap | null, chosen: string | null) {
  const n = new Map<string, number>();
  for (const a of apps) for (const g of extras?.[a.id]?.genres ?? []) n.set(g, (n.get(g) ?? 0) + 1);
  return [...genres]
    .filter((g) => (n.get(g) ?? 0) > 0 || g === chosen)
    .sort((a, b) => a.localeCompare(b))
    .map((g) => ({ value: g, label: g, count: n.get(g) ?? 0 }));
}

/** The caption under a tile's art, or null in A–Z (no caption strip at all). */
export function metaLine(app: App, sort: SortMode, extras: ExtrasMap | null): string | null {
  if (sort === "az") return null;
  const e = extras?.[app.id];
  if (sort === "added") return e?.added_at ? `Added ${relativeDay(e.added_at)}` : "—";
  const p = e?.play;
  if (!p) return "Not played yet";
  const when = relativeDay(p.last_played_at);
  const hours = hoursLabel(p.total_seconds);
  return sort === "most" ? `${hours} · ${when}` : `${when} · ${hours}`;
}

/** The letter to mark: the last section whose top has reached the jump line
 *  (where a jump parks a section), so it agrees with what a jump shows. At the
 *  very bottom a short last section (Z, #) cannot reach that line; then the
 *  letter last pressed wins if it is on screen. */
export function spyLetter(
  secs: readonly { letter: string; top: number }[],
  edge: number,
  atEnd: boolean,
  jumped: string | null,
  vh: number,
): string | null {
  let cur: string | null = null;
  for (const s of secs) {
    if (s.top > edge) break;
    cur = s.letter;
  }
  const j = atEnd && jumped ? secs.find((s) => s.letter === jumped) : undefined;
  return j && j.top < vh ? j.letter : cur;
}
