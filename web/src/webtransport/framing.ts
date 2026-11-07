// Wire framing of protocol/transport-webtransport.md: the control stream's
// length-prefixed JSON (§Channels) and the `0x01` video datagram header (§Video).

/** No control message is longer than this (§Channels); a longer length is a 4400. */
export const MAX_CONTROL_MESSAGE = 65_536;
export const CHANNEL_VIDEO = 0x01;
const VIDEO_HEADER = 25;
const FLAG_KEYFRAME = 1;
const FLAG_DISCARDABLE = 2;

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
