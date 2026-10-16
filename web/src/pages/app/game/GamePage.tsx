// /app/game/:id. The detail band grown to a page: the first viewport says what
// this is, what Play will do and offers Play; history, the last session, About
// and related titles sit below the fold. With the Classic layout switch on, the
// URL hands the game to Home's band instead.

import { useMemo } from "react";
import type { CSSProperties } from "react";
import { Link, Navigate, useParams } from "react-router-dom";
import type { App } from "../../../api/types";
import { Button } from "../../../components/Button";
import { useNewDesign } from "../../../settings/OverlayPreferencesContext";
import { HomeState } from "../home/panels";
import { useHeroPalette } from "../home/useHeroPalette";
import { useLibraryCatalog } from "../libraryCatalog";
import "../../../styles/home.css";
import "./game.css";
import {
  BackButton,
  Credits,
  GameTitle,
  GenreChips,
  HeroArt,
  HistoryStats,
  LastSession,
  LaunchBlock,
  OptionsSheet,
  Section,
  TileRow,
  historyLine,
} from "./parts";
import { moreLikeThis } from "./related";
import { useGamePage } from "./useGamePage";

/** Router state that asks Home to open a game's band (see AppHomeNext). */
export interface OpenBandState {
  openApp?: string;
}

export function GamePageRoute() {
  const { id = "" } = useParams();
  const newDesign = useNewDesign();
  const { apps, settled, error, reload } = useLibraryCatalog();
  if (!newDesign) return <Navigate to="/app" replace state={{ openApp: id } satisfies OpenBandState} />;
  const app = apps.find((a) => a.id === id);
  if (app) return <LaunchFirst key={app.id} app={app} />;
  return (
    <div className="home gp">
      <div className="gp-missing" aria-busy={!settled || undefined}>
        {error ? (
          <HomeState
            tone="error"
            title="Your library didn't load"
            body={<>The server didn't return your catalogue. It reported: “{error}”.</>}
            action={
              <Button variant="primary" onClick={reload}>
                Try again
              </Button>
            }
          />
        ) : !settled ? (
          <p className="muted">Loading…</p>
        ) : (
          <>
            <h1 className="gp-h2">This game is not in your library</h1>
            <Link className="btn" to="/app/library">
              Back to library
            </Link>
          </>
        )}
      </div>
    </div>
  );
}

function LaunchFirst({ app }: { app: App }) {
  const g = useGamePage(app);
  const hero = useHeroPalette(app);
  const x = g.extras;
  const history = historyLine(x);
  const meta = [x?.genres.slice(0, 2).join(", "), x?.developer, x?.release_date?.match(/\d{4}/)?.[0]]
    .filter(Boolean)
    .join(" · ");
  const like = useMemo(
    () => (g.extrasMap ? moreLikeThis(g.apps, g.extrasMap, app) : []),
    [g.extrasMap, g.apps, app],
  );

  return (
    <div className="home gp gp-launch-first">
      <div className="gp-page" inert={g.optionsOpen}>
        <section
          className="detail show gp-hero"
          style={{ "--scrim-rgb": hero.palette.scrimRgb, "--accent-rgb": hero.palette.accentRgb } as CSSProperties}
        >
          <HeroArt app={app} hero={hero} coverClass={g.coverClassById.get(app.id) ?? "cv-violet"} />
          <div className="d-inner gp-hero-inner">
            <BackButton onBack={g.back} />
            <div className="gp-hero-body">
              <GameTitle name={app.name} logoUrl={x?.logo_url ?? null} />
              {meta && <p className="gp-meta">{meta}</p>}
              <LaunchBlock g={g} />
              {history && <p className="gp-history">{history}</p>}
            </div>
          </div>
        </section>

        <div className="gp-body">
          {x?.play && (
            <Section title="Your history">
              <HistoryStats extras={x} />
            </Section>
          )}
          {x?.play?.last_session && (
            <Section title="Last session">
              <LastSession s={x.play.last_session} />
            </Section>
          )}
          {(app.description || x) && (
            <Section title="About" className="gp-about">
              {app.description && <p className="gp-desc">{app.description}</p>}
              {x && <GenreChips genres={x.genres} />}
              {x && <Credits extras={x} />}
            </Section>
          )}
          {like.length > 0 && (
            <Section title="More like this" className="gp-wide">
              <TileRow apps={like} coverClassById={g.coverClassById} label="More like this" />
            </Section>
          )}
        </div>
      </div>
      <OptionsSheet g={g} />
    </div>
  );
}
