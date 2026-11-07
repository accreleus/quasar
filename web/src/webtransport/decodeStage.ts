import { serialAfter } from "./framing";
import type { AccessUnit, KeyframeReason, VideoConfig } from "./transport";

/** How long a unit waits for the `hello_ack` of its `config_id` (§Hello ack). */
export const HOLD_MS = 1_000;
/** Frames queued in the decoder before deltas are skipped to the next keyframe: about
 *  50 ms at 60 fps, past which catching up costs more latency than one keyframe does. */
export const MAX_DECODE_QUEUE = 3;
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
    if (!unit.keyframe && !this.needKey && this.decoder.decodeQueueSize > MAX_DECODE_QUEUE) this.wantKey("late");
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
      codec: config.codec === "h264" ? avcCodecString(config.description) : config.codec,
      description: config.description,
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
    this.opts.requestKeyframe("decode_error");
  }
}
