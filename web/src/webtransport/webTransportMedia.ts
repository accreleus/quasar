// MediaTransport over WebTransport: protocol/transport-webtransport.md, wt_proto 1,
// video only. Input, ping/pong, audio, NACK and feedback are later tickets (#1032,
// #1033, #1039, #1040).

import {
  base64Bytes,
  ControlDecoder,
  ControlProtocolError,
  encodeControl,
  parseVideoDatagram,
} from "./framing";
import type { WebTransportCoords } from "../api/types";
import { FrameAssembler } from "./reassembly";
import {
  TransportError,
  type KeyframeReason,
  type MediaSink,
  type MediaTransport,
  type VideoConfig,
} from "./transport";

const WT_PROTO = 1;
/** `session_not_ready`: retry the same token after this long (§Hello). */
const NOT_READY_RETRY_MS = 250;
/** Mint a new token this long before the old one expires, so a retry never presents a dead one. */
const TOKEN_MARGIN_MS = 5_000;


export interface WebTransportMediaOptions {
  sessionId: string;
  coords: WebTransportCoords;
  /** `POST /v1/sessions/{id}/transport-token`. */
  mintToken(): Promise<WebTransportCoords>;
  /** Test seam; the browser's WebTransport otherwise. */
  createWebTransport?: (url: string, options: WebTransportOptions) => WebTransport;
  sleep?: (ms: number) => Promise<void>;
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

export class WebTransportMedia implements MediaTransport {
  private wt: WebTransport | null = null;
  private writer: WritableStreamDefaultWriter<Uint8Array> | null = null;
  private closed = false;
  private connected = false;
  private coords: WebTransportCoords;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly now: () => number;

  constructor(private readonly opts: WebTransportMediaOptions) {
    this.coords = opts.coords;
    this.sleep = opts.sleep ?? ((ms) => new Promise((r) => setTimeout(r, ms)));
    this.now = opts.now ?? (() => Date.now());
  }

  async connect(sink: MediaSink): Promise<void> {
    let reminted = false;
    for (;;) {
      if (this.closed) throw new TransportError("closed");
      if (Date.parse(this.coords.expires_at) - this.now() < TOKEN_MARGIN_MS) {
        this.coords = await this.mint();
      }
      const refused = await this.attempt(sink);
      if (refused === null) return;
      if (refused === "session_not_ready") {
        await this.sleep(NOT_READY_RETRY_MS);
        continue;
      }
      // §Hello: mint a fresh token once; then give up.
      if (refused === "auth_failed" && !reminted) {
        reminted = true;
        this.coords = await this.mint();
        continue;
      }
      throw new TransportError(refused);
    }
  }

  requestKeyframe(reason: KeyframeReason): void {
    this.send({ type: "keyframe_request", reason });
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    if (this.connected) this.send({ type: "bye", reason: "client_closed" });
    // The bye is queued on the stream ahead of the close.
    void this.writer?.ready.then(() => this.wt?.close({ closeCode: 0, reason: "" }), () => {});
    if (!this.writer) this.wt?.close();
  }

  private async mint(): Promise<WebTransportCoords> {
    try {
      return await this.opts.mintToken();
    } catch (err) {
      throw new TransportError("auth_failed", err instanceof Error ? err.message : undefined);
    }
  }

  private send(message: object): void {
    void this.writer?.write(encodeControl(message)).catch(() => {});
  }

  /** One connection and its `hello`: null once a `hello_ack` arrived, else the
   *  refusal's `error.code`. */
  private async attempt(sink: MediaSink): Promise<string | null> {
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
      await wt.ready;
    } catch (err) {
      throw new TransportError("connect_failed", err instanceof Error ? err.message : undefined);
    }
    this.wt = wt;
    if (this.closed) {
      wt.close();
      throw new TransportError("closed");
    }
    const stream = await wt.createBidirectionalStream();
    const writer = stream.writable.getWriter();
    this.writer = writer;
    await writer.write(
      encodeControl({
        type: "hello",
        wt_proto: WT_PROTO,
        token: this.coords.token,
        session_id: this.opts.sessionId,
        client: "browser",
      }),
    );

    const reader = stream.readable.getReader();
    const control = new ControlDecoder();
    const queue: Record<string, unknown>[] = [];
    const next = async (): Promise<Record<string, unknown> | null> => {
      while (queue.length === 0) {
        const { value, done } = await reader.read();
        if (done) return null;
        queue.push(...control.push(value));
      }
      return queue.shift() ?? null;
    };

    // Datagrams can overtake the hello_ack; the decode stage holds them by config_id.
    const datagrams = this.readDatagrams(wt, sink);
    let first: Record<string, unknown> | null;
    try {
      first = await next();
    } catch (err) {
      wt.close({ closeCode: 4400, reason: "malformed control stream" });
      throw new TransportError("internal", err instanceof Error ? err.message : undefined);
    }
    if (first?.type !== "hello_ack") {
      void datagrams.catch(() => {});
      this.writer = null;
      wt.close();
      return typeof first?.code === "string" ? first.code : "internal";
    }
    if (first.wt_proto !== WT_PROTO) {
      this.send({ type: "bye", reason: "client_closed" });
      wt.close();
      return "unsupported_version";
    }
    sink.config(videoConfigFrom(first));
    this.connected = true;

    void (async () => {
      try {
        for (let m = await next(); m; m = await next()) {
          // Unknown types are ignored (§Channels). bye is followed by the close.
          if (m.type === "hello_ack") sink.config(videoConfigFrom(m));
        }
      } catch (err) {
        if (err instanceof ControlProtocolError) wt.close({ closeCode: 4400, reason: err.message });
      }
    })();
    void wt.closed.then(
      (info) => sink.end({ code: info.closeCode ?? null, reason: info.reason ?? "" }),
      (err: unknown) => sink.end({ code: null, reason: err instanceof Error ? err.message : "" }),
    );
    void datagrams.catch(() => {});
    return null;
  }

  private async readDatagrams(wt: WebTransport, sink: MediaSink): Promise<void> {
    const reader = wt.datagrams.readable.getReader();
    const assembler = new FrameAssembler();
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      const frag = parseVideoDatagram(value as Uint8Array);
      if (!frag) continue; // another channel, or malformed: dropped (§Channels)
      const { unit, needKeyframe } = assembler.push(frag);
      if (needKeyframe) this.requestKeyframe("loss");
      if (unit) sink.unit(unit);
    }
  }
}
