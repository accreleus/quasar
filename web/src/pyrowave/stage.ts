// PyroWave frames to the screen: one decoder per configuration (concealment reuses that
// decoder's earlier coefficients, amendment 55 §Loss), its texture drawn by the shared
// presenter. A decoder that cannot go on asks the agent for the standard codec, once per
// configuration (codec-pyrowave.md `codec_fallback`).

import type { VideoConfig } from "../webtransport/transport";
import type { PyroWaveFrame } from "./receiver";
import type { PyroWaveDecoder } from "./decoder";
import { parsePwv1Header } from "./bitstream";

type DecoderModule = typeof import("./decoder");

export interface PyroWaveStageOptions {
  device: GPUDevice;
  /** Draws the decoded texture; false when nothing was drawn. */
  present(texture: GPUTexture): boolean;
  /** A frame reached the screen; its capture time, Unix µs. */
  onPresented(captureUs: number): void;
  /** `codec_fallback`, at most once per configuration. */
  fallback(reason: string): void;
  /** Test seam; the dynamic import otherwise. */
  load?: () => Promise<DecoderModule>;
}

export const loadDecoder = (): Promise<DecoderModule> => import("./decoder");

/** Decodes allowed to be outstanding at once (#1416). Two, not one: a completion callback can land
 *  a frame late, and a gate of one would then halve the frame rate of a GPU that is keeping up. */
export const MAX_IN_FLIGHT_DECODES = 2;

/** Compiles the decoder's pipelines on `device` once, so a configuration later builds fast;
 *  false when this device cannot run either decoder path. */
export async function warmPyroWave(device: GPUDevice, load: () => Promise<DecoderModule> = loadDecoder): Promise<boolean> {
  try {
    const { PyroWaveDecoder } = await load();
    const d = await PyroWaveDecoder.create(device, { width: 128, height: 128, fullRange: false }, { maxPixels: 128 * 128 });
    d.destroy();
    return true;
  } catch (err) {
    console.warn("pyrowave: the decoder cannot run on this device", err);
    return false;
  }
}

export class PyroWaveStage {
  private decoder: PyroWaveDecoder | null = null;
  private configId: number | null = null;
  /** The newest frame that came while the decoder was being built; decoded once it is. */
  private waiting: PyroWaveFrame | null = null;
  /** The newest frame that came while a decode was still running; older ones drop (#1416). */
  private pending: PyroWaveFrame | null = null;
  /** Decodes submitted whose GPU work has not completed yet. */
  private inFlight = 0;
  /** Bumped per configuration, so a late create cannot install an old decoder. */
  private generation = 0;
  private fellBack = new Set<number>();
  private stopped = false;

  constructor(private readonly opts: PyroWaveStageOptions) {}

  /** A PyroWave configuration: frames of it decode once its decoder is built; earlier ones drop. */
  config(c: VideoConfig): void {
    if (this.stopped) return;
    this.decoder?.destroy();
    this.decoder = null;
    this.waiting = null;
    this.pending = null;
    this.inFlight = 0;
    this.configId = c.configId;
    const generation = ++this.generation;
    void (async () => {
      try {
        const { PyroWaveDecoder } = await (this.opts.load ?? loadDecoder)();
        const header = parsePwv1Header(c.description);
        if (header.width !== c.width || header.height !== c.height) throw new Error("the description disagrees with hello_ack");
        const d = await PyroWaveDecoder.create(this.opts.device, header, { maxPixels: c.width * c.height });
        if (generation !== this.generation || this.stopped) {
          d.destroy();
          return;
        }
        this.decoder = d;
        const w = this.waiting;
        this.waiting = null;
        if (w) this.frame(w);
      } catch (err) {
        if (generation !== this.generation || this.stopped) return;
        console.warn("pyrowave: no decoder for this configuration", err);
        this.fail(c.configId);
      }
    })();
  }

  /** Leaves PyroWave: the standard codec's configuration arrived. */
  leave(): void {
    this.generation++;
    this.decoder?.destroy();
    this.decoder = null;
    this.configId = null;
    this.waiting = null;
    this.pending = null;
    this.inFlight = 0;
  }

  frame(f: PyroWaveFrame): void {
    if (this.stopped || f.configId !== this.configId) return;
    const d = this.decoder;
    if (!d) {
      // Only the newest matters: every frame is intra.
      this.waiting = f;
      return;
    }
    // A bounded number of decodes outstanding (#1416): a GPU slower than the frame rate would
    // otherwise be handed every frame, its queue growing without end, and the picture shown would
    // fall further and further behind. Every frame is intra, so a frame that arrives while the
    // bound is full replaces the one already waiting rather than queueing behind it.
    if (this.inFlight >= MAX_IN_FLIGHT_DECODES) {
      this.pending = f;
      return;
    }
    this.decodeNow(d, f);
  }

  private decodeNow(d: PyroWaveDecoder, f: PyroWaveFrame): void {
    this.inFlight++;
    try {
      d.decode(f.packets, f.covered);
    } catch (err) {
      this.inFlight--;
      // A malformed frame is dropped; anything else is this decoder's end.
      if ((err as { reason?: string }).reason === "bad_frame") return;
      console.warn("pyrowave: decode failed", err);
      this.fail(f.configId);
      return;
    }
    if (this.opts.present(d.texture)) this.opts.onPresented(f.captureUs);
    // The submission's completion frees the slot. A lost device abandons its work: settle either
    // way, or the gate would hold the last picture for the rest of the session.
    void this.opts.device.queue.onSubmittedWorkDone().then(
      () => this.decoded(d),
      () => this.decoded(d),
    );
  }

  /** A submitted decode finished: the newest frame that arrived meanwhile takes its place. */
  private decoded(d: PyroWaveDecoder): void {
    // A previous configuration's decode: `leave`/`config` already gave its slots back.
    if (this.decoder !== d) return;
    this.inFlight = Math.max(0, this.inFlight - 1);
    const next = this.pending;
    this.pending = null;
    if (!next || this.stopped || next.configId !== this.configId) return;
    this.decodeNow(d, next);
  }

  close(): void {
    this.stopped = true;
    this.leave();
  }

  private fail(configId: number): void {
    this.leave();
    if (this.fellBack.has(configId)) return;
    this.fellBack.add(configId);
    this.opts.fallback("decode_error");
  }
}
