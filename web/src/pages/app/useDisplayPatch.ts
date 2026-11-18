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
// The session resource DOES carry them since amendment 84 (#996): `serverDisplay` is
// what the control plane last knew, adopted on every poll until this tab ACKs a change
// of its own (#1491) — so a second tab or a takeover starts from the session's real
// values instead of the launch size, and a stale sample is corrected by the next poll.
// The last-acked values are still kept per session in sessionStorage, as the fallback
// for a control plane that does not know them yet (an older server, or the first
// seconds after a restart), so a reload never drops back to "match the stream" / 1× in
// that window.
//
// `externalSize`/`externalResizeSupported` are owned by useSessionStatus (its
// 5s poll also updates them); this hook is handed their raw setters and calls
// them directly on ack/revert.
import { useCallback, useEffect, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { updateSessionDisplay } from "../../api/library";
import { ApiError } from "../../api/client";
import type { ToastItem } from "../../components/Toast";

type Size = { w: number; h: number };

const ackedKey = (sessionId: string, field: "render" | "uiScale") => `quasar.session.${sessionId}.${field}`;

/** Read a stored acked value; `parse` returns null for anything the PATCH contract would not accept. */
function loadAcked<T>(sessionId: string | undefined, field: "render" | "uiScale", parse: (v: unknown) => T | null): T | null {
  if (!sessionId) return null;
  try {
    return parse(JSON.parse(sessionStorage.getItem(ackedKey(sessionId, field)) ?? "null"));
  } catch {
    return null; // storage blocked or unparseable: behave as if nothing was acked
  }
}

function saveAcked(sessionId: string, field: "render" | "uiScale", value: unknown) {
  try {
    sessionStorage.setItem(ackedKey(sessionId, field), JSON.stringify(value));
  } catch {
    // best-effort: without it a reload falls back to the launch value, as before
  }
}

// The PATCH contract's own bounds (even, >= 16); the launch-size ceiling is unknown at mount.
const parseRender = (v: unknown): Size | null => {
  const { w, h } = (v ?? {}) as { w?: unknown; h?: unknown };
  const ok = (n: unknown): n is number => Number.isInteger(n) && (n as number) >= 16 && (n as number) % 2 === 0;
  return ok(w) && ok(h) ? { w, h } : null;
};
// ui_scale is within [1.0, 3.0] (openapi.yaml, PATCH /v1/sessions/{id}/display).
const parseUiScale = (v: unknown): number | null =>
  typeof v === "number" && v >= 1 && v <= 3 ? v : null;

/** Settle time before a display change (render resolution / interface size)
 *  is sent. Shared timer/body across controls — two independent debounces on
 *  the same endpoint would race and re-send the loser's field stale. */
const DISPLAY_PATCH_DEBOUNCE_MS = 300;

export interface UseDisplayPatchParams {
  authToken: string | null | undefined;
  sessionId: string | undefined;
  addToast: (item: Omit<ToastItem, "id">) => void;
  /** Amendment 84 (#996): what the session resource reports about the display
   *  state the control plane last knew. Omitted or empty while it knows nothing.
   *  `render: null` is a VALUE — the session default, modelled here as "match
   *  the stream". */
  serverDisplay?: ServerDisplay;
  /** Raw setter from useSessionStatus — called on ack/revert (see module header). */
  setExternalSize: Dispatch<SetStateAction<{ w: number; h: number } | null>>;
  /** Raw setter from useSessionStatus — latched false on a 409 external_resize_unsupported. */
  setExternalResizeSupported: Dispatch<SetStateAction<boolean | undefined>>;
}

/** What the session resource reports about its live display state (amendment 84,
 *  #996). A field is ABSENT when the control plane does not know it — absence is
 *  never "the launch size". */
export interface ServerDisplay {
  render?: { w: number; h: number } | null;
  uiScale?: number;
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
  const { authToken, sessionId, addToast, serverDisplay, setExternalSize, setExternalResizeSupported } = params;

  // null means "match the stream": staying null rather than eagerly copying
  // the stream size means a revert lands on default without knowing it.
  // The initial value is what a reload of this page last had acked (see module header).
  const [initialRender] = useState(() => loadAcked(sessionId, "render", parseRender));
  const [initialUiScale] = useState(() => loadAcked(sessionId, "uiScale", parseUiScale) ?? 1);
  const [renderSize, setRenderSize] = useState<{ w: number; h: number } | null>(initialRender);
  const [uiScale, setUiScale] = useState(initialUiScale);
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
    render: initialRender,
    uiScale: initialUiScale,
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
  // Amendment 84 (#996, #1491): true once THIS tab has ACKed a display change of
  // its own. From then on its acked value is the truth for the adoption effect —
  // a poll sampled before that PATCH landed must never overwrite it again.
  const ackedLocallyRef = useRef(false);

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
        saveAcked(sessionId, "render", ackedDisplayRef.current.render);
      }
      if (body.ui_scale != null) {
        ackedDisplayRef.current.uiScale = body.ui_scale;
        saveAcked(sessionId, "uiScale", body.ui_scale);
      }
      if (body.stream_width != null && body.stream_height != null) {
        ackedDisplayRef.current.external = { w: body.stream_width, h: body.stream_height };
      }
      // A change THIS tab acked is the truth from here, so adoption ends for good
      // (#1491) — a stale poll cannot overwrite the UI, the stored ack or the
      // cursor scaling it set. A stream-only change says nothing about render/scale.
      if (body.render_width != null || body.ui_scale != null) ackedLocallyRef.current = true;
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

  // Amendment 84 (#996, #1491): adopt the session's OWN render size and UI scale on
  // EVERY poll, so a second tab, a reload or a takeover starts from where the session
  // is AND a change another tab makes is picked up, until this tab acks one of its own.
  // Adoption ends for good once THIS
  // tab has acked a change of its own (`ackedLocallyRef`): from then this tab's acked
  // value is the truth, so a poll sampled before that PATCH landed — a delayed poll,
  // the stale metrics sample in the ~1 s window after a change — cannot overwrite the
  // UI, the stored ack or the cursor scaling it set. A change of this tab's that is
  // still pending or in flight DEFERS the adoption, never drops it: the next poll
  // retries the adoption once the PATCH settles. Reported values go through the PATCH
  // contract's own parsers, and a shape they reject is skipped rather than adopted.
  useEffect(() => {
    if (!sessionId || ackedLocallyRef.current || serverDisplay == null) return;
    if (serverDisplay.render === undefined && serverDisplay.uiScale === undefined) return;
    if (displayInFlightRef.current || Object.keys(pendingDisplayRef.current).length > 0) return;
    if (serverDisplay.render !== undefined) {
      const render = parseRender(serverDisplay.render);
      // `null` is a VALUE here — the session default, modelled as "match the stream";
      // only a reported object this tab itself could have sent may replace what is shown.
      if (render !== null || serverDisplay.render === null) {
        ackedDisplayRef.current.render = render;
        saveAcked(sessionId, "render", render);
        setRenderSize(render);
      }
    }
    if (serverDisplay.uiScale !== undefined) {
      const uiScale = parseUiScale(serverDisplay.uiScale);
      if (uiScale !== null) {
        ackedDisplayRef.current.uiScale = uiScale;
        saveAcked(sessionId, "uiScale", uiScale);
        setUiScale(uiScale);
      }
    }
  }, [sessionId, serverDisplay]);

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
