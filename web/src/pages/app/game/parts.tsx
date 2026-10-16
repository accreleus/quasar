// Presentational pieces of the game page. The hero reuses
// the detail band's own classes (`.detail`, `.hero-art`, `.d-rec`, the glass
// controls) so the page reads as the band grown to a page, not a new world.

import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { App } from "../../../api/types";
import { Chip } from "../../../components/Chip";
import { IconChevronLeft, IconHeart, IconPlayGlyph } from "../../../components/icons";
import { appGlyph } from "../../../lib/appGlyph";
import { BandNotes } from "../home/BandNotes";
import { LaunchOptions } from "../home/LaunchOptions";
import type { HeroArt as HeroArtState } from "../home/useHeroPalette";
import { formatBitrate } from "../home/launchOptionRules";
import { hoursLabel, relativeDay, type GameExtras, type GameSessionStats } from "./gameData";
import { StreamSettings } from "./StreamSettings";
import { imageLogoReadable } from "./logoInk";
import type { GamePage } from "./useGamePage";

/** The art layer: the wide hero, else the portrait grid blurred to fill, else the gradient. */
export function HeroArt({ app, hero, coverClass }: { app: App; hero: HeroArtState; coverClass: string }) {
  const isGrid = !app.hero_url && Boolean(hero.art);
  return (
    <div className={`hero-art${hero.art ? "" : ` cover ${coverClass}`}`} data-grid={isGrid || undefined}>
      {hero.art ? (
        <img ref={hero.imgRef} src={hero.art} alt="" decoding="async" onLoad={hero.onLoad} />
      ) : (
        <span className="glyph" aria-hidden>
          {appGlyph(app.name)}
        </span>
      )}
    </div>
  );
}

/**
 * The game's name as its logo when the logo reads on the scrim, else as text.
 * Either way it is one h1 named after the game (the logo's alt is the name).
 */
export function GameTitle({ name, logoUrl }: { name: string; logoUrl: string | null }) {
  const [state, setState] = useState<"checking" | "logo" | "text">(logoUrl ? "checking" : "text");
  const imgRef = useRef<HTMLImageElement>(null);
  useEffect(() => setState(logoUrl ? "checking" : "text"), [logoUrl]);
  useEffect(() => {
    // A cached image can be complete before React attaches onLoad.
    const img = imgRef.current;
    if (state === "checking" && img?.complete && img.naturalWidth > 0) setState(imageLogoReadable(img) ? "logo" : "text");
  }, [state]);
  if (state === "text" || !logoUrl)
    return (
      <h1 className="gp-title">{name}</h1>
    );
  return (
    <h1 className="gp-title gp-title-logo" data-state={state}>
      <img
        ref={imgRef}
        src={logoUrl}
        alt={name}
        decoding="async"
        onLoad={(e) => setState(imageLogoReadable(e.currentTarget) ? "logo" : "text")}
        onError={() => setState("text")}
      />
    </h1>
  );
}

export function BackButton({ onBack }: { onBack: () => void }) {
  return (
    <button type="button" className="btn btn-sm gp-back" onClick={onBack}>
      <IconChevronLeft className="ic" />
      Back
    </button>
  );
}

/** Play, the favourite heart, and the settings sentence with Change. */
export function LaunchBlock({ g }: { g: GamePage }) {
  const { app } = g;
  return (
    <div className="gp-launch">
      <div className="d-actions gp-actions">
        <button
          type="button"
          className="btn btn-primary btn-lg gp-play"
          disabled={g.playDisabled}
          onClick={g.onPlay}
          aria-keyshortcuts="P"
        >
          <IconPlayGlyph className="ic" />
          {g.playLabel}
        </button>
        <button
          type="button"
          className="icon-btn heart"
          aria-label={app.favourite ? `Remove ${app.name} from favourites` : `Add ${app.name} to favourites`}
          aria-pressed={app.favourite}
          onClick={g.toggleFavourite}
        >
          <IconHeart filled={app.favourite} />
        </button>
      </div>
      <StreamSettings g={g} />
      <BandNotes {...g.notes} />
    </div>
  );
}

/** The launch options panel in a sheet over the page: one Play live at a time. */
export function OptionsSheet({ g }: { g: GamePage }) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const { optionsOpen, selection } = g;
  useEffect(() => {
    if (optionsOpen) closeRef.current?.focus();
  }, [optionsOpen]);
  if (!selection.columns || !selection.draft) return null;
  return (
    <div className="gp-sheet" data-open={optionsOpen || undefined}>
      <div className="gp-sheet-scrim" onClick={g.closeOptions} aria-hidden />
      <div
        className="gp-sheet-panel"
        role={optionsOpen ? "dialog" : undefined}
        aria-modal={optionsOpen ? "true" : undefined}
        aria-label={optionsOpen ? `Launch options for ${g.app.name}` : undefined}
      >
        <LaunchOptions
          id={`gp-lo-${g.app.id}`}
          open={optionsOpen}
          appName={g.app.name}
          columns={selection.columns}
          spec={selection.draftSpec}
          verdict={selection.draftVerdict}
          launching={g.launching}
          waitingForSlot={g.waitingForSlot}
          waitingReason={g.waitingReason}
          closeRef={closeRef}
          onSelectCodec={(codec) => selection.edit({ codec }, "codec")}
          onSelectFps={(fps) => selection.edit({ fps }, "fps")}
          onSelectHeight={(height) => selection.edit({ height }, "resolution")}
          onCancel={g.closeOptions}
          onPlay={() => g.play(selection.draft)}
        />
      </div>
    </div>
  );
}

