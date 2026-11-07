// MediaTransport over WebTransport: protocol/transport-webtransport.md, wt_proto 1,
// video, audio and the microphone. Input, ping/pong, NACK and feedback are later tickets
// (#1032, #1039, #1040).

import {
  base64Bytes,
  CHANNEL_AUDIO,
  ControlDecoder,
  ControlProtocolError,
  encodeControl,
  parseAudioDatagram,
  parseVideoDatagram,
} from "./framing";
import type { WebTransportCoords } from "../api/types";
import { FrameAssembler } from "./reassembly";
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
/** How long connect keeps retrying an agent that is not ready or gives no answer. */
export const CONNECT_DEADLINE_MS = 60_000;
/** One attempt's wait for its answer; the agent closes a connection with no `hello` at 5 s. */
const ANSWER_TIMEOUT_MS = 10_000;
/** How long a connection that failed without an answer gets to report its close code. */
const CLOSE_CODE_WAIT_MS = 1_000;
/** The agent forces at most one IDR per 150 ms (quasar-protocol#29, item 8); asking faster is noise. */
export const KEYFRAME_SPACING_MS = 150;
/** The control plane is asked for a fresh token at most this often. */
export const REMINT_MIN_INTERVAL_MS = 10_000;
/** A token held this long (TTL 60 s, control-api.md) is replaced after a failed attempt. Measured
 *  on this page's monotonic clock from its arrival, never against the server's `expires_at`. */
export const TOKEN_REFRESH_AGE_MS = 45_000;

/** A datagram carries at least this much on any path (§Channels, "Datagram size"). */
const MAX_DATAGRAM = 1148;

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
  /** Test seam; the browser's WebTransport otherwise. */
  createWebTransport?: (url: string, options: WebTransportOptions) => WebTransport;
  sleep?: (ms: number) => Promise<void>;
  /** Monotonic milliseconds. */
  now?: () => number;
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

const refused = (code: string): Answer => ({ kind: "refused", code });
const after = <T>(ms: number, value: T) => new Promise<T>((r) => setTimeout(() => r(value), ms));

export class WebTransportMedia implements MediaTransport {
  private wt: WebTransport | null = null;
  private writer: WritableStreamDefaultWriter<Uint8Array> | null = null;
  private datagramWriter: WritableStreamDefaultWriter<Uint8Array> | null = null;
  private closed = false;
  /** Closes the attempt in flight; null once one has connected or ended. */
  private abortAttempt: (() => void) | null = null;
  private connected = false;
  private coords: WebTransportCoords;
  /** When the current token arrived, on the monotonic clock. */
  private tokenAt: number;
  private lastMint = -Infinity;
  private lastKeyAt = -Infinity;
  private keyTimer: ReturnType<typeof setTimeout> | null = null;
  private pendingKey: KeyframeReason | null = null;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly now: () => number;

  constructor(private readonly opts: WebTransportMediaOptions) {
    this.coords = opts.coords;
    this.sleep = opts.sleep ?? ((ms) => after(ms, undefined));
    this.now = opts.now ?? (() => performance.now());
    this.tokenAt = this.now();
  }

  async connect(sink: MediaSink): Promise<void> {
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
    const cap = Math.min(this.wt?.datagrams.maxDatagramSize ?? MAX_DATAGRAM, MAX_DATAGRAM);
    if (!this.datagramWriter || datagram.length > cap) return false;
    void this.datagramWriter.write(datagram).catch(() => {});
    return true;
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    if (this.keyTimer !== null) clearTimeout(this.keyTimer);
    // An attempt still connecting is closed here; it sees `closed` at its next await.
    this.abortAttempt?.();
    if (this.connected) this.send({ type: "bye", reason: "client_closed" });
    // The bye is queued on the stream ahead of the close.
    void this.writer?.ready.then(() => this.wt?.close({ closeCode: 0, reason: "" }), () => {});
    if (!this.writer) this.wt?.close();
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
      await wt.ready;
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

    // Datagrams can overtake the hello_ack; the decode stage holds them by config_id.
    void this.readDatagrams(wt, sink, live).catch(() => {});
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
    this.datagramWriter = wt.datagrams.writable.getWriter();
    sink.streams?.(audioStreamsFrom(first));
    sink.config(config);
    void (async () => {
      try {
        for (let m = await next(); m && live.on; m = await next()) {
          // Unknown types are ignored (§Channels). bye is followed by the close.
          if (m.type === "hello_ack") sink.config(videoConfigFrom(m));
        }
      } catch (err) {
        // A malformed stream, or a re-sent hello_ack this page cannot use: the connection is over.
        wt.close({ closeCode: 4400, reason: err instanceof Error ? err.message.slice(0, 100) : "" });
      }
    })();
    void wt.closed.then(
      (info) => sink.end({ code: info.closeCode ?? null, reason: info.reason ?? "" }),
      (err: unknown) => sink.end({ code: null, reason: err instanceof Error ? err.message : "" }),
    );
    return { kind: "ack" };
  }

  private async readDatagrams(wt: WebTransport, sink: MediaSink, live: { on: boolean }): Promise<void> {
    const reader = wt.datagrams.readable.getReader();
    const assembler = new FrameAssembler(this.now);
    for (;;) {
      const { value, done } = await reader.read();
      if (done || !live.on) return;
      if ((value as Uint8Array)[0] === CHANNEL_AUDIO) {
        const packet = parseAudioDatagram(value as Uint8Array);
        if (packet) sink.audio?.(packet);
        continue;
      }
      const frag = parseVideoDatagram(value as Uint8Array);
      if (!frag) continue; // another channel, or malformed: dropped (§Channels)
      const { unit, needKeyframe } = assembler.push(frag);
      if (needKeyframe) this.requestKeyframe("loss");
      if (unit) sink.unit(unit);
    }
  }
}
