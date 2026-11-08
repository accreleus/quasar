import { serialAfter } from "./framing";
import type { AccessUnit, KeyframeReason, VideoConfig } from "./transport";

/** How long a unit waits for the `hello_ack` of its `config_id` (§Hello ack). */
export const HOLD_MS = 1_000;
/** Frames queued in the decoder, about 50 ms at 60 fps, that count as a backlog. */
export const MAX_DECODE_QUEUE = 3;
/** How long a backlog must last before deltas skip to a keyframe. */
export const BACKLOG_MS = 50;
/** The decoder queue never grows past this, whatever the skip above is doing. */
export const MAX_DECODE_QUEUE_HARD = 15;
/** Decoder failures in a row, with no frame decoded between them, before giving up. */
export const MAX_DECODER_RESTARTS = 3;

export type DecodeFailure = "decoder_unsupported" | "decode_error";

export interface DecodeStageOptions {
  /** Each decoded frame; the callee owns it and must close it. */
  onFrame(frame: VideoFrame): void;
  requestKeyframe(reason: KeyframeReason): void;
  /** Terminal: the browser cannot decode this configuration, or the decoder keeps failing. */
  onFatal(reason: DecodeFailure, config: VideoConfig): void;
  /** Test seam; the browser's VideoDecoder otherwise. */
  createDecoder?: (init: VideoDecoderInit) => VideoDecoder;
  isConfigSupported?: (config: VideoDecoderConfig) => Promise<VideoDecoderSupport>;
  now?: () => number;
}

/** `avc1.PPCCLL` from an avcC record's profile, constraints and level bytes. */
export function avcCodecString(avcC: Uint8Array): string {
  const hex = (b: number) => b.toString(16).padStart(2, "0");
  return `avc1.${hex(avcC[1])}${hex(avcC[2])}${hex(avcC[3])}`;
}

/** `hvc1.<profile>.<compat, bit-reversed>.<tier><level>[.<constraints>]` from an hvcC record (ISO 14496-15 Annex E). */
export function hevcCodecString(hvcC: Uint8Array): string {
  const space = ["", "A", "B", "C"][hvcC[1] >> 6];
  const compat = new DataView(hvcC.buffer, hvcC.byteOffset).getUint32(2);
  let reversed = 0;
  for (let i = 0; i < 32; i++) reversed |= ((compat >>> i) & 1) << (31 - i);
  let end = 12;
  while (end > 6 && hvcC[end - 1] === 0) end--;
  const constraints = Array.from(hvcC.subarray(6, end), (b) => "." + b.toString(16).toUpperCase());
  const tier = hvcC[1] & 0x20 ? "H" : "L";
  return `hvc1.${space}${hvcC[1] & 0x1f}.${(reversed >>> 0).toString(16).toUpperCase()}.${tier}${hvcC[12]}${constraints.join("")}`;
}

/** `av01.<profile>.<level><tier>.<depth>` from an av1C record; a Main-profile 8-bit 4.0 stream when the agent sent none. */
export function av1CodecString(av1C: Uint8Array): string {
  if (av1C.length < 4) return "av01.0.08M.08";
  const profile = av1C[1] >> 5;
  const level = (av1C[1] & 0x1f).toString().padStart(2, "0");
  const tier = av1C[2] & 0x80 ? "H" : "M";
  const depth = av1C[2] & 0x40 ? (av1C[2] & 0x20 ? "12" : "10") : "08";
  return `av01.${profile}.${level}${tier}.${depth}`;
}

/** The WebCodecs codec string for a `hello_ack` configuration, from its description. */
export function codecString(config: Pick<VideoConfig, "codec" | "description">): string {
  switch (config.codec) {
    case "h264":
      return avcCodecString(config.description);
    case "h265":
      return hevcCodecString(config.description);
    case "av1":
      return av1CodecString(config.description);
    default:
      return config.codec;
  }
}

/**
 * Units into WebCodecs, under the configuration their `config_id` names. A unit
 * for a `config_id` not seen yet is held until its `hello_ack` (datagrams can
 * overtake the control stream), at most {@link HOLD_MS}; an older one is dropped.
 */
export class DecodeStage {
  private decoder: VideoDecoder | null = null;
  private current: VideoConfig | null = null;
  private held: { unit: AccessUnit; at: number }[] = [];
  /** Deltas are useless until a keyframe: after a (re)configure, a decoder restart or a skip. */
  private needKey = true;
  private askedKey = false;
  /** Set once the stage can do nothing more: closed, or a fatal error reported. */
  private stopped = false;
  /** Bumped per configure, so a late support check cannot apply an old config. */
  private generation = 0;
  private restarts = 0;
  /** When the decoder queue went over {@link MAX_DECODE_QUEUE}; null while it is not. */
  private backlogSince: number | null = null;
  /** Cleared by a skip, set again once the keyframe it asked for has decoded. */
  private skipArmed = true;
  /** Capture time of that keyframe: its frame re-arms the skip. */
  private rearmAt: number | null = null;
  private readonly now: () => number;

  constructor(private readonly opts: DecodeStageOptions) {
    this.now = opts.now ?? (() => performance.now());
  }

  config(config: VideoConfig): void {
    if (this.stopped) return;
    // A re-sent hello_ack for the live configuration (the agent's resync) changes nothing.
    if (this.current?.configId === config.configId) return;
    this.current = config;
    // Null until configure() finishes: units for this config wait in `held` meanwhile.
    const decoder = this.decoder?.state === "closed" ? null : this.decoder;
    this.decoder = null;
    this.wantKey();
    const generation = ++this.generation;
    void this.configure(config, generation, decoder).then(() => {
      if (generation !== this.generation || this.stopped) return;
      const waiting = this.held;
      this.held = [];
      for (const h of waiting) {
        if (h.unit.configId === config.configId) this.unit(h.unit);
        else if (serialAfter(h.unit.configId, config.configId, 8)) this.held.push(h);
      }
    });
  }

