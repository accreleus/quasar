// MediaTransport over WebTransport: protocol/transport-webtransport.md, wt_proto 1,
// video, input, ping/pong, NACK, receiver feedback, audio and the microphone, with PyroWave
// (codec-pyrowave.md) for a client that offers it.

import {
  base64Bytes,
  CHANNEL_AUDIO,
  ControlDecoder,
  ControlProtocolError,
  encodeControl,
  parseAudioDatagram,
  parseVideoDatagram,
  serialAfter,
  type VideoFragment,
} from "./framing";
import { blockLayout } from "../pyrowave/bitstream";
import { PyroWaveReceiver } from "../pyrowave/receiver";
import type { WebTransportCoords } from "../api/types";
import { FEEDBACK_MS, FeedbackRecorder } from "./feedback";
import type { InputWire } from "./inputSender";
import { workerTicker, type Ticker } from "./pingTicker";
import { FrameAssembler, type Reassembled } from "./reassembly";
import {
  TransportError,
  type AudioConfig,
  type AudioStreams,
  type KeyframeReason,
  type MediaSink,
  type MediaTransport,
  type VideoConfig,
} from "./transport";

const WT_PROTO = 1;
/** `session_not_ready`: retry the same token after this long (§Hello). */
const NOT_READY_RETRY_MS = 250;
/** How long connect keeps retrying an agent that says it is not ready. */
export const CONNECT_DEADLINE_MS = 60_000;
/** How long connect waits for the WebTransport handshake. A host that completed it is reachable,
 *  so attempts after that (not ready, or an answer Chrome dropped) are not bounded by this: they
 *  run to {@link CONNECT_DEADLINE_MS}. Well under the loader's give-up time, so the page can fall
 *  back to WebRTC (control-api.md §Transport fallback) while the launch is still waiting. */
export const HANDSHAKE_DEADLINE_MS = 10_000;
/** One attempt's wait for its answer; the agent closes a connection with no `hello` at 5 s. */
const ANSWER_TIMEOUT_MS = 10_000;
/** How long a connection that failed without an answer gets to report its close code. */
const CLOSE_CODE_WAIT_MS = 1_000;
/** How long close() lets the `bye` leave before it closes the session regardless. */
export const BYE_FLUSH_MS = 100;
/** PWV1's 14-bit size fields: no PyroWave picture is larger on a side. */
const MAX_PYROWAVE_SIDE = 16_384;
/** The agent forces at most one IDR per 150 ms (quasar-protocol#29, item 8); asking faster is noise. */
export const KEYFRAME_SPACING_MS = 150;
/** How often loss-recovery deadlines are checked when no datagram arrives. */
export const RECOVERY_TICK_MS = 50;
/** The control plane is asked for a fresh token at most this often. */
export const REMINT_MIN_INTERVAL_MS = 10_000;
/** How long a fragment of a configuration not acknowledged yet is held (the decode stage's hold). */
export const FRAGMENT_HOLD_MS = 1_000;
/** Fragments held at once for configurations not acknowledged yet: a few PyroWave frames. */
export const MAX_HELD_FRAGMENTS = 4_096;
/** A token held this long (TTL 60 s, control-api.md) is replaced after a failed attempt. Measured
 *  on this page's monotonic clock from its arrival, never against the server's `expires_at`. */
export const TOKEN_REFRESH_AGE_MS = 45_000;

/** The client pings once a second (§Ping and pong). */
const PING_MS = 1_000;
/** A datagram carries at least this much on any path (§Channels, "Datagram size"). */
const MAX_INPUT_DATAGRAM = 1148;

/** WebTransport close codes during connect (§Bye and error), as the refusal they stand for. */
const CLOSE_REFUSALS: Record<number, string> = {
  4400: "internal",
  4401: "auth_failed",
  4404: "session_ended",
  4410: "taken_over",
};

/** How one attempt ended. `retry`: the agent is not ready, or the connection failed with no
 *  code, which is how the agent's refusal can arrive (its teardown races the `error`
 *  message, and Chrome may fail the connection before the page reads it). */
type Answer = { kind: "ack" } | { kind: "retry" } | { kind: "refused"; code: string };

