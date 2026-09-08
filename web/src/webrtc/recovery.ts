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
  | "superseded"
  /**
   * #128 — the signalling socket is gone but the media path is not. Media and
   * input are agent<->browser, so a control-plane restart leaves frames
   * flowing; the runtime re-attaches signalling in place. NON-TERMINAL and
   * deliberately not `failed`: `failed` is the media verdict, and escalating a
   * signalling close to it is what used to destroy a healthy peer connection
   * and end the session.
   */
  | "signaling-lost";

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
  /** #128 — signalling health, tracked independently of media health. */
  private signalingDown = false;
  /** The prose for the current signalling outage, so a media-side `connected()`
   *  re-states it instead of falsely clearing the banner. */
  private signalingMessage = "";
  /**
   * #128 — media degraded while signalling was down, so its retry was deferred
   * rather than burned. `onRetry` sends `restart_ice` over the signalling
   * socket, and `wsSend` DROPS silently when that socket is not open: running
   * the ladder during an outage spends all three attempts on a closed socket
   * in 15 s and terminalises a session the agent would have held for 120 s.
   */
  private mediaRetryDeferred = false;
  /** Last phase emitted, so a repeating source (telemetry ticks at 1 Hz) cannot
   *  republish an unchanged state and defeat the snapshot dedupe. */
  private lastPhase: RecoveryPhase | null = null;

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
    // #128: media recovered on its own, so a ladder held for it is no longer
    // wanted. Left armed it fires a pointless ICE restart the moment signalling
    // returns — a visible interruption on a path that was already working.
    this.mediaRetryDeferred = false;
    // Media being healthy does not clear a signalling outage, though. Reporting
    // "Connected" here would hide an in-progress re-attach behind a green state.
    if (this.signalingDown) {
      // Emitted only on change: media telemetry calls this every tick.
      if (this.lastPhase !== "signaling-lost") this.emit("signaling-lost", this.signalingMessage);
      return;
    }
    this.emit(recovered ? "recovered" : "connected", recovered ? "Connection recovered" : "Connected");
  }

  /**
   * #128 — the signalling socket dropped while media is unaffected. Does not
   * touch the media retry state: an ICE recovery already in flight keeps its
   * attempt count and its timer.
   */
  signalingLost(message: string): void {
    if (this.stopped) return;
    this.signalingMessage = message;
    if (this.signalingDown) return;
    this.signalingDown = true;
    this.emit("signaling-lost", message);
  }

  /** #128 — signalling re-attached. Media owns the phase if it is mid-recovery. */
  signalingRestored(): void {
    if (this.stopped || !this.signalingDown) return;
    this.signalingDown = false;
    this.signalingMessage = "";
    // A media recovery held during the outage runs now that its transport works.
    if (this.mediaRetryDeferred) {
      this.mediaRetryDeferred = false;
      this.scheduleNext();
      return;
    }
    if (this.timer || this.attempt > 0) return;
    this.emit("connected", "Connected");
  }

  /**
   * True while a media recovery has requests IN FLIGHT (#128) — a rebind sends
   * one `restart_ice` for these, to regenerate what the closed socket dropped.
   *
   * Deliberately excludes `mediaRetryDeferred`: a deferred ladder has sent
   * nothing yet and `signalingRestored()` starts it, which sends its own. If
   * this counted deferred too, a rebind would put two ICE-restart offers on the
   * wire before either was answered.
   */
  mediaRetryInFlight(): boolean {
    return this.timer != null || this.attempt > 0;
  }

  /** True while the signalling socket is known to be down (#128). */
  isSignalingDown(): boolean {
    return this.signalingDown;
  }

  interrupted(reason = "Network path interrupted"): void {
    if (this.stopped || this.timer || this.attempt > 0 || this.mediaRetryDeferred) return;
    this.emit("degraded", reason);
    // #128: the retry ladder talks over the signalling socket. With that socket
    // down every attempt is a silent no-op, so hold the ladder and run it when
    // signalling is back rather than exhausting it against nothing.
    if (this.signalingDown) {
      this.mediaRetryDeferred = true;
      return;
    }
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
      // #128: signalling went away while this ladder was mid-flight. `onRetry`
      // sends restart_ice over that socket and wsSend drops it silently, so
      // continuing here would spend the remaining attempts on nothing and
      // terminalise a session the host is still holding. Stand down and let
      // signalingRestored() resume.
      if (this.signalingDown) {
        this.mediaRetryDeferred = true;
        return;
      }
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
    this.lastPhase = phase;
    this.options.onState({ phase, attempt: this.attempt, maxAttempts: this.maxAttempts, message });
  }
}
