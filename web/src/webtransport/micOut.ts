// The microphone on the WebTransport path: the captured track -> WebCodecs AudioEncoder (Opus,
// 10 ms, mono) -> `0x05` datagrams (protocol/transport-webtransport.md §Microphone). It starts
// only when the page hands it a track, which the page gets from a user gesture (webrtc/mic.ts).

import { MicCaptureError } from "../webrtc/mic";
import { encodeMicDatagram } from "./framing";

/** The track's frames as the browser exposes them (Chrome); not in lib.dom. */
interface TrackProcessor {
  readable: ReadableStream<AudioData>;
}
type TrackProcessorCtor = new (init: { track: MediaStreamTrack }) => TrackProcessor;

export interface MicOutDeps {
  /** One `0x05` datagram; false when it was not sent. */
  send(datagram: Uint8Array): boolean;
  createEncoder?: (init: AudioEncoderInit) => AudioEncoder;
  createProcessor?: (track: MediaStreamTrack) => TrackProcessor;
}

/** The session audio's own settings (10 ms frames, low delay), for voice. `application` and
 *  `signal` are missing from lib.dom's OpusEncoderConfig, hence the wider type. */
const OPUS_SETTINGS: OpusEncoderConfig & { application: string; signal: string } = {
  frameDuration: 10_000,
  application: "lowdelay",
  signal: "voice",
  usedtx: false,
};

const unsupported = () =>
  new MicCaptureError({
    kind: "unsupported",
    title: "Microphone unsupported",
    message: "This browser cannot send the microphone over WebTransport. Use a recent Chrome.",
  });

export class MicOut {
  private encoder: AudioEncoder | null = null;
  private reader: ReadableStreamDefaultReader<AudioData> | null = null;
  private seq = 0;
  /** Bumped by every stop, so a start still awaiting its first frame can tell it was cancelled. */
  private epoch = 0;

  constructor(private readonly deps: MicOutDeps) {}

  /** shortcut: assumes the 48 kHz mono capture Chrome gives the page's constraints; a track
   *  of another rate or width makes the encoder refuse its frames and the mic goes quiet.
   *  Upgrade with a resample/downmix stage if a device is found that does this. */
  start(track: MediaStreamTrack): void {
    this.stop();
    const epoch = this.epoch;
    const Processor = (globalThis as { MediaStreamTrackProcessor?: TrackProcessorCtor }).MediaStreamTrackProcessor;
    if ((!this.deps.createEncoder && typeof AudioEncoder === "undefined") || (!this.deps.createProcessor && !Processor)) {
      throw unsupported();
    }
    const processor = this.deps.createProcessor ? this.deps.createProcessor(track) : new Processor!({ track });
    const encoder = this.deps.createEncoder
      ? this.deps.createEncoder(this.encoderInit())
      : new AudioEncoder(this.encoderInit());
    encoder.configure({
      codec: "opus",
      sampleRate: 48_000,
      numberOfChannels: 1,
      bitrate: 32_000,
      opus: OPUS_SETTINGS,
    });
    this.encoder = encoder;
    const reader = processor.readable.getReader();
    this.reader = reader;
    void (async () => {
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done || this.epoch !== epoch) return;
          try {
            if (encoder.state === "configured") encoder.encode(value);
          } finally {
            value.close();
          }
        }
      } catch {
        /* the track ended or the reader was cancelled */
      }
    })();
  }

  stop(): void {
    this.epoch++;
    void this.reader?.cancel().catch(() => {});
    this.reader = null;
    if (this.encoder && this.encoder.state !== "closed") this.encoder.close();
    this.encoder = null;
  }

  private encoderInit(): AudioEncoderInit {
    return {
      output: (chunk) => {
        const opus = new Uint8Array(chunk.byteLength);
        chunk.copyTo(opus);
        // Chunk timestamps are µs on the capture clock; 48 kHz ticks, as the contract names them.
        this.deps.send(encodeMicDatagram(this.seq++, Math.round(chunk.timestamp * 0.048), opus));
      },
      error: () => this.stop(),
    };
  }
}
