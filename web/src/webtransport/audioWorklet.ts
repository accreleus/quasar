// The AudioWorkletProcessor for the WebTransport audio path: the shell around AudioRing.
// Loaded through `?worker&url` (audioOut.ts), so it is bundled, same-origin and CSP-clean.

import { AudioRing } from "./audioRing";

declare const currentFrame: number;
declare class AudioWorkletProcessor {
  readonly port: MessagePort;
}
declare function registerProcessor(name: string, processor: new () => AudioWorkletProcessor): void;

/** Quanta between stats posts: about 133 ms. */
const STATS_EVERY = 50;

class QuasarAudioOut extends AudioWorkletProcessor {
  private readonly ring = new AudioRing();
  private quanta = 0;

  constructor() {
    super();
    this.port.onmessage = (e: MessageEvent<{ start: number; planes: Float32Array[]; concealed?: boolean; targetDelayFrames?: number }>) =>
      this.ring.push(e.data.start, e.data.planes, currentFrame, e.data.concealed, e.data.targetDelayFrames);
  }

  process(_inputs: Float32Array[][], outputs: Float32Array[][]): boolean {
    this.ring.render(outputs[0], currentFrame);
    if (++this.quanta % STATS_EVERY === 0) this.port.postMessage(this.ring.takeStats());
    return true;
  }
}

registerProcessor("quasar-audio-out", QuasarAudioOut);
