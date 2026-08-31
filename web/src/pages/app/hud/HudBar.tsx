// The HUD's rest state: a 36px pill carrying the connection glyph, the frame
// rate, the app name and the controls a player reaches for (handoff §E).
// Everything that ticks lives in `HudTelemetry`, a memoized child subscribing to
// the fan-out itself (#139), so a 1 Hz snapshot re-renders three numbers.

import { memo, useEffect, useState } from "react";
import type { Ref } from "react";
import { Signal } from "../../../components/Signal";
import { codecDisplayName, compareCodecs } from "../../../lib/codecDisplay";
import { signalQuality, qualityLabelFor } from "../streamHealth";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../../webrtc/telemetry";
import type { HudItems } from "./hudItems";
import type { HudTab } from "./hudKeys";
import {
  IconCapture,
  IconChevron,
  IconExit,
  IconFullscreen,
  IconMicrophone,
  IconTabDisplay,
  IconTabGames,
  IconTabInput,
  IconTabStats,
} from "./icons";

export type TelemetryRegistrar = (fn: (snap: TelemetrySnapshot) => void) => () => void;

export interface HudBarProps {
  barRef: Ref<HTMLDivElement>;
  items: HudItems;
  register: TelemetryRegistrar;
  channelOpen: boolean;
  appPresented: boolean;
  appName: string;
  resolvedCodec?: string;
  open: boolean;
  tab: HudTab;
  /** Single entry point — pressing the showing section collapses (Hud.tsx). */
  onTab: (tab: HudTab) => void;
  onToggleShelf: () => void;
  /** Degrees, from `dockLayout` — the chevron points at what it would do. */
  chevron: number;
  inputCaptured: boolean;
  onGrab: () => void;
  micGranted: boolean;
  micOn: boolean;
  micBusy: boolean;
  onToggleMic: () => void;
  fullscreen: boolean;
  onFullscreen: () => void;
  stopping: boolean;
  onStop: () => void;
  /** Non-null while a quick-switch swap is in flight: replaces the bar. */
  swappingTo: string | null;
  /** Encoded size while it differs from launch (adaptive external resolution). */
  externalSize?: { w: number; h: number } | null;
  /** True while a stream-resolution PATCH is in flight. */
  streamAdapting?: boolean;
}

const TABS: { id: HudTab; label: string; title: string; icon: JSX.Element }[] = [
  { id: "games", label: "Switch game", title: "Switch game", icon: <IconTabGames /> },
  {
    id: "input",
    label: "Controller and input",
    title: "Controller & input",
    icon: <IconTabInput />,
  },
  {
    id: "stats",
    label: "Performance stats",
    title: "Performance stats (⇧S)",
    icon: <IconTabStats />,
  },
  { id: "display", label: "Display", title: "Display", icon: <IconTabDisplay /> },
];

/**
 * The ticking part of the bar. Subscribes on mount and holds the snapshot in
 * its own state, so the fan-out never reaches SessionPage or the shelf (#139).
 */
const HudTelemetry = memo(function HudTelemetry({
  register,
  items,
  channelOpen,
  appPresented,
  resolvedCodec,
}: {
  register: TelemetryRegistrar;
  items: HudItems;
  channelOpen: boolean;
  appPresented: boolean;
  resolvedCodec?: string;
}) {
  const [snap, setSnap] = useState<TelemetrySnapshot>(EMPTY_SNAPSHOT);
  useEffect(() => register(setSnap), [register]);

  const quality = signalQuality({
    presentSdMs: snap.presentSdMs,
    fps: snap.fps,
    packetsLost: snap.packetsLost,
    freezeCount: snap.freezeCount,
    hasDeliveredFrames: snap.framesDecodedTotal > 0,
    // With no input channel nothing ticks, so every counter is zero and the
    // classifier's healthy default would fire on a stream that isn't there.
    mediaPathUp: channelOpen,
    // #484 §3.2: capped at "good" until the app has actually presented.
    appPresented,
  });
  const qLabel = qualityLabelFor(quality, channelOpen);
  const codecs = compareCodecs(resolvedCodec, snap.negotiatedCodec);
  const codecName = codecDisplayName(codecs.resolved ?? codecs.negotiated);
  const mbps = snap.bitrateKbps != null ? snap.bitrateKbps / 1000 : null;

  return (
    <>
      {items.signal && (
        <Signal
          quality={quality}
          className="v-signal"
          label={`Connection is ${qLabel}`}
          title={`Connection quality is ${qLabel}`}
        />
      )}
      {items.metrics && (
        <div className="metrics v-metrics">
          <div className="m">
            <b className="good">
              {snap.fps.toFixed(0)}
              <u>fps</u>
            </b>
          </div>
          <div className="m open-only">
            <b className="info">
              {snap.rttMs != null ? snap.rttMs.toFixed(0) : "–"}
              <u>ms</u>
            </b>
          </div>
          <div className="m open-only">
            <b>
              {mbps != null ? mbps.toFixed(1) : "–"}
              <u>mb</u>
            </b>
          </div>
        </div>
      )}
      {/* Detail the rest pill has no room for: the mock's pill is signal, frame
          rate, title and the controls. Both stay preference-gated on top. */}
      {items.codec && codecName && (
        <span
          className={`codec-chip open-only${codecs.agrees === false ? " warn" : ""}`}
          title={
            codecs.agrees === false
              ? `Server resolved ${codecDisplayName(codecs.resolved)}, browser reports ${codecDisplayName(codecs.negotiated)}`
              : `Video codec is ${codecName}`
          }
        >
          {codecName}
          {codecs.agrees === false && " ⚠"}
        </span>
      )}
    </>
  );
});