export interface WebTransportMediaOptions {
  sessionId: string;
  coords: WebTransportCoords;
  /** `POST /v1/sessions/{id}/transport-token`. */
  mintToken(): Promise<WebTransportCoords>;
  /** `hello.pyrowave`: this client's WebGPU device is up and its PyroWave decoder is built. */
  pyrowave?: boolean;
  /** Test seam; the browser's WebTransport otherwise. */
  createWebTransport?: (url: string, options: WebTransportOptions) => WebTransport;
  sleep?: (ms: number) => Promise<void>;
  /** Monotonic milliseconds. */
  now?: () => number;
  /** The ping's clock; a Worker's by default, so a hidden tab keeps pinging. */
  ticker?: Ticker;
}

/** `hello_ack.video` into a {@link VideoConfig}; throws on a shape it cannot use. */
export function videoConfigFrom(ack: Record<string, unknown>): VideoConfig {
  const v = ack.video as Record<string, unknown> | null | undefined;
  const configId = ack.config_id;
  if (
    !v ||
    typeof configId !== "number" ||
    typeof v.codec !== "string" ||
    typeof v.description_b64 !== "string" ||
    typeof v.width !== "number" ||
    typeof v.height !== "number"
  ) {
    throw new TransportError("internal", "hello_ack without a usable video configuration");
  }
  // The PyroWave receiver and decoder size their arrays from these.
  const side = (n: number) => Number.isInteger(n) && n >= 1 && n <= MAX_PYROWAVE_SIDE;
  if (v.codec === "pyrowave" && !(side(v.width) && side(v.height))) {
    throw new TransportError("internal", "hello_ack with a PyroWave picture size out of range");
  }
  const cs = v.color_space as Record<string, unknown> | undefined;
  return {
    configId,
    codec: v.codec,
    description: base64Bytes(v.description_b64),
    width: v.width,
    height: v.height,
    fps: typeof v.fps === "number" ? v.fps : 0,
    colorSpace: cs
      ? {
          primaries: cs.primaries as VideoColorPrimaries,
          transfer: cs.transfer as VideoTransferCharacteristics,
          matrix: cs.matrix as VideoMatrixCoefficients,
          fullRange: cs.full_range === true,
        }
      : undefined,
    ...(typeof ack.codec_switch_reason === "string" ? { switchReason: ack.codec_switch_reason } : {}),
  };
}

const audioConfigFrom = (a: unknown): AudioConfig | null => {
  const o = a as Record<string, unknown> | null | undefined;
  if (!o || o.codec !== "opus" || typeof o.channels !== "number" || typeof o.sample_rate !== "number") return null;
  return { channels: o.channels, sampleRate: o.sample_rate, ptimeMs: typeof o.ptime_ms === "number" ? o.ptime_ms : 10 };
};

/** `hello_ack.audio` and `.mic`; null for a stream the host does not carry or one this client cannot use. */
export const audioStreamsFrom = (ack: Record<string, unknown>): AudioStreams => ({
  audio: audioConfigFrom(ack.audio),
  mic: audioConfigFrom(ack.mic),
});

/** Routes one connection's video fragments: PyroWave frames to their receiver, the rest to the
 *  access-unit assembler. Also the order of configurations for both. */
interface Router {
  datagram(f: VideoFragment): void;
  config(c: VideoConfig): void;
  /** `nack_miss`: the agent no longer holds those fragments. */
  miss(frameSeq: number, frags: [number, number][]): void;
  /** The assembler's release and lateness pass, with no datagram to drive it. */
  tick(): void;
  stop(): void;
}

const refused = (code: string): Answer => ({ kind: "refused", code });
const after = <T>(ms: number, value: T) => new Promise<T>((r) => setTimeout(() => r(value), ms));

export class WebTransportMedia implements MediaTransport {
  private wt: WebTransport | null = null;
  private writer: WritableStreamDefaultWriter<Uint8Array> | null = null;
  private datagramWriter: WritableStreamDefaultWriter<Uint8Array> | null = null;
  private stopTickers: (() => void)[] = [];
  private pingId = 0;
  /** The sink of the current connect, for what is reported outside a read loop. */
  private sink: MediaSink | null = null;
  private lostSoFar: () => number = () => 0;
  private closed = false;
  /** Closes the attempt in flight; null once one has connected or ended. */
  private abortAttempt: (() => void) | null = null;
  /** The connected attempt's guard: off, its loops reach the sink no more. */
  private live: { on: boolean } | null = null;
  private connected = false;
  private coords: WebTransportCoords;
  /** When the current token arrived, on the monotonic clock. */
  private tokenAt: number;
  private lastMint = -Infinity;
  private lastKeyAt = -Infinity;
  private keyTimer: ReturnType<typeof setTimeout> | null = null;
  /** §Loss recovery's tick: releases and late frames wait for no datagram. */
  private recoveryTimer: ReturnType<typeof setInterval> | null = null;
  private pendingKey: KeyframeReason | null = null;
  private router: Router | null = null;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly now: () => number;

