// Browser trace-event emitter: POSTs to /v1/sessions/{id}/trace/events
// (control-api.md §B.2). Best-effort (failures swallowed), batched on the same
// 5s cadence as the stats POST.
//
// Never couple emission to rAF (input-batching lesson, CLAUDE.md) — it hitches
// the stream. Event detection uses its own path; flush is a setInterval outside
// the render loop.

/** The v1 browser allow-list of event types (trace-format.md §3.3). */
export type BrowserTraceEventType =
  | "playout.changed"
  | "client.freeze_detected"
  | "client.visibility_changed"
  | "webrtc.state_changed"
  // Bench mode only (?bench=1). Never emitted by an ordinary user session.
  | "bench.window";

/** One client event pending posting. */
interface PendingEvent {
  ts_unix_ms: number;
  type: BrowserTraceEventType;
  payload: Record<string, unknown>;
}

/** Maximum pending events before older ones are discarded (bounded buffer). */
const MAX_PENDING = 128;
/** POST cadence (ms) — same cadence as the stats POST. */
const FLUSH_INTERVAL_MS = 5_000;

/** One instance per session. */
export class TraceEventEmitter {
  private readonly sessionId: string;
  private readonly token: string;
  private readonly pending: PendingEvent[] = [];
  private flushTimer: ReturnType<typeof setInterval> | null = null;
  private stopped = false;

  // Stored so we can remove it on stop().
  private readonly onVisibilityChange: () => void;

  constructor(sessionId: string, token: string) {
    this.sessionId = sessionId;
    this.token = token;

    this.onVisibilityChange = () => {
      const hidden = document.visibilityState === "hidden";
      this.push("client.visibility_changed", { hidden });
    };
  }

  start(): void {
    if (this.flushTimer !== null) return; // idempotent
    document.addEventListener("visibilitychange", this.onVisibilityChange);
    this.flushTimer = setInterval(() => { void this.flush(); }, FLUSH_INTERVAL_MS);
  }

  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    if (this.flushTimer !== null) {
      clearInterval(this.flushTimer);
      this.flushTimer = null;
    }
    document.removeEventListener("visibilitychange", this.onVisibilityChange);
    void this.flush(); // best-effort, fire-and-forget
  }

  // ── Typed event emitters ────────────────────────────────────────────────────

  emitPlayoutChanged(fromMs: number, toMs: number, reason?: string): void {
    const payload: Record<string, unknown> = { from_ms: fromMs, to_ms: toMs };
    if (reason != null) payload["reason"] = reason;
    this.push("playout.changed", payload);
  }

  /**
   * gapMs null means the window produced no cadence (too few RVFC frames, which
   * is what a long freeze looks like) — omit `gap_ms` rather than fabricate one
   * from the counter; a fabricated duration is worse than a missing one.
   */
  emitFreezeDetected(gapMs: number | null, isHidden?: boolean): void {
    const payload: Record<string, unknown> = {};
    if (gapMs != null) payload["gap_ms"] = gapMs;
    if (isHidden != null) payload["is_hidden"] = isHidden;
    this.push("client.freeze_detected", payload);
  }

  emitWebRtcStateChanged(kind: "ice" | "connection", from: string, to: string): void {
    this.push("webrtc.state_changed", { kind, from, to });
  }

  /**
   * Payload is the free-form aggregate from `bench/aggregate.ts`, deliberately
   * NOT a persisted stats key — the stats series is allow-listed per schema.md.
   */
  emitBenchWindow(payload: Record<string, unknown>): void {
    this.push("bench.window", payload);
  }

  // ── Internal ────────────────────────────────────────────────────────────────

  private push(type: BrowserTraceEventType, payload: Record<string, unknown>): void {
    if (this.stopped) return;
    this.pending.push({ ts_unix_ms: Date.now(), type, payload });
    // Bounded buffer: drop oldest if over limit.
    if (this.pending.length > MAX_PENDING) {
      this.pending.splice(0, this.pending.length - MAX_PENDING);
    }
  }

  private async flush(): Promise<void> {
    if (this.pending.length === 0) return;
    // Take up to 64 events, bounded per the control-api.md §B.2 contract.
    const batch = this.pending.splice(0, Math.min(this.pending.length, 64));
    try {
      await fetch(`/v1/sessions/${this.sessionId}/trace/events`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${this.token}`,
        },
        body: JSON.stringify({ client: "browser", events: batch }),
      });
    } catch {
      // Fire-and-forget: trace events never affect session state.
    }
  }
}
