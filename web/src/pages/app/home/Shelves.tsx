// Home shelves above the library grid: Favourites, and Jump back in (the most
// recently played). Each is the house tile at shelf size in one sideways row; a
// tile opens the game's page, its hover Play launches.

import { useMemo } from "react";
import type { ReactNode } from "react";
import type { App } from "../../../api/types";
import { LibraryTile } from "./LibraryTile";
import { hoursLabel, relativeDay, useLibraryExtras } from "../library/libraryExtras";
import "./shelves.css";

export interface ShelfTiles {
  busy: boolean;
  liveAppId: string | null;
  blockedIds: Set<string>;
  blockedByName: string | null;
  /** sharedLibraryIds: tiles a sharing desktop holds (#1001). */
  dimmedIds?: Set<string>;
  coverClass: (app: App) => string;
  onOpen: (app: App) => void;
  onPlay: (app: App) => void;
}

const JUMP_BACK_SIZE = 10;
// A shelf tile must not take over the grid tile's ref slot (detail band anchoring).
const noRef = () => {};

function Shelf({
  kind,
  title,
  count,
  apps,
  tile,
  meta,
}: {
  kind: "fav" | "jump";
  title: string;
  count?: number;
  apps: App[];
  tile: ShelfTiles;
  meta?: (app: App) => ReactNode;
}) {
  const titleId = `shelf-${kind}-title`;
  return (
    <section className={`shelf-${kind}`} aria-labelledby={titleId}>
      <div className="src-head">
        <h2 id={titleId}>{title}</h2>
        {count !== undefined && <span className="count">{count}</span>}
      </div>
      <div className={`shelf-${kind}-track`}>
        {apps.map((app) => (
          <LibraryTile
            key={app.id}
            app={app}
            isRunning={tile.liveAppId === app.id}
            isBlocked={tile.blockedIds.has(app.id)}
            blockedByName={tile.blockedByName}
            isDimmed={tile.dimmedIds?.has(app.id)}
            isOpen={false}
            isBusy={tile.busy}
            coverClass={tile.coverClass(app)}
            tileRef={noRef}
            onOpen={() => tile.onOpen(app)}
            onPlay={() => tile.onPlay(app)}
            opensPage
            artSizes="96px" /* shelves.css flex-basis */
            meta={meta?.(app)}
          />
        ))}
      </div>
    </section>
  );
}

export function FavouritesShelf({ apps, tile }: { apps: App[]; tile: ShelfTiles }) {
  if (apps.length === 0) return null;
  return <Shelf kind="fav" title="Favourites" count={apps.length} apps={apps} tile={tile} />;
}

export function JumpBackShelf({ apps, tile }: { apps: App[]; tile: ShelfTiles }) {
  const extras = useLibraryExtras();
  const recent = useMemo(() => {
    if (!extras) return [];
    return apps
      .flatMap((app) => {
        const play = extras[app.id]?.play;
        return play ? [{ app, play }] : [];
      })
      .sort((a, b) => Date.parse(b.play.last_played_at) - Date.parse(a.play.last_played_at))
      .slice(0, JUMP_BACK_SIZE);
  }, [apps, extras]);
  if (recent.length === 0) return null;
  const playOf = new Map(recent.map((r) => [r.app.id, r.play]));
  return (
    <Shelf
      kind="jump"
      title="Jump back in"
      apps={recent.map((r) => r.app)}
      tile={tile}
      meta={(app) => {
        const play = playOf.get(app.id)!;
        return (
          <span className="shelf-jump-meta">
            <span className="when">{relativeDay(play.last_played_at)}</span>
            <span className="hours">{hoursLabel(play.total_seconds)} played</span>
          </span>
        );
      }}
    />
  );
}
