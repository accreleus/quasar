// The client side of the transport slot (CLAUDE.md invariant 3): media arrives
// through a MediaTransport as decoder configurations and whole access units, and
// the decode and present stages never see the transport. WebTransport
// (protocol/transport-webtransport.md) is the first implementation here; WebRTC
// still decodes inside the browser's <video> and does not go through this seam.

import type { AudioPacket } from "./framing";
import type { PyroWaveFrame } from "../pyrowave/receiver";
import type { InputWire } from "./inputSender";

/** A `hello_ack.video` (§Hello ack), the decoder configuration a `config_id` names. */
export interface VideoConfig {
  configId: number;
  /** Wire vocabulary: h264 | h265 | av1 | pyrowave. */
  codec: string;
  /** avcC / hvcC / av1C, or PyroWave's PWV1Header. */
  description: Uint8Array;
  width: number;
  height: number;
  fps: number;
  colorSpace?: VideoColorSpaceInit;
  /** `hello_ack.codec_switch_reason` (codec-pyrowave.md §Switching codec). */
  switchReason?: string;
}

/** A `hello_ack.audio` or `.mic` (§Hello ack): 10 ms Opus packets. */
export interface AudioConfig {
  channels: number;
  sampleRate: number;
  ptimeMs: number;
}

/** What the session carries besides video; null for what it lacks (no audio, no mic grant). */
export interface AudioStreams {
  audio: AudioConfig | null;
  mic: AudioConfig | null;
}

/** One access unit, whole, in the form `EncodedVideoChunk` takes (§Video). */
export interface AccessUnit {
  keyframe: boolean;
  configId: number;
  frameSeq: number;
  ts90k: number;
  /** The agent's wall clock at capture, Unix µs. */
  captureUs: number;
  data: Uint8Array;
}

export type KeyframeReason = "startup" | "loss" | "late" | "decode_error";

/** How the transport ended. `code` is the WebTransport close code (§Bye and error),
 *  null when the connection failed without one. */
export interface TransportEnd {
  code: number | null;
  reason: string;
}

export interface MediaSink {
  /** Every `hello_ack`, the first included, in control-stream order. */
  config(config: VideoConfig): void;
  /** The first `hello_ack`'s audio and microphone, before any datagram is read. */
  streams?(streams: AudioStreams): void;
  /** One Opus packet from the agent, as it arrived: lost and reordered ones are the sink's to handle. */
  audio?(packet: AudioPacket): void;
  /** Reassembled units, in completion order. */
  unit(unit: AccessUnit): void;
  /** PyroWave frames, complete or at their deadline, in frame_seq order. */
  pyrowave(frame: PyroWaveFrame): void;
  /** Called once, after connect resolved, when the transport is gone. */
  end(end: TransportEnd): void;
  /** A pong, once a second while connected (§Ping and pong): the round trip in ms, and the
   *  agent's wall clock (`tw`, Unix ms) when it answered. */
  rtt?(ms: number, agentWallMs?: number): void;
  /** A `keyframe_request` went on the wire (requests inside the spacing window are merged). */
  keyframeRequested?(reason: KeyframeReason): void;
  /** Video fragments the client has found missing since connect, cumulative: each counted once,
   *  when it is first NACKed, whether or not the resend then arrives. PyroWave: the datagrams
   *  missing from each frame closed or dropped incomplete. */
  lost?(fragments: number): void;
}

export interface MediaTransport {
  /** Resolves at the first `hello_ack`; rejects with a {@link TransportError}. */
  connect(sink: MediaSink): Promise<void>;
  requestKeyframe(reason: KeyframeReason): void;
  /** Sends one `0x05` microphone datagram; false when it was not sent (not connected, too large). */
  sendMic(datagram: Uint8Array): boolean;
  /** `codec_fallback`: this client cannot go on decoding PyroWave (codec-pyrowave.md). */
  codecFallback(reason: string): void;
  /** Where client input goes once connected (§Input). */
  readonly input: InputWire;
  /** Says `bye` when connected; idempotent. Settles once the session has been told to close
   *  (after the bye is flushed, or a short bound when it cannot be): a fallback waits for it. */
  close(): Promise<void> | void;
}

/** A refused or failed connect. `code` is the agent's `error.code`
 *  (§Hello) or `connect_failed`. */
export class TransportError extends Error {
  constructor(readonly code: string, message?: string) {
    super(message ?? code);
    this.name = "TransportError";
  }
}
