// The library section under the hero (v3 handoff §B "Library section"): the
// `.lib-head` bar and the grid(s) below it.
//
// Two views, one section. Home renders one flat grid and links on to the full
// library; Library renders one grid per source, each under a `.src-head`
// (librarySources.groupBySource). The heading is the only thing that names the
// section for a screen reader, so the `<section>` points at it.
//
// The tiles themselves are the caller's: `renderGrid` is passed in because a
// tile needs a dozen pieces of page state (live session, blocked family, busy,
// the open band) that this component has no business holding.

import type { ReactNode, RefObject } from "react";
import { Link } from "react-router-dom";
import type { App } from "../../../api/types";
import { SegmentedControl, type SegmentOption } from "../../../components/SegmentedControl";
import type { KindFilter } from "../libraryGrid";
import type { SourceGroup } from "./librarySources";

export interface LibraryGridProps {
  view: "home" | "library";
  /** Apps matching the current filter, or null while the catalogue is
   *  unknown (loading, or a failed fetch) — the count line is a fact about a
   *  catalogue, not a zero to print over an error. */
  count: number | null;
  showFilter: boolean;
  kindFilter: KindFilter;
  kindOptions: readonly SegmentOption<KindFilter>[];
  onKindFilter: (value: KindFilter) => void;
  /** Right-hand cluster: "Stop session" while one is live. */
  actions?: ReactNode;
  /** Library view: one grid per source. Null in Home view. */
  groups: readonly SourceGroup<App>[] | null;
  /** Home view: the one flat grid. */
  apps: readonly App[];
  renderGrid: (list: App[], key: string) => ReactNode;
  /** The box the page's roving-focus model queries tiles out of. */
  gridsRef: RefObject<HTMLDivElement>;
  /** Loading / error / empty panels, rendered above the grids. */
  children?: ReactNode;
}

export function LibraryGrid({
  view,
  count,
  showFilter,
  kindFilter,
  kindOptions,
  onKindFilter,
  actions,
  groups,
  apps,
  renderGrid,
  gridsRef,
  children,
}: LibraryGridProps) {
  const title = view === "library" ? "Library" : "Your library";
  return (
    <section className="home-lib" aria-labelledby="home-lib-title">
      <div className="lib-head">
        <h2 id="home-lib-title">{title}</h2>
        {count !== null && (
          <span className="count">{count === 1 ? "1 app" : `${count} apps`}</span>
        )}
        {/* Not rendered when it cannot match anything: over a failed catalogue
            it filtered nothing while looking healthy; over one app it was four
            guaranteed-empty segments above a single tile. */}
        {showFilter && (
          <SegmentedControl<KindFilter>
            aria-label="Filter by kind"
            value={kindFilter}
            onChange={onKindFilter}
            options={[...kindOptions]}
          />
        )}
        <span className="grow" />
        {actions}
        {/* Home shows the catalogue flat; Library groups it by source. The
            link is what carries you between the two, so in the Library view it
            would point at the page you are already on. */}
        {view === "home" && (
          <Link className="lib-open" to="/app/library">
            Open full library
          </Link>
        )}
      </div>

      {children}

      <div ref={gridsRef}>
        {groups
          ? groups.map((group) => (
              <section key={group.id} aria-labelledby={group.id}>
                <div className="src-head">
                  <h3 id={group.id}>{group.title}</h3>
                  <span className="count">{group.apps.length}</span>
                  <span className="s-sub">{group.sub}</span>
                </div>
                {renderGrid(group.apps, group.id)}
              </section>
            ))
          : apps.length > 0 && renderGrid([...apps], "all")}
      </div>
    </section>
  );
}
