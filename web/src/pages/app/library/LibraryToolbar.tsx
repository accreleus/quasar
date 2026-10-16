// What joins `.lib-head`: the chosen-genre chip (beside the count it changes)
// and the Genre and Sort menus. Rendered as a fragment so each piece is a flex
// item of the head row (library-new.css orders them).

import { useMemo, useRef } from "react";
import type { App } from "../../../api/types";
import { IconClose, IconSliders } from "../../../components/icons";
import { genreOptions, type SortMode } from "./organise";
import { PickMenu } from "./PickMenu";
import type { Organised } from "./useOrganised";

const reduced = () => typeof window.matchMedia === "function" && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

/** Sort glyph in the shared icon convention (16×16, 1.5 stroke): down and up arrows. */
function IconSort({ className }: { className?: string }) {
  return (
    <svg className={className ?? "ic"} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M5 3v10M2.5 10.5 5 13l2.5-2.5M11 13V3M8.5 5.5 11 3l2.5 2.5" />
    </svg>
  );
}

/** `apps`: the list before the genre filter, so each genre's count is what picking it would show. */
export function LibraryToolbar({ o, apps }: { o: Organised; apps: readonly App[] }) {
  const menus = useRef<HTMLDivElement>(null);
  const genres = useMemo(() => genreOptions(o.genres, apps, o.extras, o.genre), [o.genres, apps, o.extras, o.genre]);
  const sortable = o.sorts.length > 1 && apps.length > 1;
  const sortLabel = o.sorts.find((s) => s.value === o.sort)?.label ?? "A–Z";

  // A pick reorders what is below; on Home the head sits under the hero, so
  // bring it to the top.
  const reveal = () =>
    requestAnimationFrame(() => {
      const head = menus.current?.closest<HTMLElement>(".lib-head");
      if (head && head.getBoundingClientRect().top > window.innerHeight * 0.35)
        head.scrollIntoView({ block: "start", behavior: reduced() ? "auto" : "smooth" });
    });

  return (
    <>
      {o.genre && (
        <button
          type="button"
          className="lib-chip"
          aria-label={`Clear genre: ${o.genre}`}
          onClick={() => {
            o.setGenre(null);
            // The chip is gone; focus goes to the menu that set it.
            menus.current?.querySelector<HTMLElement>(".lib-trigger")?.focus();
          }}
        >
          {o.genre}
          <IconClose className="lib-chip-x" />
        </button>
      )}
      {(genres.length > 0 || sortable) && (
        <div className="lib-menus" ref={menus}>
          {genres.length > 0 && (
            <PickMenu<string>
              name="Genre"
              text="Genre"
              icon={<IconSliders className="ic lib-icon" />}
              value={o.genre ?? ""}
              valueLabel={o.genre}
              options={[{ value: "", label: "All genres", count: apps.length }, ...genres]}
              onPick={(g) => {
                o.setGenre(g || null);
                reveal();
              }}
            />
          )}
          {sortable && (
            <PickMenu<SortMode>
              name="Sort"
              text={sortLabel}
              icon={<IconSort className="ic lib-icon" />}
              value={o.sort}
              valueLabel={sortLabel}
              options={o.sorts}
              onPick={(s) => {
                o.setSort(s);
                reveal();
              }}
            />
          )}
        </div>
      )}
    </>
  );
}