/** "Last played last week · 42 h", or nothing for a game never played. */
export function historyLine(extras: GameExtras | null): string | null {
  const p = extras?.play;
  if (!p) return null;
  return `Last played ${relativeDay(p.last_played_at)} · ${hoursLabel(p.total_seconds)}`;
}

export function Section({ title, children, className }: { title: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={`gp-section${className ? ` ${className}` : ""}`}>
      <h2 className="gp-h2">{title}</h2>
      {children}
    </section>
  );
}

export function HistoryStats({ extras }: { extras: GameExtras }) {
  const p = extras.play;
  if (!p) return null;
  return (
    <dl className="gp-stats">
      <div>
        <dt>Last played</dt>
        <dd>{relativeDay(p.last_played_at)}</dd>
      </div>
      <div>
        <dt>Total</dt>
        <dd>{hoursLabel(p.total_seconds)}</dd>
      </div>
      <div>
        <dt>Sessions</dt>
        <dd>{p.sessions}</dd>
      </div>
    </dl>
  );
}

const CODEC: Record<string, string> = { h264: "H.264", h265: "HEVC", hevc: "HEVC", av1: "AV1" };

/** What was asked for against what the host actually delivered. */
export function LastSession({ s }: { s: GameSessionStats }) {
  const { requested: r, delivered: d } = s;
  const rows: [string, string, string][] = [
    ["Resolution", `${r.width}×${r.height}`, `${d.width}×${d.height} · full for ${d.held_full_res_pct}%`],
    ["Frame rate", `${r.fps} fps`, `${d.fps_avg} fps avg`],
    ["Bitrate", formatBitrate(r.bitrate_kbps), `${formatBitrate(d.bitrate_kbps_avg)} avg`],
    ["Codec", CODEC[r.codec] ?? r.codec, CODEC[d.codec] ?? d.codec],
    ["Round trip", "", `${d.rtt_ms_p50} ms`],
  ];
  return (
    <div className="gp-last">
      <p className="gp-last-meta">
        {relativeDay(s.started_at)[0].toUpperCase() + relativeDay(s.started_at).slice(1)} · {hoursLabel(s.duration_seconds)} on {d.host}
      </p>
      <table className="gp-cmp">
        <thead>
          <tr>
            <th scope="col">
              <span className="sr-only">Measure</span>
            </th>
            <th scope="col">Requested</th>
            <th scope="col">Delivered</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(([label, want, got]) => (
            <tr key={label}>
              <th scope="row">{label}</th>
              <td>{want || "—"}</td>
              <td>{got}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function GenreChips({ genres }: { genres: string[] }) {
  if (!genres.length) return null;
  return (
    <ul className="gp-chips" aria-label="Genres">
      {genres.map((g) => (
        <li key={g}>
          <Chip variant="neutral">{g}</Chip>
        </li>
      ))}
    </ul>
  );
}

/** Developer / publisher / release, only the ones known. */
export function Credits({ extras }: { extras: GameExtras }) {
  const rows = [
    ["Developer", extras.developer],
    ["Publisher", extras.publisher !== extras.developer ? extras.publisher : null],
    ["Released", extras.release_date],
    ["Controller", extras.controller_support === "full" ? "Full support" : extras.controller_support],
  ].filter((r): r is [string, string] => Boolean(r[1]));
  if (!rows.length) return null;
  return (
    <dl className="gp-credits">
      {rows.map(([k, v]) => (
        <div key={k}>
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

/** A row of portrait tiles, each opening its own game page. */
export function TileRow({ apps, coverClassById, label }: { apps: App[]; coverClassById: Map<string, string>; label: string }) {
  return (
    <ul className="gp-tiles" aria-label={label}>
      {apps.map((a) => (
        <li key={a.id}>
          <Link to={`/app/game/${a.id}`} state={{ from: "game" }} className="gp-tile">
            <span className={`cover ${coverClassById.get(a.id) ?? "cv-violet"}`} aria-hidden>
              <span className="glyph">{appGlyph(a.name)}</span>
              {a.cover_url && (
                <img
                  className="cover-img"
                  src={a.cover_url}
                  alt=""
                  loading="lazy"
                  decoding="async"
                  onError={(e) => (e.currentTarget.hidden = true)}
                />
              )}
            </span>
            <span className="gp-tile-name">{a.name}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}
