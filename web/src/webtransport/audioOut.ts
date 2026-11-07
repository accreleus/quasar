// Game audio on the WebTransport path: Opus packets -> WebCodecs AudioDecoder -> an AudioWorklet
// that plays each chunk at the render frame audioSync picked for its capture time.
// protocol/transport-webtransport.md §Audio. Audio never ends the session: every failure
// here is logged and leaves video running.

import type { AudioPacket } from "./framing";
import { AudioSync } from "./audioSync";
import type { AudioConfig } from "./transport";

/** Decoder errors in a row, with no audio decoded between, before giving up on audio. */
const MAX_DECODER_RESTARTS = 3;
/** Posting a chunk to the audio thread and rendering it takes about this long. */
const POST_MARGIN_MS = 15;

export interface AudioOutStats {
  targetDelayMs: number;
  videoLatencyMs: number | null;
  /** Audio is heard this much after its video (negative: before). */
  avOffsetMs: number | null;
  underrunMs: number;
  dropped: number;
  decoded: number;
}

/** The slice of AudioContext this uses; a test supplies a fake. */
export interface ContextLike {
  readonly state: string;
  readonly sampleRate: number;
  readonly currentTime: number;
  readonly outputLatency?: number;
  readonly baseLatency?: number;
  getOutputTimestamp(): { contextTime?: number; performanceTime?: number };
  resume(): Promise<void>;
  close(): Promise<void>;
  /** Builds the worklet node, already connected to the output through a gain. */
  addOutput(): Promise<{ port: MessagePort; setGain(g: number): void }>;
}

export interface AudioOutDeps {
  createContext?: () => ContextLike;
  createDecoder?: (init: AudioDecoderInit) => AudioDecoder;
  isConfigSupported?: (c: AudioDecoderConfig) => Promise<AudioDecoderSupport>;
  now?: () => number;
}

/** The browser's AudioContext, with the quasar-audio-out worklet behind a gain node. */
export function browserContext(): ContextLike {
  const ctx = new AudioContext({ sampleRate: 48_000, latencyHint: "interactive" });
  return {
    get state() {
      return ctx.state;
    },
    sampleRate: ctx.sampleRate,
    get currentTime() {
      return ctx.currentTime;
    },
    get outputLatency() {
      return ctx.outputLatency;
    },
    get baseLatency() {
      return ctx.baseLatency;
    },
    getOutputTimestamp: () => ctx.getOutputTimestamp(),
    resume: () => ctx.resume(),
    close: () => ctx.close(),
    async addOutput() {
      // Imported here so nothing that never plays audio (tests) pulls the bundler's worker URL in.
      const { default: workletUrl } = await import("./audioWorklet.ts?worker&url");
      await ctx.audioWorklet.addModule(workletUrl);
      const node = new AudioWorkletNode(ctx, "quasar-audio-out", { numberOfInputs: 0, outputChannelCount: [2] });
      const gain = ctx.createGain();
      gain.gain.value = 0;
      node.connect(gain).connect(ctx.destination);
      return { port: node.port, setGain: (g) => void (gain.gain.value = g) };
    },
  };
}

export class AudioOut {
  private ctx: ContextLike | null = null;
  private out: { port: MessagePort; setGain(g: number): void } | null = null;
  private decoder: AudioDecoder | null = null;
  private config: AudioConfig | null = null;
  private held = true;
  private closed = false;
  private restarts = 0;
  private decoded = 0;
  private worklet = { underrunFrames: 0, dropped: 0 };
  private readonly sync: AudioSync;
  private readonly now: () => number;

  constructor(private readonly deps: AudioOutDeps = {}) {
    this.now = deps.now ?? (() => performance.now());
    this.sync = new AudioSync();
  }

  /** Builds the context, the worklet and the decoder. Resolves when packets can be taken. */
  async start(config: AudioConfig): Promise<void> {
    this.config = config;
    const decoderConfig = this.decoderConfig(config);
    const check = this.deps.isConfigSupported ?? ((c) => AudioDecoder.isConfigSupported(c));
    if (!(await check(decoderConfig).catch(() => ({ supported: false }))).supported) {
      console.warn("audio: this browser cannot decode the session's Opus audio; the stream is silent");
      return;
    }
    const ctx = (this.deps.createContext ?? browserContext)();
    let out: { port: MessagePort; setGain(g: number): void };
    try {
      out = await ctx.addOutput();
    } catch (err) {
      console.warn("audio: the audio worklet did not load; the stream is silent", err);
      void ctx.close().catch(() => {});
      return;
    }
    if (this.closed) {
      void ctx.close().catch(() => {});
      return;
    }
    this.ctx = ctx;
    this.out = out;
    out.port.onmessage = (e: MessageEvent<{ underrunFrames: number; dropped: number }>) => {
      this.worklet = e.data;
    };
    out.setGain(this.held ? 0 : 1);
    this.makeDecoder();
  }

