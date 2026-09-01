// The inline detail band (v3 handoff §B "Detail view"): the app's identity over
// its hero art, the spec strip, the recommendation line with Adjust, and Play.
// LaunchOptions fills the card when Adjust is pressed, useLaunchDraft owns the
// selection it edits, BandNotes carries the warnings beneath it.

import { forwardRef, useCallback, useEffect, useMemo, useRef } from "react";
import type { RefObject } from "react";
import type { App, ProfilesResponse } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { IconClose, IconHeart, IconPlayGlyph, IconSliders } from "../../../components/icons";
import { appGlyph } from "../../../lib/appGlyph";
import type { probeCodecs } from "../../../webrtc/capability";
import {
  blockingReasons,
  optionsFor,
  resolveSelection,
  toWireCodec,
  type DraftCodec,
  type LaunchDraft,
} from "../launchOptions";
import { BandNotes } from "./BandNotes";
import { LaunchOptions } from "./LaunchOptions";
import { recommendation } from "./launchOptionRules";
import { useHeroPalette } from "./useHeroPalette";
import { useLaunchDraft } from "./useLaunchDraft";

const KIND_LABEL: Record<App["kind"], string> = {
  game: "Game",
  desktop: "Desktop",
  launcher: "Launcher",
};

export interface DetailBandProps {
  app: App;
  codecCaps: ReturnType<typeof probeCodecs>;
  launching: boolean;
  /** #494: a capacity_exhausted bounce is being retried, not shown as failure. */
  waitingForSlot?: boolean;
  /** null = evaluation in flight, string = error message, object = loaded. */
  profiles: ProfilesResponse | string | null;
  /** The overlay's open state — the page's one Escape handler owns it. */
  optionsOpen: boolean;
  optionsToggleRef: RefObject<HTMLButtonElement | null>;
  onToggleOptions: () => void;
  /** Closes the overlay and returns focus to Adjust. Cancel, the overlay's ✕
   *  and Escape all route through this one path. */
  onCloseOptions: () => void;
  onRetryProfiles: () => void;
  onClose: () => void;
  /** `streamCodec` (wire vocabulary) is omitted for an Auto commit. */
  onConfirmProfile: (profileId: string, streamCodec?: "h264" | "h265" | "av1") => void;
  onToggleFavourite: () => void;
  /** spec §2.2: a live session on this app's home is running. Presentation
   *  only — the server's 409 is the gate. */
  isBlocked: boolean;
  blockedByName: string | null;
  liveSessionId: string | null;
  /** This app owns the live session. Play becomes Resume: launching would 409. */
  isLive: boolean;
  onResume: () => void;
}

