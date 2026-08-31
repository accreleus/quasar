// Shelf section 4 — Display (spec §9: the mock has no home for the live
// display controls, and users rely on them, so the shelf gains a fourth tab).
//
// Stream and Render resolution are independent axes (2026-08-16 amendment),
// each bounded only by the session's launch size, never by each other: stream is
// what is encoded and sent (it drops on a bad connection; the app never sees a
// mode change), render is what the app draws into (a `wl_output` mode change it
// must redraw for). The three controls are moved verbatim — debounce, in-flight
// guard and revert-to-last-acked all still live in useDisplayPatch.

import { Chip } from "../../../../components/Chip";
import { RenderResolutionControl, renderResolutionNote } from "../../RenderResolutionControl";
import {
  StreamResolutionControl,
  STREAM_RESOLUTION_NOTE,
  streamRowLabel,
} from "../../StreamResolutionControl";
import { UiScaleControl, UI_SCALE_NOTE } from "../../UiScaleControl";

export const STREAM_RESIZE_UNSUPPORTED_NOTE =
  "This session's encoder can't change stream resolution live.";

/**
 * Whether "Interface size" is offered. ON since 2026-08-19: KWin's nested
 * backend used to re-apply its own remembered output scale on every
 * `outputsQueried`, silently discarding a changed `wp_fractional_scale_v1`
 * hint. Fixed by the quasar-kde kwin patch; proved live in
 * `docs/reports/2026-08-19-kde-ui-scale/`.
 *
 * DESKTOP-only: a client that ignores fractional scale (a game drawing its own
 * UI, an unpatched compositor) sees no effect. Not a user preference — this is
 * a host-image capability gate.
 */
export const SHOW_INTERFACE_SIZE_CONTROL = true;

export interface DisplayPaneProps {
  /** Session's pinned launch size, or null when unknown (a URL-resumed session
   *  carries no launch state). Null hides the rows rather than guessing: the
   *  PATCH validates every dimension against this exact size. */
  streamSize: { w: number; h: number } | null;
  /** Current external (encoded) size, or null while at launch size. */
  externalSize?: { w: number; h: number } | null;
  onStreamSizeChange?: (v: { w: number; h: number }) => void;
  /** `session.stream.rungs`, server-ordered. Rendered as given. */
  streamRungs?: ReadonlyArray<readonly [number, number]>;
  /** `session.stream.external_resize_supported`. Absent means supported; an
   *  explicit false keeps the row visible but inert, with the reason. */
  externalResizeSupported?: boolean;
  /** Who owns the external size: the ABR ladder ("auto") or a user pick. */
  externalOwner?: "auto" | "pinned";
  /** True while a STREAM-resolution change is pending or in flight. */
  streamAdapting?: boolean;
  /** Last-acked render resolution, or null for "match the stream". */
  renderSize: { w: number; h: number } | null;
  onRenderSizeChange: (v: { w: number; h: number }) => void;
  uiScale: number;
  onUiScaleChange: (v: number) => void;
  /** Override for {@link SHOW_INTERFACE_SIZE_CONTROL} — a prop so a test can
   *  exercise the row in either state without mutating module state. */
  showInterfaceSize?: boolean;
  /** True while a display PATCH is in flight — the rows go inert together,
   *  because they share one debounced request. */
  displayBusy: boolean;
}

export function DisplayPane(props: DisplayPaneProps) {
  // Drives the Stream row only. It is not the Render row's ceiling: a stream
  // drop never forces render down.
  const currentStream = props.externalSize ?? props.streamSize;
  const streamRungs = props.streamRungs ?? [];
  // One option and no explanation to give is noise, so the row appears only
  // when there is a choice — or a reason there isn't one.
  const showStreamRow =
    props.streamSize != null &&
    props.onStreamSizeChange != null &&
    (streamRungs.length > 1 || props.externalResizeSupported === false);
  // Render's ceiling is the launch size; falls back to it until an ack lands.
  const renderValue = props.renderSize ?? props.streamSize;

  return (
    <>
      <div className="pane-head">
        <h3>Display</h3>
        <p>
          Changes apply to the running session. The stream keeps playing while the host
          answers.
        </p>
      </div>
      <div className="cols">
        {props.streamSize == null || currentStream == null ? (
          <div className="col-note">
            This session did not report a stream size, so its display controls are
            unavailable. Relaunch from the library to get them back.
          </div>
        ) : (
          <>
            <div>
              <div className="col-lb">Stream resolution</div>
              {showStreamRow ? (
                <>
                  <div className="ctl-row">
                    {props.externalOwner === "auto" && (
                      <Chip variant="neutral">
                        {streamRowLabel("auto", currentStream.w, currentStream.h)}
                      </Chip>
                    )}
                    <StreamResolutionControl
                      launch={props.streamSize}
                      rungs={streamRungs}
                      value={currentStream}
                      onChange={props.onStreamSizeChange!}
                      busy={props.displayBusy}
                      owner={props.externalOwner}
                      disabledReason={
                        props.externalResizeSupported === false
                          ? STREAM_RESIZE_UNSUPPORTED_NOTE
                          : undefined
                      }
                    />
                  </div>
                  {props.streamAdapting && (
                    <p className="col-note" role="status">
                      Adapting…
                    </p>
                  )}
                  <p className="col-note">{STREAM_RESOLUTION_NOTE}</p>
                  {props.externalResizeSupported === false && (
                    <p className="col-note">{STREAM_RESIZE_UNSUPPORTED_NOTE}</p>
                  )}
                </>
              ) : (
                <p className="col-note">
                  This session has one stream resolution, so there is nothing to choose.
                </p>
              )}
            </div>

            <div>
              <div className="col-lb">Render resolution</div>
              <div className="ctl-row">
                <RenderResolutionControl
                  streamWidth={props.streamSize.w}
                  streamHeight={props.streamSize.h}
                  value={renderValue ?? props.streamSize}
                  onChange={props.onRenderSizeChange}
                  busy={props.displayBusy}
                />
              </div>
              <p className="col-note">
                {renderResolutionNote(props.streamSize.w, props.streamSize.h)}
              </p>
            </div>

            {/* The row and its caption are gated together; a caption for an
                absent control would be nonsense. */}
            {(props.showInterfaceSize ?? SHOW_INTERFACE_SIZE_CONTROL) && (
              <div>
                <div className="col-lb">Interface size</div>
                <div className="ctl-row">
                  <UiScaleControl
                    value={props.uiScale}
                    onChange={props.onUiScaleChange}
                    busy={props.displayBusy}
                  />
                </div>
                <p className="col-note">{UI_SCALE_NOTE}</p>
              </div>
            )}
          </>
        )}
      </div>
    </>
  );
}
