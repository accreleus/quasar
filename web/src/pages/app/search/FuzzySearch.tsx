// The topbar search field with a results popover (APG combobox + listbox).
//
// Same box, same place, same `.search` class as the classic field (so `/` still
// focuses it). Typing still filters the grid below; the popover adds the six
// best hits with icon, genre and last played, and Enter opens the top one's
// launcher. Focus on an empty box offers Recently played and Favourites.

import { useEffect, useId, useMemo, useRef, useState } from "react";
import type { App } from "../../../api/types";
import { IconSearch } from "../../../components/icons";
import { artFor } from "../../../lib/appArtwork";
import { appGlyph } from "../../../lib/appGlyph";
import { relativeDay, useLibraryExtras, type ExtrasMap } from "../library/libraryExtras";
import { rank } from "./match";
import { OPEN_EVENT, recencyOf, requestOpen } from "./useFuzzy";
import "../../../styles/search-fuzzy.css";

const HITS = 6;
const QUICK = 4;

interface Group {
  title: string;
  apps: App[];
}

export function metaFor(app: App, extras: ExtrasMap | null): string {
  const e = extras?.[app.id];
  const genre = e?.genres[0] ?? (app.kind === "game" ? "Game" : app.kind.charAt(0).toUpperCase() + app.kind.slice(1));
  return e?.play ? `${genre} · played ${relativeDay(e.play.last_played_at)}` : `${genre} · not played yet`;
}

function Icon({ app, extras }: { app: App; extras: ExtrasMap | null }) {
  // The title's icon, then the grid art, then the glyph underneath both.
  const sources = [extras?.[app.id]?.icon_url, artFor(app, "tile")].filter((s): s is string => Boolean(s));
  const [failed, setFailed] = useState(0);
  const src = sources[failed];
  return (
    <span className="fz-ico" aria-hidden>
      <span className="glyph">{appGlyph(app.name)}</span>
      {src && <img src={src} alt="" loading="lazy" decoding="async" onError={() => setFailed((n) => n + 1)} />}
    </span>
  );
}

export function FuzzySearch({
  query,
  setQuery,
  apps,
  onChoose = (app) => requestOpen(app.id),
}: {
  query: string;
  setQuery: (q: string) => void;
  apps: readonly App[];
  /** What choosing a game does; by default its launcher opens in the library. */
  onChoose?: (app: App) => void;
}) {
  const extras = useLibraryExtras();
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const id = useId();
  const q = query.trim();
  // A chosen game (here or in the palette) hands focus back to this field; that
  // return is not a request for quick picks over the launcher it just opened.
  const quietUntil = useRef(0);
  useEffect(() => {
    const on = () => {
      quietUntil.current = Date.now() + 600;
      setOpen(false);
    };
    window.addEventListener(OPEN_EVENT, on);
    return () => window.removeEventListener(OPEN_EVENT, on);
  }, []);

  const groups = useMemo<Group[]>(() => {
    const recency = recencyOf(extras);
    if (q) return [{ title: "Games", apps: rank(apps, q, recency).slice(0, HITS) }];
    const recent = apps.filter((a) => recency(a.id) > 0).sort((a, b) => recency(b.id) - recency(a.id)).slice(0, QUICK);
    const favs = apps.filter((a) => a.favourite && !recent.includes(a)).sort((a, b) => recency(b.id) - recency(a.id)).slice(0, QUICK);
    return [
      { title: "Recently played", apps: recent },
      { title: "Favourites", apps: favs },
    ].filter((g) => g.apps.length);
  }, [q, apps, extras]);
  const flat = groups.flatMap((g) => g.apps);
  const total = flat.length;
  const sel = Math.min(active, total - 1);
  const showing = open && (total > 0 || q !== "");
  const optId = (i: number) => `${id}-o${i}`;

  const choose = (app: App | undefined) => {
    if (!app) return;
    setOpen(false);
    onChoose(app);
  };

  let i = -1;
  return (
    <div className="search fz-search">
      <IconSearch className="ic" />
      <input
        placeholder="Search games and hosts…"
        value={query}
        role="combobox"
        aria-label="Search library"
        aria-autocomplete="list"
        aria-expanded={showing}
        aria-controls={`${id}-list`}
        aria-activedescendant={showing && sel >= 0 ? optId(sel) : undefined}
        autoComplete="off"
        spellCheck={false}
        onFocus={() => setOpen(Date.now() > quietUntil.current)}
        onBlur={() => setOpen(false)}
        onChange={(e) => {
          setQuery(e.target.value);
          setActive(0);
          setOpen(true);
        }}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" || e.key === "ArrowUp") {
            e.preventDefault();
            setOpen(true);
            if (total) setActive((sel + (e.key === "ArrowDown" ? 1 : total - 1)) % total);
          } else if (e.key === "Enter" && showing) {
            e.preventDefault();
            choose(flat[sel]);
          } else if (e.key === "Escape" && showing) {
            // Ours alone: the page's Escape would also close an open band.
            e.stopPropagation();
            setOpen(false);
          }
        }}
      />
      {showing && (
        <div className="fz-pop" id={`${id}-list`} role="listbox" aria-label="Search results">
          {total === 0 && <p className="fz-empty">No games match “{q}”</p>}
          {groups.map((g) => (
            <div key={g.title} role="group" aria-labelledby={`${id}-${g.title}`}>
              <div className="fz-sec" id={`${id}-${g.title}`}>
                {q ? `${g.apps.length === HITS ? "Top matches" : g.apps.length === 1 ? "1 match" : `${g.apps.length} matches`}` : g.title}
              </div>
              {g.apps.map((app) => {
                i += 1;
                const k = i;
                return (
                  <div
                    key={app.id}
                    id={optId(k)}
                    role="option"
                    aria-selected={k === sel}
                    aria-label={app.name}
                    aria-describedby={`${optId(k)}-m`}
                    className="fz-opt"
                    // Keep focus in the field: the field owns the keyboard.
                    onMouseDown={(e) => e.preventDefault()}
                    onMouseEnter={() => setActive(k)}
                    onClick={() => choose(app)}
                  >
                    <Icon app={app} extras={extras} />
                    <span className="fz-txt">
                      <span className="fz-name">{app.name}</span>
                      <span className="fz-meta" id={`${optId(k)}-m`}>
                        {metaFor(app, extras)}
                      </span>
                    </span>
                  </div>
                );
              })}
            </div>
          ))}
          <div className="fz-foot" aria-hidden>
            <span>↑↓ choose</span>
            <span>↵ open</span>
            <span>esc close</span>
          </div>
        </div>
      )}
    </div>
  );
}
