// Live preview of the in-session HUD, for /app/account/overlay.
//
// Operator request 2026-08-05: "show a preview... so people see what it looks
// like from the sessions screen." This embeds the real `Hud` (in its `preview`
// mode: no document listeners, no idle clock, no keyboard map) rather than a
// redrawn copy — a second implementation would silently drift out of sync the
// next time the real one changes. The only new work here is a frame to sit it
// in and a static telemetry snapshot to feed it.
//
// The HUD reads its content/position/auto-hide from OverlayPreferencesContext.
// AccountOverlay (this component's only caller) is already nested inside that
// provider (OverlayPreferencesRoute wraps /app/account/*), which is what makes
// the preview update live as the user flips a switch below it.

import { useCallback, useEffect, useLayoutEffect, useRef } from "react";
import { Hud } from "../hud/Hud";
import { useOverlayPreferences } from "../../../settings/OverlayPreferencesContext";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../../webrtc/telemetry";
// The HUD's classes live in hud.css, normally loaded only by the live session
// route — the account page never mounts it, so the preview imports the
// stylesheet itself. Safe to import twice (Vite dedupes identical imports);
// this file must not add rules to hud.css, which is for in-session surfaces.
import "../../../styles/hud.css";

/** One static, plausible, healthy-looking snapshot. A preview must not
 *  animate or imply live data, so this is pushed once and never again — see
 *  registerPreviewTelemetry below. Built from EMPTY_SNAPSHOT so it inherits
 *  the real shape (every field SessionStrip might read) rather than a
 *  hand-rolled partial that silently omits one. */
const PREVIEW_SNAPSHOT: TelemetrySnapshot = {
  ...EMPTY_SNAPSHOT,
  fps: 60,
  bitrateKbps: 8400,
  rttMs: 18,
  presentFps: 60,
  presentSdMs: 2,
  presentP95Ms: 4,
  packetsLost: 0,
  freezeCount: 0,
  framesDecodedTotal: 900,
  negotiatedCodec: "video/H264",
};

const PREVIEW_APP_NAME = "Nebula Raceway";
const PREVIEW_TIER = "1920×1080@60";
const PREVIEW_CODEC = "h264";

/** Pushes PREVIEW_SNAPSHOT once on subscribe and never updates; the returned
 *  unsubscribe is a real (no-op) function, matching the registrar contract.
 *  Declared at module scope so it is referentially stable across every render —
 *  the bar readout's `useEffect(() => register(setSnap), [register])` would
 *  otherwise re-subscribe on every parent re-render. */
function registerPreviewTelemetry(push: (snap: TelemetrySnapshot) => void): () => void {
  push(PREVIEW_SNAPSHOT);
  return () => {};
}

function noop(): void {}

