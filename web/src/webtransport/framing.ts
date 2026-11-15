// Wire framing of protocol/transport-webtransport.md: the control stream's
// length-prefixed JSON (§Channels) and the `0x01` video datagram header (§Video).

/** No control message is longer than this (§Channels); a longer length is a 4400. */
export const MAX_CONTROL_MESSAGE = 65_536;
export const CHANNEL_VIDEO = 0x01;
export const CHANNEL_INPUT = 0x03;
export const CHANNEL_AUDIO = 0x02;
/** §Audio redundancy: the previous audio packet, a second time (amendment 67). */
export const CHANNEL_AUDIO_RED = 0x06;
export const CHANNEL_MIC = 0x05;
export const CHANNEL_FEC = 0x04;
const FEC_HEADER = 14;
const FEC_SCHEME_XOR = 1;
/** §FEC: one repair datagram protects 2 to 48 fragments. */
const FEC_MAX_COUNT = 48;
const VIDEO_HEADER = 25;
const AUDIO_HEADER = 19;
const FLAG_KEYFRAME = 1;
const FLAG_DISCARDABLE = 2;
const FLAG_RETRANSMIT = 4;

/** u32 LE length, then UTF-8 JSON. */
export function encodeControl(message: object): Uint8Array {
  const body = new TextEncoder().encode(JSON.stringify(message));
  const out = new Uint8Array(4 + body.length);
  new DataView(out.buffer).setUint32(0, body.length, true);
  out.set(body, 4);
  return out;
}

export class ControlProtocolError extends Error {}

/** Splits the control stream's byte chunks back into messages. */
export class ControlDecoder {
  private buf = new Uint8Array(0);

  /** Every whole message `chunk` completes; throws {@link ControlProtocolError} on an
   *  oversize length or a body that is not a JSON object. */
  push(chunk: Uint8Array): Record<string, unknown>[] {
    const joined = new Uint8Array(this.buf.length + chunk.length);
    joined.set(this.buf);
    joined.set(chunk, this.buf.length);
    const out: Record<string, unknown>[] = [];
    let at = 0;
    while (joined.length - at >= 4) {
      const len = new DataView(joined.buffer, at, 4).getUint32(0, true);
      if (len > MAX_CONTROL_MESSAGE) throw new ControlProtocolError("control message over 64 KiB");
      if (joined.length - at - 4 < len) break;
      let message: unknown;
      try {
        message = JSON.parse(new TextDecoder().decode(joined.subarray(at + 4, at + 4 + len)));
      } catch {
        throw new ControlProtocolError("malformed control message");
      }
      if (typeof message !== "object" || message === null || Array.isArray(message)) {
        throw new ControlProtocolError("control message is not an object");
      }
      out.push(message as Record<string, unknown>);
      at += 4 + len;
    }
    this.buf = joined.slice(at);
    return out;
  }
}

export interface VideoFragment {
  pktSeq: number;
  keyframe: boolean;
  discardable: boolean;
  /** Re-sent in answer to a `nack` (§NACK). */
  retransmit: boolean;
  configId: number;
  frameSeq: number;
  fragIndex: number;
  fragCount: number;
  ts90k: number;
  captureUs: number;
  /** A view into the datagram, not a copy. */
  payload: Uint8Array;
}

/** A `0x01` datagram's header and payload; null for another channel or a malformed one. */
export function parseVideoDatagram(d: Uint8Array): VideoFragment | null {
  if (d.length < VIDEO_HEADER || d[0] !== CHANNEL_VIDEO) return null;
  const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const fragIndex = v.getUint16(9, true);
  const fragCount = v.getUint16(11, true);
  if (fragCount === 0 || fragIndex >= fragCount) return null;
  const flags = d[5];
  return {
    pktSeq: v.getUint32(1, true),
    keyframe: (flags & FLAG_KEYFRAME) !== 0,
    discardable: (flags & FLAG_DISCARDABLE) !== 0,
    retransmit: (flags & FLAG_RETRANSMIT) !== 0,
    configId: d[6],
    frameSeq: v.getUint16(7, true),
    fragIndex,
    fragCount,
    ts90k: v.getUint32(13, true),
    // Unix µs sits near 2^51, inside a double's exact range.
    captureUs: Number(v.getBigUint64(17, true)),
    payload: d.subarray(VIDEO_HEADER),
  };
}

/** A `0x04` repair datagram of scheme `xor` (§FEC): it rebuilds any one of its fragments. */
export interface FecRepair {
  frameSeq: number;
  /** The fragments it protects: `firstFrag + i * stride` for `i` in `0 … count − 1`. */
  firstFrag: number;
  count: number;
  stride: number;
  /** XOR of the protected payloads' lengths. */
  lengthXor: number;
  /** XOR of the protected payloads, each zero-padded to the longest. A view, not a copy. */
  payload: Uint8Array;
}

/** A `0x04` datagram of a scheme this client decodes; null for anything else or a malformed one. */
export function parseFecDatagram(d: Uint8Array): FecRepair | null {
  if (d.length <= FEC_HEADER || d[0] !== CHANNEL_FEC || d[5] !== FEC_SCHEME_XOR) return null;
  const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const count = d[10];
  const stride = d[11];
  if (count < 2 || count > FEC_MAX_COUNT || stride < 1) return null;
  return {
    frameSeq: v.getUint16(6, true),
    firstFrag: v.getUint16(8, true),
    count,
    stride,
    lengthXor: v.getUint16(12, true),
    payload: d.subarray(FEC_HEADER),
  };
}

export interface AudioPacket {
  packetSeq: number;
  ts48k: number;
  /** The agent's wall clock when the packet's 10 ms began, Unix µs. */
  captureUs: number;
  /** One Opus packet: a view into the datagram, not a copy. */
  opus: Uint8Array;
}

/** A `0x02` datagram's header and Opus packet, or a `0x06` copy of one (§Audio redundancy);
 *  null for another channel or a malformed one. */
export function parseAudioDatagram(d: Uint8Array): AudioPacket | null {
  if (d.length <= AUDIO_HEADER || (d[0] !== CHANNEL_AUDIO && d[0] !== CHANNEL_AUDIO_RED)) return null;
  const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
  return {
    packetSeq: v.getUint16(5, true),
    ts48k: v.getUint32(7, true),
    captureUs: Number(v.getBigUint64(11, true)),
    opus: d.subarray(AUDIO_HEADER),
  };
}

/** A `0x05` microphone datagram (§Microphone): channel, u16 packet_seq, u32 ts_48khz, Opus. */
export function encodeMicDatagram(packetSeq: number, ts48k: number, opus: Uint8Array): Uint8Array {
  const d = new Uint8Array(7 + opus.length);
  const v = new DataView(d.buffer);
  d[0] = CHANNEL_MIC;
  v.setUint16(1, packetSeq & 0xffff, true);
  v.setUint32(3, ts48k >>> 0, true);
  d.set(opus, 7);
  return d;
}

/** `a` is after `b` in `bits`-bit serial-number arithmetic (§Channels, "Wrap-around"). */
export function serialAfter(a: number, b: number, bits: number): boolean {
  const mod = 2 ** bits;
  const d = (((a - b) % mod) + mod) % mod;
  return d !== 0 && d < mod / 2;
}

/** Base64 to bytes, for `description_b64` and `cert_hashes[].value`. */
export function base64Bytes(b64: string): Uint8Array<ArrayBuffer> {
  const s = atob(b64);
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
  return out;
}