export const DetailBand = forwardRef<HTMLDivElement, DetailBandProps>(function DetailBand(
  {
    app,
    codecCaps,
    launching,
    waitingForSlot = false,
    profiles,
    optionsOpen,
    optionsToggleRef,
    onToggleOptions,
    onCloseOptions,
    onRetryProfiles,
    onClose,
    onConfirmProfile,
    onToggleFavourite,
    isBlocked,
    blockedByName,
    liveSessionId,
    isLive,
    onResume,
  },
  ref,
) {
  const { isAdmin } = useAuth();
  const loaded = typeof profiles === "object" && profiles !== null ? profiles : null;
  const profilesError = typeof profiles === "string" ? profiles : null;
  const optionsId = `lo-${app.id}`;

  const hero = useHeroPalette(app);

  const model = useMemo(
    () => (loaded ? optionsFor({ app, data: loaded, caps: codecCaps, isAdmin }) : null),
    [loaded, app, codecCaps, isAdmin],
  );
  const selection = useLaunchDraft(model, app, optionsOpen);

  // The overlay covers the band, so focus has to enter it; `.d-inner` goes
  // inert behind it so Tab cannot walk back out into the covered controls.
  const closeRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (optionsOpen) closeRef.current?.focus();
  }, [optionsOpen]);

  /** Commits `next` (defaulting to the committed selection) and launches it. */
  const play = useCallback(
    (next?: LaunchDraft | null) => {
      const chosen = next ?? selection.committed;
      if (!model || !chosen) return;
      const resolved = resolveSelection(model.space, chosen);
      if (!resolved) return;
      selection.commit(chosen);
      const wireCodec =
        chosen.codec === "auto" || resolved.codec === null
          ? undefined
          : toWireCodec(resolved.codec);
      // #525: on a pinned app send the pin itself. {codec, fps, height} is
      // lossy — two profiles sharing height+fps are indistinguishable, and
      // `resolveSelection` picks whichever came first, risking a 409.
      onConfirmProfile(model.pinnedProfileId ?? resolved.profileId, wireCodec);
    },
    [model, selection, onConfirmProfile],
  );

  // The ✕ dismisses the overlay first; only a closed overlay closes the band.
  const handleCloseX = useCallback(() => {
    if (optionsOpen) onCloseOptions();
    else onClose();
  }, [optionsOpen, onCloseOptions, onClose]);

  const resolved = selection.resolved;
  const isDeadEnd =
    Boolean(loaded) && !profilesError && (!resolved || resolved.eligibility === "ineligible");
  const rec = recommendation({
    state: profilesError ? "failed" : loaded ? "loaded" : "loading",
    deadEnd: isDeadEnd,
    selection: resolved,
    codec: selection.committed?.codec,
    recommended:
      (model?.space.entriesByCodec.get("auto") ?? []).find((e) => e.recommended) ?? null,
  });

  const playDisabled = isLive
    ? false
    : launching || isBlocked || !codecCaps.h264 || !resolved || isDeadEnd;
  const playLabel = isLive
    ? "Resume session"
    : waitingForSlot
      ? "Waiting for a slot…"
      : launching
        ? "Launching…"
        : "Play";

  return (
    <div className="detail show" id={`lib-detail-${app.id}`} ref={ref} style={hero.style}>
      <div className="hero-art">
        {hero.art ? (
          /* Demand-loaded by the band opening, so eager is right; async decode
             keeps the open animation off the decode. */
          <img ref={hero.imgRef} src={hero.art} alt="" decoding="async" onLoad={hero.onLoad} />
        ) : (
          <span className="glyph" aria-hidden>
            {appGlyph(app.name)}
          </span>
        )}
      </div>

      <button
        type="button"
        className="icon-btn d-close"
        onClick={handleCloseX}
        aria-label="Close details"
      >
        <IconClose />
      </button>

      <div className="d-inner" inert={optionsOpen}>
        <div className="d-kind">{KIND_LABEL[app.kind] ?? "Game"}</div>
        <h2>{app.name}</h2>
        {app.description && <p className="d-desc">{app.description}</p>}

        <div className="d-specs">
          <Spec label="Resolution" value={selection.spec.resolution} />
          <Spec label="Frame rate" value={selection.spec.fps} />
          <Spec label="Bitrate" value={selection.spec.bitrate} />
          <Spec label="Codec" value={selection.spec.codec} />
        </div>

        <div className="d-rec" data-tone={rec.tone}>
          <span className="dot" aria-hidden />
          <span className="d-rec-text">{rec.text}</span>
          {/* Raw <button>, not <Button>: this one takes the page's ref, which
              focus returns to when the overlay closes. */}
          {profilesError ? (
            <button type="button" className="btn btn-sm" onClick={onRetryProfiles}>
              Retry
            </button>
          ) : (
            <button
              type="button"
              className="btn btn-sm"
              ref={optionsToggleRef}
              disabled={!model}
              aria-expanded={optionsOpen}
              aria-controls={optionsId}
              onClick={onToggleOptions}
            >
              <IconSliders className="ic" />
              Adjust
            </button>
          )}
        </div>

        <BandNotes
          appName={app.name}
          risky={resolved?.eligibility === "risky"}
          riskyReasons={resolved?.reasons ?? []}
          deadEnd={isDeadEnd}
          deadEndReasons={
            resolved?.reasons?.length ? resolved.reasons : loaded ? blockingReasons(loaded) : []
          }
          isLive={isLive}
          isBlocked={isBlocked}
          blockedByName={blockedByName}
          liveSessionId={liveSessionId}
          canDecodeH264={codecCaps.h264}
          waitingForSlot={waitingForSlot}
          onRetryProfiles={onRetryProfiles}
        />

        <div className="d-actions">
          <Button
            variant="primary"
            size="lg"
            disabled={playDisabled}
            onClick={isLive ? onResume : () => play()}
          >
            <IconPlayGlyph className="ic" />
            {playLabel}
          </Button>
          <button
            type="button"
            className="icon-btn heart"
            aria-label={
              app.favourite ? `Remove ${app.name} from favourites` : `Add ${app.name} to favourites`
            }
            aria-pressed={app.favourite}
            onClick={onToggleFavourite}
          >
            <IconHeart filled={app.favourite} />
          </button>
        </div>
      </div>

      {selection.columns && selection.draft && (
        <LaunchOptions
          id={optionsId}
          open={optionsOpen}
          appName={app.name}
          columns={selection.columns}
          spec={selection.draftSpec}
          verdict={selection.draftVerdict}
          launching={launching}
          waitingForSlot={waitingForSlot}
          closeRef={closeRef}
          onSelectCodec={(codec: DraftCodec) => selection.edit({ codec }, "codec")}
          onSelectFps={(fps) => selection.edit({ fps }, "fps")}
          onSelectHeight={(height) => selection.edit({ height }, "resolution")}
          onCancel={onCloseOptions}
          onPlay={() => play(selection.draft)}
        />
      )}
    </div>
  );
});

function Spec({ label, value }: { label: string; value: string }) {
  return (
    <span className="sp">
      <span className="l">{label}</span>
      <div className="v">{value}</div>
    </span>
  );
}