  /** Both paths are no-ops until connected. */
  readonly input: InputWire = {
    reliable: (event) => this.send({ type: "input", event }),
    datagram: (bytes) => {
      const cap = Math.min(this.wt?.datagrams.maxDatagramSize ?? MAX_INPUT_DATAGRAM, MAX_INPUT_DATAGRAM);
      if (!this.datagramWriter || bytes.length > cap) return false;
      void this.datagramWriter.write(bytes).catch(() => {});
      return true;
    },
  };

  constructor(private readonly opts: WebTransportMediaOptions) {
    this.coords = opts.coords;
    this.sleep = opts.sleep ?? ((ms) => after(ms, undefined));
    this.now = opts.now ?? (() => performance.now());
    this.tokenAt = this.now();
  }

  async connect(sink: MediaSink): Promise<void> {
    this.sink = sink;
    const deadline = this.now() + CONNECT_DEADLINE_MS;
    let reminted = false;
    for (;;) {
      if (this.closed) throw new TransportError("closed");
      if (this.now() >= deadline) throw new TransportError("connect_timeout");
      const answer = await this.attempt(sink);
      if (answer.kind === "ack") return;
      if (answer.kind === "retry") {
        await this.sleep(NOT_READY_RETRY_MS);
        if (this.now() - this.tokenAt >= TOKEN_REFRESH_AGE_MS) await this.remint();
        continue;
      }
      // §Hello: on auth_failed mint a fresh token once; then give up.
      if (answer.code === "auth_failed" && !reminted && (await this.remint())) {
        reminted = true;
        continue;
      }
      throw new TransportError(answer.code);
    }
  }

  /** At most one request per {@link KEYFRAME_SPACING_MS}; one asked inside it is sent at its end. */
  requestKeyframe(reason: KeyframeReason): void {
    if (this.closed || !this.connected) return;
    const wait = this.lastKeyAt + KEYFRAME_SPACING_MS - this.now();
    if (wait <= 0) {
      this.lastKeyAt = this.now();
      this.send({ type: "keyframe_request", reason });
      this.sink?.keyframeRequested?.(reason);
      return;
    }
    this.pendingKey = reason;
    this.keyTimer ??= setTimeout(() => {
      this.keyTimer = null;
      const r = this.pendingKey;
      this.pendingKey = null;
      if (r) this.requestKeyframe(r);
    }, wait);
  }

  sendMic(datagram: Uint8Array): boolean {
    return this.input.datagram(datagram);
  }

  codecFallback(reason: string): void {
    if (this.closed || !this.connected) return;
    this.send({ type: "codec_fallback", reason });
  }

  /** Settles when `close()` has closed the session. */
  private closing: Promise<void> = Promise.resolve();

  private stopRecovery(): void {
    if (this.recoveryTimer !== null) clearInterval(this.recoveryTimer);
    this.recoveryTimer = null;
  }

  close(): Promise<void> {
    if (this.closed) return this.closing;
    this.closed = true;
    if (this.keyTimer !== null) clearTimeout(this.keyTimer);
    // Not left to `wt.closed`, which a connection that never finishes closing does not resolve.
    this.stopRecovery();
    this.router?.stop();
    this.stopPings();
    // An attempt still connecting is closed here; it sees `closed` at its next await.
    this.abortAttempt?.();
    if (this.live) this.live.on = false;
    if (this.connected) this.send({ type: "bye", reason: "client_closed" });
    // The bye is queued on the stream ahead of the close. A control stream that was reset, or one
    // that stays blocked, must not keep the session open (control-api.md §Transport fallback).
    const flushed = this.writer?.ready.catch(() => {});
    this.closing = Promise.race([flushed, after(BYE_FLUSH_MS, undefined)]).then(() => this.wt?.close({ closeCode: 0, reason: "" }));
    return this.closing;
  }