export function HudBar(props: HudBarProps) {
  const { items } = props;

  return (
    <div className="hud-bar" ref={props.barRef}>
      {props.swappingTo ? (
        <div className="swapping" role="status">
          <span className="spin" aria-hidden="true" />
          <span>
            Switching to <b>{props.swappingTo}</b>…
          </span>
        </div>
      ) : (
        <>
          {/* Passive readouts only: an operable control inside an
              auto-announcing live region confuses AT users, so the buttons
              are a plain sibling. */}
          <div className="hud-read" role="status" aria-label="Session status">
            <HudTelemetry
              register={props.register}
              items={items}
              channelOpen={props.channelOpen}
              appPresented={props.appPresented}
              resolvedCodec={props.resolvedCodec}
            />
            {/* No mock element covers the adaptive-resolution notice, so it
                reuses the codec chip's treatment. `key` replays the fade so a
                change during play is never silent. */}
            {props.streamAdapting ? (
              <span className="codec-chip extres adapting" title="Changing stream resolution">
                Adapting…
              </span>
            ) : (
              props.externalSize && (
                <span
                  key={`${props.externalSize.w}x${props.externalSize.h}`}
                  className="codec-chip extres"
                  title={`Encoding at ${props.externalSize.w}×${props.externalSize.h}, below this session's launch resolution`}
                >
                  Stream {props.externalSize.w}×{props.externalSize.h}
                </span>
              )
            )}
          </div>

          {items.title && <span className="sep" />}
          {items.title && (
            <span className="title" title="Now playing">
              {props.appName}
            </span>
          )}
          {items.hint && (
            <span className="summon open-only">
              Menu <kbd>Ctrl</kbd>
              <kbd>Alt</kbd>
              <kbd>⇧</kbd>
              <kbd>Q</kbd>
            </span>
          )}

          <div className="hud-ctl">
            {/* aria-selected is only valid on a tab, and a tab needs a
                tablist. The wrapper matches the cluster's own gap, so it adds
                semantics without changing the layout. */}
            <div className="hud-tabs" role="tablist" aria-label="Session menu sections">
              {TABS.map((t) => (
                <button
                  key={t.id}
                  type="button"
                  role="tab"
                  className="ib open-only"
                  data-tab={t.id}
                  title={t.title}
                  aria-label={t.label}
                  aria-selected={props.open && props.tab === t.id}
                  onClick={() => props.onTab(t.id)}
                >
                  {t.icon}
                </button>
              ))}
            </div>
            <span className="div open-only" />

            {/* Contract (openapi.yaml SessionOverlayPreferences): while input
                is captured the actions are inert — the pointer is locked to the
                game and a click could not have been meant for them. Capture is
                the exception: it is the way back out. */}
            {items.capture && (
              <button
                type="button"
                className={`ib${props.inputCaptured ? " on" : ""}`}
                title={props.inputCaptured ? "Release input (Ctrl Alt ⇧ Z)" : "Capture input"}
                aria-label="Capture input"
                aria-pressed={props.inputCaptured}
                disabled={!props.channelOpen}
                onClick={props.onGrab}
              >
                <IconCapture />
              </button>
            )}
            {items.mic && (
              <button
                type="button"
                className={`ib${props.micOn ? " on" : ""}`}
                title={
                  !props.micGranted
                    ? "Microphone disabled by server"
                    : props.micOn
                      ? "Microphone on — click to mute"
                      : "Microphone off — click to talk"
                }
                aria-label="Microphone"
                aria-pressed={props.micOn}
                disabled={!props.micGranted || props.micBusy || props.inputCaptured}
                onClick={props.onToggleMic}
              >
                <IconMicrophone on={props.micOn} />
              </button>
            )}
            {items.fullscreen && (
              <button
                type="button"
                className="ib"
                title={props.fullscreen ? "Exit fullscreen" : "Fullscreen"}
                aria-label="Fullscreen"
                aria-pressed={props.fullscreen}
                disabled={props.inputCaptured}
                onClick={props.onFullscreen}
              >
                <IconFullscreen />
              </button>
            )}

            <span className="div" />
            <button
              type="button"
              className="ib chev"
              title="Menu"
              aria-label={props.open ? "Close menu" : "Open menu"}
              aria-expanded={props.open}
              onClick={props.onToggleShelf}
              style={{ ["--chev-rot" as string]: `${props.chevron}deg` }}
            >
              <IconChevron />
            </button>
            {items.exit && (
              <button
                type="button"
                className="ib danger"
                title="Exit session"
                aria-label="Exit session"
                disabled={props.stopping || props.inputCaptured}
                onClick={props.onStop}
              >
                <IconExit />
              </button>
            )}
          </div>
        </>
      )}
    </div>
  );
}