  unit(unit: AccessUnit): void {
    if (this.stopped) return;
    const cur = this.current;
    if (cur && unit.configId !== cur.configId && !serialAfter(unit.configId, cur.configId, 8)) {
      return; // older than the current configuration
    }
    if (!cur || unit.configId !== cur.configId || !this.decoder) {
      this.hold(unit);
      return;
    }
    // A decoder that never catches up would queue without bound while the skip waits to re-arm:
    // at the cap every unit is dropped, a keyframe too, and another keyframe asked for (throttled).
    if (this.decoder.decodeQueueSize >= MAX_DECODE_QUEUE_HARD) {
      if (unit.keyframe || !this.needKey) this.wantKey("late");
      return;
    }
    // A burst of jank drains on its own; only a decoder behind for over BACKLOG_MS is not keeping
    // pace, and then one keyframe costs less latency than draining. Once per keyframe decoded,
    // so a slow decoder is not left on keyframes alone.
    if (this.decoder.decodeQueueSize <= MAX_DECODE_QUEUE) this.backlogSince = null;
    else this.backlogSince ??= this.now();
    if (
      !unit.keyframe &&
      !this.needKey &&
      this.skipArmed &&
      this.backlogSince !== null &&
      this.now() - this.backlogSince > BACKLOG_MS
    ) {
      this.skipArmed = false;
      this.backlogSince = null;
      this.wantKey("late");
    }
    if (unit.keyframe && !this.skipArmed) this.rearmAt ??= unit.captureUs;
    if (this.needKey && !unit.keyframe) {
      if (!this.askedKey) this.opts.requestKeyframe("startup");
      this.askedKey = true;
      return;
    }
    this.needKey = false;
    try {
      // The capture time rides as the chunk timestamp, so each VideoFrame carries it out.
      // Transferred, not copied again: the unit's buffer is the decoder's from here.
      const init = {
        type: unit.keyframe ? "key" : "delta",
        timestamp: unit.captureUs,
        data: unit.data,
        transfer: [unit.data.buffer],
      };
      this.decoder.decode(new EncodedVideoChunk(init as EncodedVideoChunkInit));
    } catch {
      this.restart();
    }
  }

  close(): void {
    this.stopped = true;
    this.held = [];
    if (this.decoder && this.decoder.state !== "closed") this.decoder.close();
    this.decoder = null;
  }

  /** Skip to the next keyframe; with a `reason`, ask the agent for it now. */
  private wantKey(reason?: KeyframeReason): void {
    this.needKey = true;
    this.askedKey = reason !== undefined;
    if (reason) this.opts.requestKeyframe(reason);
  }

  private hold(unit: AccessUnit): void {
    const at = this.now();
    if (this.held.length > 0 && at - this.held[0].at >= HOLD_MS) {
      this.held = [];
      this.opts.requestKeyframe("decode_error");
    }
    this.held.push({ unit, at });
  }

  private fatal(reason: DecodeFailure, config: VideoConfig): void {
    this.close();
    this.opts.onFatal(reason, config);
  }

  /** Puts `config` on `decoder` (or a new one) and publishes it as `this.decoder`. */
  private async configure(config: VideoConfig, generation: number, decoder: VideoDecoder | null): Promise<void> {
    const decoderConfig: VideoDecoderConfig = {
      codec: codecString(config),
      // av1C is optional for AV1 (the sequence header rides in-band).
      ...(config.description.length > 0 ? { description: config.description } : {}),
      codedWidth: config.width,
      codedHeight: config.height,
      optimizeForLatency: true,
      ...(config.colorSpace ? { colorSpace: config.colorSpace } : {}),
    };
    const check = this.opts.isConfigSupported ?? ((c) => VideoDecoder.isConfigSupported(c));
    const support = await check(decoderConfig).catch(() => ({ supported: false }));
    if (generation !== this.generation || this.stopped) {
      if (decoder && decoder !== this.decoder && decoder.state !== "closed") decoder.close();
      return;
    }
    if (!support.supported) {
      decoder?.close();
      this.fatal("decoder_unsupported", config);
      return;
    }
    try {
      const d = decoder ?? this.makeDecoder();
      d.configure(decoderConfig);
      this.decoder = d;
    } catch {
      // isConfigSupported passed and configure() still refused: no keyframe will fix that.
      this.fatal("decoder_unsupported", config);
    }
  }

  private makeDecoder(): VideoDecoder {
    const init: VideoDecoderInit = {
      output: (frame) => {
        if (this.stopped) {
          frame.close();
          return;
        }
        this.restarts = 0;
        if (this.rearmAt !== null && frame.timestamp >= this.rearmAt) {
          this.skipArmed = true;
          this.rearmAt = null;
        }
        this.opts.onFrame(frame);
      },
      error: () => this.restart(),
    };
    return this.opts.createDecoder ? this.opts.createDecoder(init) : new VideoDecoder(init);
  }

  /** A decode error closes the decoder: make a new one under the same configuration and
   *  resume at a keyframe, at most {@link MAX_DECODER_RESTARTS} times in a row. */
  private restart(): void {
    const config = this.current;
    if (this.stopped || !config) return;
    if (++this.restarts > MAX_DECODER_RESTARTS) {
      this.fatal("decode_error", config);
      return;
    }
    if (this.decoder && this.decoder.state !== "closed") this.decoder.close();
    this.decoder = null;
    this.current = null;
    this.config(config);
    // This request is the new decoder's keyframe: no "startup" one on top of it.
    this.askedKey = true;
    this.opts.requestKeyframe("decode_error");
  }
}
