// Render resolution + interface size (control-api.md session-display-update).
// Extracted verbatim out of SessionPage: same debounce, single-in-flight
// guard, partial ack, converge-on-success, revert-all-on-failure, and
// `external_resize_unsupported` latch.
//
// Unlike scaling (SessionPage's AS10-14 `scalingMode`), these are HOST-side
// and ephemeral — the agent holds them, nothing persists them, the 202 body
// doesn't carry them back — so this hook keeps the last server-ACKED value
// and treats its own state as optimistic until then.
//
// `externalSize`/`externalResizeSupported` are owned by useSessionStatus (its
// 5s poll also updates them); this hook is handed their raw setters and calls
// them directly on ack/revert.
import { useCallback, useEffect, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { updateSessionDisplay } from "../../api/library";
import { ApiError } from "../../api/client";
import type { ToastItem } from "../../components/Toast";

/** Settle time before a display change (render resolution / interface size)
 *  is sent. Shared timer/body across controls — two independent debounces on
 *  the same endpoint would race and re-send the loser's field stale. */
const DISPLAY_PATCH_DEBOUNCE_MS = 300;

export interface UseDisplayPatchParams {
  authToken: string | null | undefined;
  sessionId: string | undefined;
  addToast: (item: Omit<ToastItem, "id">) => void;
  /** Raw setter from useSessionStatus — called on ack/revert (see module header). */
  setExternalSize: Dispatch<SetStateAction<{ w: number; h: number } | null>>;
  /** Raw setter from useSessionStatus — latched false on a 409 external_resize_unsupported. */
  setExternalResizeSupported: Dispatch<SetStateAction<boolean | undefined>>;
}

export interface UseDisplayPatchResult {
  /** `null` render size means "match the stream" — the session default. */
  renderSize: { w: number; h: number } | null;
  uiScale: number;
  displayBusy: boolean;
  /** True while a STREAM-resolution change is pending or in flight — drives the strip's "Adapting…" badge. */
  streamChanging: boolean;
  handleRenderSizeChange: (v: { w: number; h: number }) => void;
  handleStreamSizeChange: (v: { w: number; h: number }) => void;
  handleUiScaleChange: (v: number) => void;
}

export function useDisplayPatch(params: UseDisplayPatchParams): UseDisplayPatchResult {
  const { authToken, sessionId, addToast, setExternalSize, setExternalResizeSupported } = params;

  // null means "match the stream": staying null rather than eagerly copying
  // the stream size means a revert lands on default without knowing it.
  const [renderSize, setRenderSize] = useState<{ w: number; h: number } | null>(null);
  const [uiScale, setUiScale] = useState(1);
  const [displayBusy, setDisplayBusy] = useState(false);
  // Distinct from displayBusy (true for any display PATCH, including render-res).
  const [streamChanging, setStreamChanging] = useState(false);
  // Last server-accepted state. A 409 rejection is contractually a NO-OP, so
  // reverting to this is correct, not merely cosmetic.
  const ackedDisplayRef = useRef<{
    render: { w: number; h: number } | null;
    uiScale: number;
    /** Last-acked EXTERNAL (encoded) size; null = the launch size. */
    external: { w: number; h: number } | null;
  }>({
    render: null,
    uiScale: 1,
    external: null,
  });
  // One shared timer + one merged body for BOTH controls — see
  // DISPLAY_PATCH_DEBOUNCE_MS. Fields accumulate, so changing resolution and
  // then scale within the window sends one PATCH carrying both.
  const displayTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const pendingDisplayRef = useRef<{
    render_width?: number;
    render_height?: number;
    ui_scale?: number;
    stream_width?: number;
    stream_height?: number;
  }>({});
  // Invariant: at most one display PATCH in flight. The endpoint is
  // last-write-wins on the host, so overlapping requests could ack out of
  // order. A ref, not `displayBusy`: the guard must be exact at call time,
  // and state updates are async.
  const displayInFlightRef = useRef(false);
  // Breaks the flush ⇄ arm cycle (each needs to call the other) without making
  // either a dependency of the other. Kept current by the effect below.
  const flushDisplayRef = useRef<() => void>(() => {});

  const armDisplayTimer = useCallback(() => {
    if (displayTimerRef.current) clearTimeout(displayTimerRef.current);
    displayTimerRef.current = setTimeout(() => {
      displayTimerRef.current = null;
      flushDisplayRef.current();
    }, DISPLAY_PATCH_DEBOUNCE_MS);
  }, []);

  const flushDisplayPatch = useCallback(async () => {
    // Single in-flight: re-arm and let the settled request's own tail pick the
    // pending body up. Never drop it.
    if (displayInFlightRef.current) {
      armDisplayTimer();
      return;
    }
    const body = pendingDisplayRef.current;
    pendingDisplayRef.current = {};
    if (!authToken || !sessionId || Object.keys(body).length === 0) {
      setStreamChanging(false);
      return;
    }
    displayInFlightRef.current = true;
    setDisplayBusy(true);
    try {
      await updateSessionDisplay(authToken, sessionId, body);
      // Ack only what this request actually carried — a partial update leaves
      // the other field's acked value alone.
      if (body.render_width != null && body.render_height != null) {
        ackedDisplayRef.current.render = { w: body.render_width, h: body.render_height };
      }
      if (body.ui_scale != null) ackedDisplayRef.current.uiScale = body.ui_scale;
      if (body.stream_width != null && body.stream_height != null) {
        ackedDisplayRef.current.external = { w: body.stream_width, h: body.stream_height };
      }
      // Converge on success too: acked state becomes displayed state so they
      // never drift. Skipped if a newer edit is already queued (would stomp it).
      if (Object.keys(pendingDisplayRef.current).length === 0) {
        setRenderSize(ackedDisplayRef.current.render);
        setUiScale(ackedDisplayRef.current.uiScale);
        setExternalSize(ackedDisplayRef.current.external);
      }
    } catch (err) {
      // Revert EVERY control, not just the rejected field: a merged body can
      // carry all three, and the whole PATCH failed as a unit.
      setRenderSize(ackedDisplayRef.current.render);
      setUiScale(ackedDisplayRef.current.uiScale);
      setExternalSize(ackedDisplayRef.current.external);
      // external_resize_unsupported (§D5) isn't transient — this host+codec
      // will never accept a live resize — so it gets its own message and
      // latches the control inert.
      const unsupported =
        err instanceof ApiError && err.code === "external_resize_unsupported";
      if (unsupported) setExternalResizeSupported(false);
      addToast({
        variant: "danger",
        title: unsupported ? "Stream resolution unchanged" : "Display change not applied",
        body: unsupported
          ? "This session's encoder can't change stream resolution live"
          : err instanceof ApiError
            ? err.message
            : "The host could not apply the change. The session is unchanged.",
      });
    } finally {
      displayInFlightRef.current = false;
      setDisplayBusy(false);
      // The "Adapting…" badge stays up only while a stream change is still
      // owed — either queued behind this request, or about to be re-sent.
      if (pendingDisplayRef.current.stream_width == null) setStreamChanging(false);
      // Anything queued while this was in flight goes out now (after its own
      // debounce window), so a deferred change is delayed, never lost.
      if (Object.keys(pendingDisplayRef.current).length > 0) armDisplayTimer();
    }
  }, [authToken, sessionId, addToast, armDisplayTimer, setExternalSize, setExternalResizeSupported]);

  useEffect(() => {
    flushDisplayRef.current = () => void flushDisplayPatch();
  }, [flushDisplayPatch]);

  const queueDisplayPatch = useCallback(
    (patch: {
      render_width?: number;
      render_height?: number;
      ui_scale?: number;
      stream_width?: number;
      stream_height?: number;
    }) => {
      Object.assign(pendingDisplayRef.current, patch);
      armDisplayTimer();
    },
    [armDisplayTimer],
  );

  const handleRenderSizeChange = useCallback(
    (v: { w: number; h: number }) => {
      setRenderSize(v);
      // Both dims always, never one: the contract is both-or-neither.
      queueDisplayPatch({ render_width: v.w, render_height: v.h });
    },
    [queueDisplayPatch],
  );

  // Same debounce/ack/revert machinery as above — all three PATCH the same
  // endpoint, so a separate pipeline would race this one.
  const handleStreamSizeChange = useCallback(
    (v: { w: number; h: number }) => {
      setExternalSize(v);
      setStreamChanging(true);
      // Both dims always, never one: the contract is both-or-neither.
      queueDisplayPatch({ stream_width: v.w, stream_height: v.h });
    },
    [queueDisplayPatch, setExternalSize],
  );

  const handleUiScaleChange = useCallback(
    (v: number) => {
      setUiScale(v);
      queueDisplayPatch({ ui_scale: v });
    },
    [queueDisplayPatch],
  );

  // Drop a pending debounce on unmount — the page is gone and, on the stop
  // path, so is the session.
  useEffect(() => {
    return () => {
      if (displayTimerRef.current) clearTimeout(displayTimerRef.current);
    };
  }, []);

  return {
    renderSize,
    uiScale,
    displayBusy,
    streamChanging,
    handleRenderSizeChange,
    handleStreamSizeChange,
    handleUiScaleChange,
  };
}
