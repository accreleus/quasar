// The session HUD: one object, dockable to any edge, that morphs between a rest
// pill and a full-edge shelf (handoff §E). #139: no telemetry lands in this
// component's state — the bar readout and each pane subscribe for themselves.

import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ReactNode } from "react";
import { useOverlayPreferences } from "../../../settings/OverlayPreferencesContext";
import type { ScalingMode } from "../../../settings/displayPreferences";
import { dockLayout, hudShellPin } from "./hudDock";
import { HUD_IDLE_HIDE_MS, hudVisible } from "./hudAutoHide";
import { hudKeyAction, stepTab, type HudKeyAction, type HudTab } from "./hudKeys";
import { hudItems } from "./hudItems";
import { HudBar, type TelemetryRegistrar } from "./HudBar";
import { Shelf } from "./Shelf";
import { InputPane } from "./panes/InputPane";
import { StatsPane } from "./panes/StatsPane";
import { DisplayPane } from "./panes/DisplayPane";

/** What SessionPage can ask the HUD to do from outside: the summon chord that
 *  capture.ts answers while the pointer is locked, and the stage click. */
export interface HudHandle {
  open: (tab?: HudTab) => void;
  close: () => void;
  stageClick: () => void;
}

export interface HudProps {
  register: TelemetryRegistrar;
  channelOpen: boolean;
  appPresented: boolean;
  appName: string;
  tier?: string;
  resolvedCodec?: string;
  sessionId?: string;

  inputCaptured: boolean;
  /** Capture toggle — engages when released, releases when captured. */
  onGrab: () => void;
  /** Release only, no-op when already released. Used before the shelf opens:
   *  a menu behind a captured keyboard fights Tab and Enter with the game. */
  onRelease: () => void;
  escReleases: boolean;
  escInsecureContext: boolean;
  /** Certificate bypassed rather than trusted (lib/certTrust.ts); selects the
   *  trust-the-certificate Esc wording. Only SessionPage supplies it. */
  escCertUntrusted?: boolean;
  /** keyboard.lock() was observed refused this session. */
  escLockRefused?: boolean;
  pointerLockAvailable: boolean;
  touchLook: boolean;

  micGranted: boolean;
  micOn: boolean;
  micBusy: boolean;
  onToggleMic: () => void;

  fullscreen: boolean;
  onFullscreen: () => void;

  stopping: boolean;
  onStop: () => void;

  /** Non-null while a quick-switch swap is in flight. */
  swappingTo: string | null;
  /** The Games pane, injected so the HUD stays ignorant of the swap API. */
  games: ReactNode;

  scalingMode: ScalingMode;
  onScalingChange: (m: ScalingMode) => void;
  displayHzWarning: { displayHz: number; streamFps: number } | null;
  /** `session.started_at`, for the stats pane's elapsed row. */
  startedAt?: string | null;

  streamSize: { w: number; h: number } | null;
  externalSize?: { w: number; h: number } | null;
  onStreamSizeChange?: (v: { w: number; h: number }) => void;
  streamRungs?: ReadonlyArray<readonly [number, number]>;
  externalResizeSupported?: boolean;
  externalOwner?: "auto" | "pinned";
  streamAdapting?: boolean;
  renderSize: { w: number; h: number } | null;
  onRenderSizeChange: (v: { w: number; h: number }) => void;
  uiScale: number;
  onUiScaleChange: (v: number) => void;
  displayBusy: boolean;
  showInterfaceSize?: boolean;

  /** Encoded size for the bar badge — only when it differs from launch. */
  badgeExternalSize?: { w: number; h: number } | null;

  /** Told when the shelf opens or closes, so the owner can dim the stage. */
  onOpenChange?: (open: boolean) => void;

  /**
   * Account-page preview (`#/app/account/overlay`): renders the rest pill with
   * whatever telemetry the caller pushes, and installs no document listeners,
   * no idle clock and no keyboard map. A settings page must not be able to
   * swallow Escape or hide its own preview.
   */
  preview?: boolean;
}

