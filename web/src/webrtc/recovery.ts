export type RecoveryPhase =
  | "connecting"
  | "connected"
  | "degraded"
  | "reconnecting"
  | "recovered"
  | "failed"
  /**
   * #526 — a later attach took over this session's signaling (WS close 4410).
   * Terminal but deliberately NOT `failed`: sessionRuntime escalates `failed`
   * by re-attaching with a new token, which would evict the tab that just
   * evicted us, looping. Not keyed on any escalation path.
   */
  | "superseded";

export interface RecoveryState {
  phase: RecoveryPhase;
  attempt: number;
  maxAttempts: number;
  message: string;
}

export interface RecoveryControllerOptions {
  maxAttempts?: number;
  retryDelaysMs?: readonly number[];
  onRetry: (attempt: number) => void;
  onState: (state: RecoveryState) => void;
  setTimer?: typeof setTimeout;
  clearTimer?: typeof clearTimeout;
}

/**
 * Client side of an in-place ICE recovery: keeps the existing media/peer
 * connection/data channel/telemetry/signaling socket while bounded restart
 * requests are made. Never mints a new signaling token here — tokens are
 * single-use.
 */
export class RecoveryController {
  private readonly maxAttempts: number;
  private readonly retryDelaysMs: readonly number[];
  private readonly setTimer: typeof setTimeout;
  private readonly clearTimer: typeof clearTimeout;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private attempt = 0;
  private stopped = false;

  constructor(private readonly options: RecoveryControllerOptions) {
    this.maxAttempts = options.maxAttempts ?? 3;
    this.retryDelaysMs = options.retryDelaysMs ?? [0, 5_000, 10_000];
    // Must stay bound to globalThis: `this.setTimer(...)` calls with the
    // controller as receiver, and Chrome's native setTimeout throws
    // "Illegal invocation" on any other receiver (fake-timer test mocks are
    // receiver-agnostic, so only real browsers hit this).
    this.setTimer = options.setTimer ?? setTimeout.bind(globalThis);
    this.clearTimer = options.clearTimer ?? clearTimeout.bind(globalThis);
    this.emit("connecting", "Connecting to the host…");
  }

  connected(): void {
    if (this.stopped) return;
    this.clearPending();
    const recovered = this.attempt > 0;
    this.attempt = 0;
    this.emit(recovered ? "recovered" : "connected", recovered ? "Connection recovered" : "Connected");
  }

  interrupted(reason = "Network path interrupted"): void {
    if (this.stopped || this.timer || this.attempt > 0) return;
    this.emit("degraded", reason);
    this.scheduleNext();
  }

  terminal(message: string): void {
    if (this.stopped) return;
    this.clearPending();
    this.emit("failed", message);
    this.stopped = true;
  }

  /**
   * Sets `stopped` so the ICE failure that follows a takeover (host now offers
   * to the new peer) can't re-enter `interrupted()` and restart escalation.
   */
  superseded(message: string): void {
    if (this.stopped) return;
    this.clearPending();
    this.emit("superseded", message);
    this.stopped = true;
  }

  cancel(): void {
    this.terminal("Recovery cancelled");
  }

  close(): void {
    this.stopped = true;
    this.clearPending();
  }

  private scheduleNext(): void {
    if (this.attempt >= this.maxAttempts) {
      this.terminal("Connection could not be recovered after bounded retries");
      return;
    }
    const delay = this.retryDelaysMs[this.attempt] ?? this.retryDelaysMs.at(-1) ?? 0;
    this.timer = this.setTimer(() => {
      this.timer = null;
      if (this.stopped) return;
      this.attempt += 1;
      this.emit(
        "reconnecting",
        `Reconnecting (${this.attempt}/${this.maxAttempts})…`,
      );
      this.options.onRetry(this.attempt);
      this.scheduleNext();
    }, delay);
  }

  private clearPending(): void {
    if (this.timer) this.clearTimer(this.timer);
    this.timer = null;
  }

  private emit(phase: RecoveryPhase, message: string): void {
    this.options.onState({ phase, attempt: this.attempt, maxAttempts: this.maxAttempts, message });
  }
}