export function OverlayPreview() {
  const { prefs } = useOverlayPreferences();
  const stageRef = useRef<HTMLDivElement>(null);
  const scaleRef = useRef<HTMLDivElement>(null);

  /**
   * Scale the strip down when it is wider than the stage, and never up.
   *
   * The pill's width is content-driven: every item the user switches on makes
   * it wider, and at "Full" on a wide account page it measured 766px against a
   * 480px frame — so it was being painted-clipped at both ends, showing the
   * middle of a strip and cutting off the very actions this preview exists to
   * show. A fixed frame width cannot fix that: the strip's width depends on the
   * preference set AND on the app name, so any constant is wrong for some
   * combination. Measuring is the only thing that holds at every viewport and
   * every item set.
   *
   * offsetWidth, not getBoundingClientRect: the latter reports the POST-transform
   * box, so reading it here would feed the scale back into itself and converge on
   * nonsense. offsetWidth is the pre-transform layout width.
   *
   * Capped at 1 — a strip narrower than the stage (say Minimal) stays at its
   * true size rather than being blown up, because how big the strip looks
   * relative to a screen is itself part of what is being previewed.
   */
  const fit = useCallback(() => {
    const stage = stageRef.current;
    const scale = scaleRef.current;
    const strip = scale?.querySelector<HTMLElement>(".hud");
    if (!stage || !scale || !strip) return;
    const natural = strip.offsetWidth;
    if (!natural) return;
    // Breathing room so a scaled strip never touches the frame edge.
    const available = stage.clientWidth - 32;
    scale.style.setProperty("--ovprev-k", String(Math.min(1, available / natural)));
  }, []);

  // Re-fit on every render (an item toggle changes the strip's width) and on
  // any stage resize (window/sidebar). useLayoutEffect so the scale is applied
  // before paint — with useEffect the first frame shows the strip overflowing.
  useLayoutEffect(fit);
  useEffect(() => {
    const stage = stageRef.current;
    if (!stage || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(fit);
    ro.observe(stage);
    return () => ro.disconnect();
  }, [fit]);

  // Accessibility mechanism (no <inert> under this React 18 / @types/react
  // setup): the embedded strip's buttons must be visible — they're the point
  // of the preview — but not operable and not reachable by Tab out of the
  // settings form above/below them. `inert` would give all three properties
  // (removed from the accessibility tree, unfocusable, unclickable) in one
  // attribute; without it they're reproduced by hand:
  //   - aria-hidden="true" on the stage (below) removes the whole replica
  //     from the accessibility tree, so a screen-reader user gets nothing
  //     from inside it — the caption text outside the stage is the only
  //     thing announced, i.e. the preview reads as one illustrative image.
  //   - this effect sweeps tabindex="-1" onto every focusable descendant so
  //     Tab skips over the fake controls entirely. aria-hidden alone does
  //     NOT do this (a focusable-but-aria-hidden element is a known
  //     anti-pattern: it can still take keyboard focus while invisible to
  //     AT), so the sweep is load-bearing, not redundant.
  //   - the stage's CSS (.ovprev-stage, components.css) sets
  //     pointer-events: none, so a click can't reach onGrab/onStop/etc.
  //     either — belt-and-braces, since those handlers are no-ops anyway.
  // Re-run on every render with no dependency array: toggling a strip-item
  // switch (Capture/Mic/Fullscreen/Exit) changes which buttons SessionStrip
  // renders, and a newly-mounted button would otherwise land back in the tab
  // order until the next unrelated re-render swept it.
  useEffect(() => {
    const stage = stageRef.current;
    if (!stage) return;
    stage.querySelectorAll<HTMLElement>("button, [tabindex]").forEach((el) => {
      el.setAttribute("tabindex", "-1");
    });
  });

  // The HUD's own preference rule hides it outright under "Never show". A frame
  // around nothing would misrepresent the very setting being previewed, so that
  // state gets an explicit placeholder instead of a faked-visible pill.
  if (prefs.stripAutoHide === "never_visible") {
    return (
      <div className="field mt5">
        <span className="label">Live preview</span>
        <div className="ovprev-stage ovprev-off">
          <p>
            With the overlay off, only the microphone indicator is drawn over your session.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="field mt5">
      <span className="label">Live preview</span>
      <p className="muted mt3">
        What the HUD looks like over a running session, using your current settings below. The
        controls shown are illustrative only.
      </p>
      <div className="ovprev-stage" ref={stageRef} aria-hidden="true">
        {/* The scale layer is also what the fixed-position HUD resolves
            against: a transformed ancestor becomes the containing block for
            fixed descendants, exactly as `contain: layout` on the stage does.
            Both are present on purpose — the stage's containment still holds
            when the scale is 1. */}
        <div className="ovprev-scale" ref={scaleRef}>
          <Hud
            preview
            register={registerPreviewTelemetry}
            channelOpen
            appPresented
            appName={PREVIEW_APP_NAME}
            tier={PREVIEW_TIER}
            resolvedCodec={PREVIEW_CODEC}
            inputCaptured={false}
            onGrab={noop}
            onRelease={noop}
            escReleases={false}
            escInsecureContext={false}
            pointerLockAvailable
            touchLook={false}
            // Granted, so the mic button previews in its normal enabled state
            // rather than the server-disabled look.
            micGranted
            micOn={false}
            micBusy={false}
            onToggleMic={noop}
            fullscreen={false}
            onFullscreen={noop}
            stopping={false}
            onStop={noop}
            swappingTo={null}
            games={null}
            scalingMode="contain"
            onScalingChange={noop}
            displayHzWarning={null}
            streamSize={null}
            renderSize={null}
            onRenderSizeChange={noop}
            uiScale={1}
            onUiScaleChange={noop}
            displayBusy={false}
          />
        </div>
      </div>
    </div>
  );
}