/** Whether a key press landed in a field that owns its own keystrokes. */
function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return (
    target.isContentEditable || ["INPUT", "SELECT", "TEXTAREA"].includes(target.tagName)
  );
}

/** Shortcuts a field swallows: a bare letter or arrow is text entry there. The
 *  chords and Escape are not, so they still reach the HUD. */
const FIELD_SWALLOWS: ReadonlySet<HudKeyAction> = new Set(["stats", "next-tab", "prev-tab"]);

export const Hud = forwardRef<HudHandle, HudProps>(function Hud(props, ref) {
  const { prefs } = useOverlayPreferences();
  // Memoised: HudTelemetry takes `items` by identity, and a fresh object every
  // render would defeat its memo and put the 1 Hz readout back on the parent's
  // render path.
  const items = useMemo(() => hudItems(prefs.stripItems), [prefs.stripItems]);
  const dock = prefs.stripPosition;

  const [open, setOpen] = useState(false);
  const [tab, setTab] = useState<HudTab>("games");
  const [hidden, setHidden] = useState(false);

  const rootRef = useRef<HTMLDivElement>(null);
  const hudRef = useRef<HTMLDivElement>(null);
  const barRef = useRef<HTMLDivElement>(null);
  const shelfRef = useRef<HTMLDivElement>(null);
  const measuringRef = useRef(false);

  const layout = dockLayout(dock, open);

  const onOpenChange = props.onOpenChange;
  useEffect(() => {
    onOpenChange?.(open);
  }, [open, onOpenChange]);

  const onRelease = props.onRelease;
  const inputCaptured = props.inputCaptured;

  /** The single entry point; opening the section already showing collapses.
   *  Releases capture first: a shelf behind a locked pointer cannot be clicked,
   *  and a captured keyboard would fight the menu for Tab and Enter. */
  const openShelf = useCallback(
    (name: HudTab) => {
      if (open && name === tab) {
        setOpen(false);
        return;
      }
      if (!open && inputCaptured) onRelease();
      setTab(name);
      setOpen(true);
    },
    [open, tab, inputCaptured, onRelease],
  );

  const closeShelf = useCallback(() => setOpen(false), []);

  const toggleShelf = useCallback(() => {
    if (open) closeShelf();
    else openShelf(tab);
  }, [open, tab, openShelf, closeShelf]);

  useImperativeHandle(
    ref,
    () => ({
      open: (name?: HudTab) => {
        if (inputCaptured) onRelease();
        setTab(name ?? tab);
        setOpen(true);
      },
      close: closeShelf,
      // Clicking the picture summons the shelf only when input is not
      // captured: captured clicks belong to the game. This is the only pointer
      // route in for a user who doesn't know the chord.
      stageClick: () => {
        if (inputCaptured) return;
        setOpen(true);
      },
    }),
    [inputCaptured, onRelease, tab, closeShelf],
  );

  // Capture engaging closes the shelf — the mock's `setCapture(true)` rule.
  // The picture is the point of capturing; a menu over it is not.
  useEffect(() => {
    if (inputCaptured) setOpen(false);
  }, [inputCaptured]);

  // The collapsed pill is as big as its content, which changes with state,
  // preferences and orientation. Measure the bar alone — the shelf's much
  // larger intrinsic size must not contribute — and cache it, so the morph is a
  // plain width/height transition along the docked axis.
  const measure = useCallback(() => {
    const root = rootRef.current;
    const hud = hudRef.current;
    const bar = barRef.current;
    const shelf = shelfRef.current;
    if (!root || !hud || !bar || !shelf) return;
    if (measuringRef.current) return;
    measuringRef.current = true;

    const wasOpen = root.dataset.open === "true";
    const vertical = root.dataset.axis === "v";
    root.dataset.open = "false";
    hud.style.transition = "none";
    hud.style.setProperty("--hud-w", "auto");
    hud.style.setProperty("--hud-h", "auto");
    shelf.style.display = "none";
    if (!vertical) bar.style.width = "max-content";

    const r = bar.getBoundingClientRect();

    bar.style.width = "";
    shelf.style.display = "";
    // Both clamped against the same measurement: the pill never exceeds the
    // viewport, and the open state is never smaller than the bar needs, which
    // would make the morph read as a shrink.
    const pin = hudShellPin(r);
    if (pin) {
      hud.style.setProperty("--hud-w", pin.w);
      hud.style.setProperty("--hud-h", pin.h);
    }
    if (wasOpen) root.dataset.open = "true";
    void hud.offsetWidth;
    hud.style.transition = "";
    measuringRef.current = false;
  }, []);

  // Re-measure whenever the bar's content can have changed size: a longer app
  // name, a swap line, a readout settling on its final text. It observes the
  // bar's descendants, not the bar, because `--hud-w` pins the bar's own box —
  // observing that alone reports nothing and the pill keeps its first-paint
  // width, ellipsising the title and clipping "fps" to "fp". The listener below
  // covers the viewport and late webfonts, which no observer sees. No
  // dependency array: a swap replaces the bar's children. A measurement is
  // layout, not telemetry — nothing here reaches React state (#139).
  useLayoutEffect(() => {
    // Same rule as the observer callback below: open, the pill size is unused
    // — and measure() toggles the shelf's display, which REPLAYS the pane's
    // entry animation, so a per-render measure while open blanked the open
    // pane on every parent re-render (#78). Closing re-renders, which re-runs
    // this effect with open=false and measures then.
    if (!open) measure();
    if (typeof ResizeObserver === "undefined") return;
    let frame = 0;
    const ro = new ResizeObserver(() => {
      // Open, the bar spans its whole edge and the cached pill size is unused,
      // so the 1 Hz readouts would only buy a reflow a second. Closing
      // re-renders, which re-runs this effect's measure().
      if (open) return;
      // One measurement per frame: a single text change moves several boxes at
      // once, and measure() forces a reflow.
      if (frame) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        measure();
      });
    });
    const bar = barRef.current;
    if (bar) {
      ro.observe(bar);
      for (const el of Array.from(bar.querySelectorAll("*"))) ro.observe(el);
    }
    return () => {
      if (frame) cancelAnimationFrame(frame);
      ro.disconnect();
    };
  });

  useEffect(() => {
    const onResize = () => measure();
    window.addEventListener("resize", onResize);
    const fonts = (document as Document & { fonts?: FontFaceSet }).fonts;
    void fonts?.ready?.then(() => measure());
    return () => window.removeEventListener("resize", onResize);
  }, [measure]);

  // One effect owns visibility so preference, capture, open and swap cannot
  // disagree. No "reappear on activity" rule: while captured every pointer and
  // key event is gameplay, so waking on input would mean it never hides.
  const autoHide = prefs.stripAutoHide;
  const swapping = props.swappingTo != null;
  useEffect(() => {
    if (props.preview) {
      setHidden(false);
      return;
    }
    const now = hudVisible({ mode: autoHide, captured: inputCaptured, open, swapping, idleMs: 0 });
    if (!now) {
      setHidden(true);
      return;
    }
    setHidden(false);
    const later = hudVisible({
      mode: autoHide,
      captured: inputCaptured,
      open,
      swapping,
      idleMs: HUD_IDLE_HIDE_MS + 1,
    });
    if (later) return;
    const timer = setTimeout(() => setHidden(true), HUD_IDLE_HIDE_MS);
    return () => clearTimeout(timer);
  }, [autoHide, inputCaptured, open, swapping, props.preview]);

  useEffect(() => {
    if (props.preview) return;
    const onKeyDown = (e: KeyboardEvent) => {
      const action = hudKeyAction(e, { open, captured: inputCaptured });
      if (!action) return;
      // Otherwise the Display pane's selects cannot be driven from the keyboard.
      if (FIELD_SWALLOWS.has(action) && isEditableTarget(e.target)) return;
      switch (action) {
        case "close":
          e.preventDefault();
          closeShelf();
          return;
        case "release-and-open":
          // While the pointer is locked, capture.ts owns this chord: it must
          // release the lock and swallow the key before the game sees it, then
          // calls back through the imperative handle. Mutually exclusive on
          // `document.pointerLockElement`, so one press never opens twice.
          if (document.pointerLockElement) return;
          e.preventDefault();
          openShelf(tab);
          return;
        case "release":
          e.preventDefault();
          onRelease();
          return;
        case "stats":
          openShelf("stats");
          return;
        case "next-tab":
          openShelf(stepTab(tab, 1));
          return;
        case "prev-tab":
          openShelf(stepTab(tab, -1));
          return;
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open, tab, inputCaptured, openShelf, closeShelf, onRelease, props.preview]);

  const pane = (): ReactNode => {
    switch (tab) {
      case "games":
        return props.games;
      case "input":
        return (
          <InputPane
            register={props.register}
            channelOpen={props.channelOpen}
            inputCaptured={props.inputCaptured}
            onGrab={props.onGrab}
            escReleases={props.escReleases}
            escInsecureContext={props.escInsecureContext}
            escCertUntrusted={props.escCertUntrusted}
            escLockRefused={props.escLockRefused}
            pointerLockAvailable={props.pointerLockAvailable}
            touchLook={props.touchLook}
            scalingMode={props.scalingMode}
            onScalingChange={props.onScalingChange}
          />
        );
      case "stats":
        return (
          <StatsPane
            register={props.register}
            tier={props.tier}
            resolvedCodec={props.resolvedCodec}
            sessionId={props.sessionId}
            displayHzWarning={props.displayHzWarning}
            startedAt={props.startedAt}
          />
        );
      case "display":
        return (
          <DisplayPane
            streamSize={props.streamSize}
            externalSize={props.externalSize}
            onStreamSizeChange={props.onStreamSizeChange}
            streamRungs={props.streamRungs}
            externalResizeSupported={props.externalResizeSupported}
            externalOwner={props.externalOwner}
            streamAdapting={props.streamAdapting}
            renderSize={props.renderSize}
            onRenderSizeChange={props.onRenderSizeChange}
            uiScale={props.uiScale}
            onUiScaleChange={props.onUiScaleChange}
            displayBusy={props.displayBusy}
            showInterfaceSize={props.showInterfaceSize}
          />
        );
    }
  };

  return (
    <div
      className="hud-root"
      ref={rootRef}
      data-pos={dock}
      data-axis={layout.axis}
      data-open={open ? "true" : "false"}
      data-shelf={tab}
      data-swapping={swapping ? "true" : undefined}
    >
      <div
        className={`hud${hidden ? " hidden" : ""}`}
        ref={hudRef}
        style={{
          flexDirection: layout.direction,
          width: layout.width,
          height: layout.height,
          borderRadius: layout.radius,
        }}
      >
        <HudBar
          barRef={barRef}
          items={items}
          register={props.register}
          channelOpen={props.channelOpen}
          appPresented={props.appPresented}
          appName={props.appName}
          resolvedCodec={props.resolvedCodec}
          open={open}
          tab={tab}
          onTab={openShelf}
          onToggleShelf={toggleShelf}
          chevron={layout.chevron}
          inputCaptured={props.inputCaptured}
          onGrab={props.onGrab}
          micGranted={props.micGranted}
          micOn={props.micOn}
          micBusy={props.micBusy}
          onToggleMic={props.onToggleMic}
          fullscreen={props.fullscreen}
          onFullscreen={props.onFullscreen}
          stopping={props.stopping}
          onStop={props.onStop}
          swappingTo={props.swappingTo}
          externalSize={props.badgeExternalSize}
          streamAdapting={props.streamAdapting}
        />
        <Shelf tab={tab} open={open} shelfRef={shelfRef}>
          {open ? pane() : null}
        </Shelf>
      </div>
    </div>
  );
});