  /** A fresh token, at most once per {@link REMINT_MIN_INTERVAL_MS}; false when it is too soon. */
  private async remint(): Promise<boolean> {
    if (this.now() - this.lastMint < REMINT_MIN_INTERVAL_MS) return false;
    this.lastMint = this.now();
    try {
      this.coords = await this.opts.mintToken();
    } catch (err) {
      // A refused mint (the session is over, or no longer on WebTransport) ends the connect.
      throw new TransportError("auth_failed", err instanceof Error ? err.message : undefined);
    }
    this.tokenAt = this.now();
    return true;
  }

  /** Stops the ping and feedback tickers. */
  private stopPings(): void {
    for (const stop of this.stopTickers.splice(0)) stop();
  }

  private send(message: object): void {
    void this.writer?.write(encodeControl(message)).catch(() => {});
  }

  /** One connection and its `hello`. Every way out but `ack` closes the connection, and the
   *  attempt's own guard stops its loops from reaching `sink` afterwards. */
  private async attempt(sink: MediaSink): Promise<Answer> {
    const live = { on: true };
    const hashes = this.coords.cert_hashes.map((h) => ({
      algorithm: h.algorithm,
      value: base64Bytes(h.value),
    }));
    const options: WebTransportOptions = hashes.length ? { serverCertificateHashes: hashes } : {};
    let wt: WebTransport;
    try {
      wt = this.opts.createWebTransport
        ? this.opts.createWebTransport(this.coords.url, options)
        : new WebTransport(this.coords.url, options);
    } catch {
      return refused("connect_failed");
    }
    // Read here or below; a failed connect must not leave it unhandled.
    const closeCode = wt.closed.then(
      (info) => info.closeCode ?? null,
      () => null,
    );
    const abandon = (answer: Answer, closeInfo?: WebTransportCloseInfo): Answer => {
      live.on = false;
      try {
        wt.close(closeInfo);
      } catch {
        /* already closed */
      }
      if (this.abortAttempt === abort) this.abortAttempt = null;
      return answer;
    };
    const abort = () => void abandon(refused("closed"));
    this.abortAttempt = abort;
    /** The answer a connection that ended without a message gave, read off its close code. */
    const fromClose = async (): Promise<Answer> => {
      const code = await Promise.race([closeCode, after(CLOSE_CODE_WAIT_MS, null)]);
      return abandon(code === null || code === 0 ? { kind: "retry" } : refused(CLOSE_REFUSALS[code] ?? "internal"));
    };

    try {
      // A blocked UDP port can leave the handshake pending for the browser's own, longer, timeout.
      let timer: ReturnType<typeof setTimeout> | undefined;
      const ready = await Promise.race([
        wt.ready.then(() => true),
        new Promise<boolean>((r) => void (timer = setTimeout(() => r(false), HANDSHAKE_DEADLINE_MS))),
      ]).finally(() => clearTimeout(timer));
      if (!ready) return abandon(refused(this.closed ? "closed" : "connect_timeout"));
    } catch {
      // The handshake or the certificate failed (another try meets the same), or close() ended it.
      return abandon(refused(this.closed ? "closed" : "connect_failed"));
    }
    if (this.closed) return abandon(refused("closed"));

    let writer: WritableStreamDefaultWriter<Uint8Array>;
    let reader: ReadableStreamDefaultReader<Uint8Array>;
    try {
      const stream = await wt.createBidirectionalStream();
      writer = stream.writable.getWriter();
      reader = stream.readable.getReader();
      await writer.write(
        encodeControl({
          type: "hello",
          wt_proto: WT_PROTO,
          token: this.coords.token,
          session_id: this.opts.sessionId,
          client: "browser",
          ...(this.opts.pyrowave ? { pyrowave: true } : {}),
        }),
      );
    } catch {
      return fromClose();
    }
    if (this.closed) return abandon(refused("closed"));

    const control = new ControlDecoder();
    const queue: Record<string, unknown>[] = [];
    const next = async (): Promise<Record<string, unknown> | null> => {
      while (queue.length === 0) {
        const { value, done } = await reader.read();
        if (done) return null;
        queue.push(...control.push(value));
      }
      return queue.shift() as Record<string, unknown>;
    };

    // Datagrams can overtake the hello_ack; the decode stage holds units by config_id, the
    // router PyroWave fragments.
    const router = this.makeRouter(sink, live);
    const feedback = new FeedbackRecorder(this.now());
    void this.readDatagrams(wt, sink, router, live, feedback).catch((err: unknown) => {
      // No more video would arrive on this connection: end it, as a malformed control stream does.
      if (live.on) wt.close({ closeCode: 4400, reason: err instanceof Error ? err.message.slice(0, 100) : "" });
    });
    let first: Record<string, unknown> | null;
    try {
      first = await Promise.race([next(), after(ANSWER_TIMEOUT_MS, null)]);
    } catch (err) {
      if (err instanceof ControlProtocolError) return abandon(refused("internal"), { closeCode: 4400, reason: err.message });
      return fromClose();
    }
    if (this.closed) return abandon(refused("closed"));
    if (first === null) return fromClose();
    if (first.type === "error") {
      const code = typeof first.code === "string" ? first.code : "internal";
      return abandon(code === "session_not_ready" ? { kind: "retry" } : refused(code));
    }
    if (first.type !== "hello_ack") return abandon(refused("internal"), { closeCode: 4400, reason: "expected hello_ack" });
    if (first.wt_proto !== WT_PROTO) {
      void writer.write(encodeControl({ type: "bye", reason: "client_closed" })).catch(() => {});
      return abandon(refused("unsupported_version"));
    }
    let config: VideoConfig;
    try {
      config = videoConfigFrom(first);
    } catch {
      return abandon(refused("internal"), { closeCode: 4400, reason: "malformed hello_ack" });
    }

    if (this.closed) return abandon(refused("closed"));
    this.abortAttempt = null;
    this.wt = wt;
    this.writer = writer;
    this.connected = true;
    this.live = live;
    this.router = router;
    this.datagramWriter = wt.datagrams.writable.getWriter();
    sink.streams?.(audioStreamsFrom(first));
    router.config(config);
    const ping = () => this.send({ type: "ping", id: ++this.pingId, tc: this.now() });
    ping();
    const ticker = this.opts.ticker ?? workerTicker;
    // On the Worker clock too: the agent reads feedback that stops while it sends media as
    // congestion, so a hidden tab must keep reporting.
    const report = () => {
      const message = live.on ? feedback.take(this.now()) : null;
      if (message) this.send(message);
    };
    this.stopTickers.push(ticker(PING_MS, ping), ticker(FEEDBACK_MS, report));
    void (async () => {
      try {
        for (let m = await next(); m && live.on; m = await next()) {
          // Unknown types are ignored (§Channels). bye is followed by the close.
          if (m.type === "hello_ack") router.config(videoConfigFrom(m));
          else if (m.type === "nack_miss" && typeof m.frame_seq === "number" && Array.isArray(m.frags)) {
            router.miss(m.frame_seq, m.frags as [number, number][]);
          } else if (m.type === "pong" && typeof m.tc === "number") sink.rtt?.(Math.max(0, this.now() - m.tc), typeof m.tw === "number" ? m.tw : undefined);
          else if (m.type === "ping") {
            this.send({ type: "pong", id: m.id, ts: m.ts, tw: m.tw, tc: this.now() });
          }
        }
      } catch (err) {
        // A malformed stream, or a re-sent hello_ack this page cannot use: the connection is over.
        wt.close({ closeCode: 4400, reason: err instanceof Error ? err.message.slice(0, 100) : "" });
      }
    })();
    // Release and lateness must not wait for the next datagram: a still picture may send none.
    this.recoveryTimer = setInterval(() => {
      if (live.on && !this.closed) router.tick();
    }, RECOVERY_TICK_MS);
    void wt.closed.then(
      (info) => {
        this.stopRecovery();
        this.stopPings();
        sink.end({ code: info.closeCode ?? null, reason: info.reason ?? "" });
      },
      (err: unknown) => {
        this.stopRecovery();
        this.stopPings();
        sink.end({ code: null, reason: err instanceof Error ? err.message : "" });
      },
    );
    return { kind: "ack" };
  }