  /** Unmutes. A suspended context needs the user activation the caller is inside, or sticky. */
  release(): void {
    this.held = false;
    this.out?.setGain(1);
    void this.ctx?.resume().catch(() => {});
  }

  /** Mutes, and keeps the audio running so it is in time when released. */
  hold(): void {
    this.held = true;
    this.out?.setGain(0);
  }

  /** A video frame captured at `captureUs` was presented now. */
  noteVideo(captureUs: number): void {
    this.sync.noteVideo(captureUs, this.now());
  }

  /** The packet as it arrived. Audio is realtime: nothing is held for a late or lost one. */
  packet(p: AudioPacket): void {
    this.sync.noteAudio(p.captureUs, this.now());
    const decoder = this.decoder;
    // A suspended context's clock is not moving: what it was given now would play stale later.
    if (!decoder || decoder.state !== "configured" || this.ctx?.state !== "running") return;
    try {
      decoder.decode(
        new EncodedAudioChunk({ type: "key", timestamp: p.captureUs, data: p.opus }),
      );
    } catch {
      this.restart();
    }
  }

  stats(): AudioOutStats {
    const rate = this.ctx?.sampleRate ?? 48_000;
    return {
      targetDelayMs: this.sync.targetDelayMs(this.minDelayMs()),
      videoLatencyMs: this.sync.videoLatencyMs(),
      avOffsetMs: this.sync.avOffsetMs(this.minDelayMs()),
      underrunMs: (this.worklet.underrunFrames / rate) * 1000,
      dropped: this.worklet.dropped,
      decoded: this.decoded,
    };
  }

  close(): void {
    this.closed = true;
    if (this.decoder && this.decoder.state !== "closed") this.decoder.close();
    this.decoder = null;
    void this.ctx?.close().catch(() => {});
    this.ctx = null;
    this.out = null;
  }

  private decoderConfig(c: AudioConfig): AudioDecoderConfig {
    return { codec: "opus", sampleRate: c.sampleRate, numberOfChannels: c.channels };
  }

  /** What the output device itself takes, then what it needs to be posted in time. */
  private minDelayMs(): number {
    const latency = this.ctx?.outputLatency || this.ctx?.baseLatency || 0;
    return latency * 1000 + POST_MARGIN_MS;
  }

  private makeDecoder(): void {
    const config = this.config;
    if (!config || this.closed) return;
    const init: AudioDecoderInit = {
      output: (data) => this.play(data),
      error: () => this.restart(),
    };
    const decoder = this.deps.createDecoder ? this.deps.createDecoder(init) : new AudioDecoder(init);
    decoder.configure(this.decoderConfig(config));
    this.decoder = decoder;
  }

  private restart(): void {
    if (this.closed) return;
    if (this.decoder && this.decoder.state !== "closed") this.decoder.close();
    this.decoder = null;
    if (++this.restarts > MAX_DECODER_RESTARTS) {
      console.warn("audio: the decoder keeps failing; the stream is silent");
      return;
    }
    this.makeDecoder();
  }

  /** One decoded chunk: scheduled at the render frame its capture time maps to. */
  private play(data: AudioData): void {
    const ctx = this.ctx;
    const out = this.out;
    try {
      if (this.closed || !ctx || !out) return;
      this.restarts = 0;
      this.decoded += 1;
      const playAt = this.sync.playAtMs(data.timestamp, this.minDelayMs());
      if (playAt === null) return;
      // The output timeline: where the speaker is now, and the wall time that was.
      const ts = ctx.getOutputTimestamp();
      const contextTime = ts.contextTime || ctx.currentTime;
      const performanceTime = ts.performanceTime || this.now();
      const start = Math.round((contextTime + (playAt - performanceTime) / 1000) * ctx.sampleRate);
      const planes: Float32Array[] = [];
      for (let c = 0; c < data.numberOfChannels; c++) {
        const plane = new Float32Array(data.numberOfFrames);
        data.copyTo(plane, { planeIndex: c, format: "f32-planar" });
        planes.push(plane);
      }
      out.port.postMessage({ start, planes }, planes.map((p) => p.buffer));
    } finally {
      data.close();
    }
  }
}
