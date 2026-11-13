// Game audio on the WebTransport path: Opus packets -> WebCodecs AudioDecoder -> an AudioWorklet
// that plays each chunk at the render frame audioSync picked for its capture time.
// protocol/transport-webtransport.md §Audio. Audio never ends the session: every failure
// here is logged and leaves video running.

import type { AudioPacket } from "./framing";
import { AudioSync } from "./audioSync";
import type { RingStats } from "./audioRing";
import { serialAfter } from "./framing";
import type { AudioConfig } from "./transport";

/** Decoder errors in a row, with no audio decoded between, before giving up on audio. */
const MAX_DECODER_RESTARTS = 3;
/** Posting a chunk to the audio thread and rendering it takes about this long. */
const POST_MARGIN_MS = 15;
/** Chunks in a row that were due before they could be posted: the path's delay has stepped
 *  (a handover, a reroute) and the delay floor measured before it is stale. 20 is 200 ms. */
export const LATE_RUN_RESET = 20;
/** The ramp after a concealed gap, so the first real chunk does not click in (2.5 ms). */
const FADE_IN_FRAMES = 120;

export interface AudioOutStats {
  targetDelayMs: number;
  videoLatencyMs: number | null;
  /** Audio is heard this much after its video (negative: before). */
  avOffsetMs: number | null;
  underrunMs: number;
  /** The context has run with the worklet, so the counts below are measured; false: do not post them. */
  measured: boolean;
  /** Frames of audio that were not the stream's: silence for want of data, or a concealment fade that played. Cumulative. */
  concealedSamples: number;
  /** Gaps in the stream's audio, a run of silence and the fade before it counting once. Cumulative. */
  concealmentEvents: number;
  dropped: number;
  decoded: number;
  /** Packets dropped for a sequence number already seen or behind one played. */
  stale: number;
  /** Gaps filled with a concealed chunk. */
  concealed: number;
  /** Chunks not posted because their time had passed. */
  late: number;
  /** The playout ring's buffer, for the client.wt_audio_* series; null until the ring has
   *  played audio, so a session with none posts nothing. */
  buffer: AudioBufferStats | null;
}

/** What the playout ring's buffer was, and aimed to be, in the window since the last stats() (#1346). */
export interface AudioBufferStats {
  /** The least stream audio queued at a quantum since the previous stats() call, ms. 0 is a ring
   *  that ran empty, which is the underrun this was added to tell apart from concealed packets. */
  minMs: number;
  /** The depth the ring is aiming for now: the largest of the video-matched base target, the
   *  output device and posting floor, and that floor plus the measured jitter, ms (#1346). */
  targetMs: number;
  /** CUMULATIVE: times the ring ran empty and played silence, not a lost packet's fade. */
  underruns: number;
}