  private async readDatagrams(
    wt: WebTransport,
    sink: MediaSink,
    router: Router,
    live: { on: boolean },
    feedback: FeedbackRecorder,
  ): Promise<void> {
    const reader = wt.datagrams.readable.getReader();
    for (;;) {
      const { value, done } = await reader.read();
      if (done || !live.on) {
        router.stop();
        return;
      }
      feedback.record(value as Uint8Array, this.now());
      if ((value as Uint8Array)[0] === CHANNEL_AUDIO) {
        const packet = parseAudioDatagram(value as Uint8Array);
        if (packet) sink.audio?.(packet);
        continue;
      }
      const frag = parseVideoDatagram(value as Uint8Array);
      if (!frag) continue; // another channel, or malformed: dropped (§Channels)
      router.datagram(frag);
    }
  }

  private makeRouter(sink: MediaSink, live: { on: boolean }): Router {
    const assembler = new FrameAssembler(this.now);
    const configs = new Map<number, VideoConfig>();
    let latest: number | null = null;
    let pyro: PyroWaveReceiver | null = null;
    let held: { f: VideoFragment; at: number }[] = [];
    let timer: ReturnType<typeof setTimeout> | null = null;
    let armedAt: number | null = null;
    const stop = () => {
      if (timer !== null) clearTimeout(timer);
      timer = null;
      armedAt = null;
    };
    // One timer, at the PyroWave receiver's next frame deadline; re-armed only when that moves,
    // not per datagram (a 1080p frame is ~380 of them).
    const arm = () => {
      const at = pyro?.nextDeadline() ?? null;
      if (at === armedAt) return;
      stop();
      if (at === null || !live.on) return;
      armedAt = at;
      timer = setTimeout(() => {
        timer = null;
        armedAt = null;
        const frame = live.on ? pyro?.due(this.now()) : null;
        if (frame) sink.pyrowave(frame);
        arm();
      }, Math.max(0, at - this.now()));
    };
    // codec-pyrowave.md §Loss: a PyroWave frame never reaches the assembler, so it is never
    // NACKed or declared late; its losses are concealed by the decoder.
    const datagram = (f: VideoFragment) => {
      const c = configs.get(f.configId);
      if (c?.codec === "pyrowave") {
        const r =
          pyro?.configId === c.configId
            ? pyro
            : (pyro = new PyroWaveReceiver(c.configId, blockLayout(c.width, c.height).totalBlocks, 1000 / (c.fps || 60)));
        const now = this.now();
        const due = r.due(now);
        if (due) sink.pyrowave(due);
        const done = r.push(f, now);
        if (done) sink.pyrowave(done);
        arm();
        return;
      }
      // A PyroWave fragment must never reach the assembler, so a client that offered PyroWave
      // holds the fragments of a configuration it has no hello_ack for yet.
      if (!c && this.opts.pyrowave && (latest === null || serialAfter(f.configId, latest, 8))) {
        const at = this.now();
        held = held.filter((h) => at - h.at < FRAGMENT_HOLD_MS);
        if (held.length < MAX_HELD_FRAGMENTS) held.push({ f, at });
        return;
      }
      this.recovered(assembler.push(f), sink);
    };
    const config = (c: VideoConfig) => {
      configs.delete(c.configId);
      configs.set(c.configId, c);
      if (configs.size > 16) configs.delete(configs.keys().next().value as number);
      latest = c.configId;
      sink.config(c);
      const replay = held.filter((h) => h.f.configId === c.configId);
      held = held.filter((h) => serialAfter(h.f.configId, c.configId, 8));
      for (const h of replay) datagram(h.f);
    };
    const miss = (frameSeq: number, frags: [number, number][]) => this.recovered(assembler.miss(frameSeq, frags), sink);
    const tick = () => this.recovered(assembler.tick(), sink);
    this.lostSoFar = () => assembler.lost;
    return { datagram, config, miss, tick, stop };
  }

  /** §Loss recovery: NACKs out, a keyframe asked for a late frame, whole units to the sink. */
  private recovered({ units, nacks, needKeyframe }: Reassembled, sink: MediaSink): void {
    for (const n of nacks) this.send({ type: "nack", frame_seq: n.frameSeq, frags: n.frags });
    if (nacks.length) sink.lost?.(this.lostSoFar());
    if (needKeyframe) this.requestKeyframe("late");
    for (const u of units) sink.unit(u);
  }
}
