import { serialAfter } from "./framing";
import type { AccessUnit, KeyframeReason, VideoConfig } from "./transport";

/** How long a unit waits for the `hello_ack` of its `config_id` (§Hello ack). */
export const HOLD_MS = 1_000;

export interface DecodeStageOptions {
  /** Each decoded frame; the callee owns it and must close it. */
  onFrame(frame: VideoFrame): void;
  requestKeyframe(reason: KeyframeReason): void;
  /** The browser cannot decode this configuration. */
  onUnsupported(config: VideoConfig): void;
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
  private needKey = true;
  private askedKey = false;
  private closed = false;
  /** Bumped per configure, so a late support check cannot apply an old config. */
  private generation = 0;
  private unsupported = false;
  /** The decoder holds `current`'s configuration. */
  private configured = false;
  private readonly now: () => number;

  constructor(private readonly opts: DecodeStageOptions) {
    this.now = opts.now ?? (() => performance.now());
  }

  config(config: VideoConfig): void {
    if (this.closed) return;
    // A re-sent hello_ack for the live configuration (the agent's resync) changes nothing.
    if (this.current?.configId === config.configId) return;
    this.current = config;
    this.unsupported = false;
    this.configured = false;
    this.needKey = true;
    this.askedKey = false;
    const generation = ++this.generation;
    void this.configure(config, generation).then(() => {
      if (generation !== this.generation || this.closed) return;
      const waiting = this.held;
      this.held = [];
      for (const h of waiting) {
        if (h.unit.configId === config.configId) this.unit(h.unit);
        else if (serialAfter(h.unit.configId, config.configId, 8)) this.held.push(h);
      }
    });
  }

  unit(unit: AccessUnit): void {
    if (this.closed || this.unsupported) return;
    const cur = this.current;
    if (cur && unit.configId !== cur.configId && !serialAfter(unit.configId, cur.configId, 8)) {
      return; // older than the current configuration
    }
    if (!cur || unit.configId !== cur.configId || !this.configured || !this.decoder) {
      this.hold(unit);
      return;
    }
    if (this.needKey && !unit.keyframe) {
      if (!this.askedKey) this.opts.requestKeyframe("startup");
      this.askedKey = true;
      return;
    }
    this.needKey = false;
    try {
      // The capture time rides as the chunk timestamp, so each VideoFrame carries it out.
      this.decoder.decode(
        new EncodedVideoChunk({
          type: unit.keyframe ? "key" : "delta",
          timestamp: unit.captureUs,
          data: unit.data,
        }),
      );
    } catch {
      this.recover();
    }
  }

  close(): void {
    this.closed = true;
    this.held = [];
    if (this.decoder && this.decoder.state !== "closed") this.decoder.close();
    this.decoder = null;
  }

  private hold(unit: AccessUnit): void {
    const at = this.now();
    if (this.held.length > 0 && at - this.held[0].at >= HOLD_MS) {
      this.held = [];
      this.opts.requestKeyframe("decode_error");
    }
    this.held.push({ unit, at });
  }

  private async configure(config: VideoConfig, generation: number): Promise<void> {
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
    if (generation !== this.generation || this.closed) return;
    if (!support.supported) {
      this.unsupported = true;
      this.held = [];
      this.opts.onUnsupported(config);
      return;
    }
    if (!this.decoder || this.decoder.state === "closed") this.decoder = this.makeDecoder();
    this.decoder.configure(decoderConfig);
    this.configured = true;
  }

  private makeDecoder(): VideoDecoder {
    const init: VideoDecoderInit = {
      output: (frame) => {
        if (this.closed) frame.close();
        else this.opts.onFrame(frame);
      },
      error: () => this.recover(),
    };
    return this.opts.createDecoder ? this.opts.createDecoder(init) : new VideoDecoder(init);
  }

  /** A decode error closes the decoder: make a new one under the same configuration
   *  and resume at a keyframe. */
  private recover(): void {
    if (this.closed || !this.current) return;
    if (this.decoder && this.decoder.state !== "closed") this.decoder.close();
    this.decoder = null;
    const config = this.current;
    this.current = null;
    this.config(config);
    this.opts.requestKeyframe("decode_error");
  }
}
