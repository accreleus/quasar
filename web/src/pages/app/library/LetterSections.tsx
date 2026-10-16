// A–Z splits a list into letter sections; the other sorts keep one grid and
// switch on the caption strip under each tile.

import type { ReactNode } from "react";
import type { App } from "../../../api/types";
import { letterOf, lettered, type SortMode } from "./organise";

/** Wraps the page's `renderGrid`. `total` is the whole visible list: a short
 *  A–Z list (a filter, a search) stays one plain grid. */
export function sectioned(sort: SortMode, total: number, renderGrid: (list: readonly App[], key: string) => ReactNode) {
  return (list: readonly App[], key: string): ReactNode => {
    if (sort === "az" && !lettered(sort, total)) return renderGrid(list, key);
    if (sort !== "az") return <div className="lib-meta" key={key}>{renderGrid(list, key)}</div>;
    const chunks: { letter: string; apps: App[] }[] = [];
    for (const a of list) {
      const l = letterOf(a.name);
      if (chunks[chunks.length - 1]?.letter !== l) chunks.push({ letter: l, apps: [] });
      chunks[chunks.length - 1].apps.push(a);
    }
    // Home's letters sit under the h2; the Library view's under a source h3.
    const H = key === "all" ? "h3" : "h4";
    return chunks.map((c) => {
      const id = `lib-letter-${key}-${c.letter === "#" ? "num" : c.letter}`;
      return (
        <section key={id} className="lib-letter" data-letter-section={c.letter} aria-labelledby={id}>
          <H id={id} className="lib-letter-h">
            {c.letter}
            <span className="count">{c.apps.length}</span>
          </H>
          {renderGrid(c.apps, id)}
        </section>
      );
    });
  };
}

/** The caption strip's content for one tile, or null (A–Z: no strip). */
export function tileMeta(metaFor: (app: App) => string | null, app: App): ReactNode {
  const text = metaFor(app);
  return text ? <span className="lib-tile-meta">{text}</span> : null;
}