/** The slice of AudioContext this uses; a test supplies a fake. */
export interface ContextLike {
  readonly state: string;
  readonly sampleRate: number;
  readonly currentTime: number;
  readonly outputLatency?: number;
  readonly baseLatency?: number;
  getOutputTimestamp(): { contextTime?: number; performanceTime?: number };
  /** Called when the context's `state` changes. */
  onstatechange: (() => void) | null;
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
    get onstatechange() {
      return ctx.onstatechange as (() => void) | null;
    },
    set onstatechange(fn) {
      ctx.onstatechange = fn;
    },
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
  private stale = 0;
  private concealed = 0;
  /** Latched: a player that ran and was then suspended still owes the dropouts it counted. */
  private hasMeasured = false;
  private late = 0;
  /** The last packet taken, by `packet_seq`; null before the first and after a pause. */
  private lastSeq: number | null = null;
  /** Capture times of the packets in the decoder, in order: one Opus packet in, one chunk out. */
  private inFlight: { captureUs: number; fadeIn: boolean }[] = [];
  /** The last chunk decoded, kept to fade out of when a packet is lost. */
  private lastPlanes: Float32Array[] | null = null;
  private lateRun = 0;
  private worklet: RingStats = {
    underrunFrames: 0,
    concealedFrames: 0,
    underrunEvents: 0,
    silenceRuns: 0,
    dropped: 0,
    minQueuedFrames: Infinity,
  };
  /** The least the worklet has had queued since the previous stats(): the telemetry posts once a
   *  tick and the worklet reports about eight times a tick, so this spans its reports. */
  private bufferMin = Infinity;
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
    out.port.onmessage = (e: MessageEvent<RingStats>) => {
      this.hasMeasured = true; // the worklet reports only while the context runs
      this.worklet = e.data;
      // Infinity (nothing played in that window) must not erase what an earlier one measured.
      if (Number.isFinite(e.data.minQueuedFrames)) this.bufferMin = Math.min(this.bufferMin, e.data.minQueuedFrames);
    };
    out.setGain(this.held ? 0 : 1);
    // A context made without a user gesture starts suspended; the browser lets it resume on the
    // gesture the page is already inside (release), and again whenever it suspends under us.
    ctx.onstatechange = () => {
      if (!this.held && !this.closed && ctx.state !== "running" && ctx.state !== "closed") {
        void ctx.resume().catch(() => {});
      }
    };
    if (!this.held) void ctx.resume().catch(() => {});
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
    const gap = this.sequence(p.packetSeq);
    if (gap === null) {
      this.stale += 1;
      return;
    }
    this.sync.noteAudio(p.captureUs, this.now());
    const decoder = this.decoder;
    // A suspended context's clock is not moving: what it was given now would play stale later.
    if (!decoder || decoder.state !== "configured" || this.ctx?.state !== "running") {
      this.lastPlanes = null;
      return;
    }
    if (gap > 0) this.conceal(p.captureUs - (this.config?.ptimeMs ?? 10) * 1000);
    try {
      // The chunk after a gap fades in, whichever chunks are still in the decoder before it.
      this.inFlight.push({ captureUs: p.captureUs, fadeIn: gap > 0 });
      decoder.decode(new EncodedAudioChunk({ type: "key", timestamp: p.captureUs, data: p.opus }));
    } catch {
      this.restart();
    }
  }

  /** Takes `seq` into the order: the packets lost before it (0 for the next in line), or null
   *  for one already seen, or behind one played. */
  private sequence(seq: number): number | null {
    if (this.lastSeq !== null) {
      if (!serialAfter(seq, this.lastSeq, 16)) return null;
      const gap = (seq - this.lastSeq - 1 + 0x10000) % 0x10000;
      // Seconds of loss are a pause or a restarted stream, not packets to make up.
      this.lastSeq = seq;
      return gap > 100 ? 0 : gap;
    }
    this.lastSeq = seq;
    return 0;
  }

  /** One 10 ms chunk in place of the lost packet: the last chunk fading to silence. WebCodecs'
   *  Opus decoder takes no lost-packet input (no PLC), so this is the concealment. */
  private conceal(captureUs: number): void {
    const last = this.lastPlanes;
    if (!last) return;
    const planes = last.map((plane) => {
      const faded = plane.slice();
      for (let i = 0; i < faded.length; i++) faded[i] *= 1 - (i + 1) / faded.length;
      return faded;
    });
    this.concealed += 1;
    this.schedule(captureUs, planes, true);
  }

  stats(): AudioOutStats {
    this.hasMeasured ||= this.out !== null && this.ctx?.state === "running";
    const rate = this.ctx?.sampleRate ?? 48_000;
    // The buffer minimum is per window: reading it starts the next one, because telemetry posts
    // one of these a tick. No report since the last read means no audio is playing to measure.
    const queued = this.bufferMin;
    this.bufferMin = Infinity;
    const targetDelayMs = this.sync.targetDelayMs(this.minDelayMs());
    return {
      targetDelayMs,
      videoLatencyMs: this.sync.videoLatencyMs(),
      avOffsetMs: this.sync.avOffsetMs(this.minDelayMs()),
      underrunMs: (this.worklet.underrunFrames / rate) * 1000,
      measured: this.hasMeasured,
      concealedSamples: this.worklet.underrunFrames + this.worklet.concealedFrames,
      concealmentEvents: this.worklet.underrunEvents,
      buffer: Number.isFinite(queued)
        ? { minMs: (queued / rate) * 1000, targetMs: targetDelayMs, underruns: this.worklet.silenceRuns }
        : null,
      dropped: this.worklet.dropped,
      decoded: this.decoded,
      stale: this.stale,
      concealed: this.concealed,
      late: this.late,
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
    // Whatever the old decoder held is gone: its capture times must not label the new one's chunks.
    this.inFlight = [];
    this.lastPlanes = null;
    if (this.decoder && this.decoder.state !== "closed") this.decoder.close();
    this.decoder = null;
    if (++this.restarts > MAX_DECODER_RESTARTS) {
      console.warn("audio: the decoder keeps failing; the stream is silent");
      return;
    }
    this.makeDecoder();
  }

  /** One decoded chunk, scheduled from its own packet's capture time: the decoder is one packet
   *  in, one chunk out, so the capture times queue up in step with it. A lost packet moves
   *  none of its neighbours' times. */
  private play(data: AudioData): void {
    try {
      if (this.closed || !this.ctx || !this.out) return;
      this.restarts = 0;
      this.decoded += 1;
      const sent = this.inFlight.shift();
      const captureUs = sent?.captureUs ?? data.timestamp;
      const planes: Float32Array[] = [];
      for (let c = 0; c < data.numberOfChannels; c++) {
        const plane = new Float32Array(data.numberOfFrames);
        data.copyTo(plane, { planeIndex: c, format: "f32-planar" });
        planes.push(plane);
      }
      this.lastPlanes = planes.map((p) => p.slice());
      if (sent?.fadeIn) {
        for (const plane of planes) {
          for (let i = 0; i < Math.min(FADE_IN_FRAMES, plane.length); i++) plane[i] *= i / FADE_IN_FRAMES;
        }
      }
      this.schedule(captureUs, planes);
    } finally {
      data.close();
    }
  }

  /** Post `planes` for the render frame `captureUs` maps to. A chunk whose time has passed is not
   *  posted; a run of them means the delay floor is stale, and it is measured again. */
  private schedule(captureUs: number, planes: Float32Array[], concealed = false): void {
    const ctx = this.ctx;
    const out = this.out;
    if (!ctx || !out) return;
    const playAt = this.sync.playAtMs(captureUs, this.minDelayMs());
    if (playAt === null) return;
    const now = this.now();
    const outputLatencyMs = (ctx.outputLatency || ctx.baseLatency || 0) * 1000;
    if (playAt - now < outputLatencyMs) {
      this.late += 1;
      if (++this.lateRun >= LATE_RUN_RESET) {
        this.lateRun = 0;
        this.sync.reset();
      }
      return;
    }
    this.lateRun = 0;
    // The output timeline: where the speaker is now, and the wall time that was.
    const ts = ctx.getOutputTimestamp();
    const contextTime = ts.contextTime || ctx.currentTime;
    const performanceTime = ts.performanceTime || now;
    const start = Math.round((contextTime + (playAt - performanceTime) / 1000) * ctx.sampleRate);
    out.port.postMessage({ start, planes, concealed }, planes.map((p) => p.buffer));
  }
}
