// Input over WebTransport (protocol/transport-webtransport.md §Input): the same input.md JSON
// as the DataChannel, on one of two paths. A lost datagram is lost, so a press or release
// goes on the control stream and only continuous input goes as a datagram.

import type { InputMsg, InputSender } from "../input/inputSender";
import { CHANNEL_INPUT } from "./framing";

/** The two paths input takes on a WebTransport connection. */
export interface InputWire {
  /** `{type:"input", event}` on the control stream: reliable and ordered. */
  reliable(event: InputMsg): void;
  /** One datagram; false when it does not fit, and the sender then goes reliable. */
  datagram(bytes: Uint8Array): boolean;
}

/** A pad button counts as pressed from here; the agent reads gamepad buttons the same way. */
const PRESSED = 0.5;

const encoder = new TextEncoder();

export function webTransportInputSender(wire: InputWire): InputSender {
  /** The last pressed-set sent per pad: a `gp` that changes it is a press or release. */
  const pads = new Map<number, boolean[]>();
  /** The latest `ma`, when it went as a datagram and no reliable message has repeated it since. */
  let pendingAbs: { x: unknown; y: unknown } | null = null;
  const scratch = new Uint8Array(1200);
  scratch[0] = CHANNEL_INPUT;

  const lossy = (msg: InputMsg): boolean => {
    switch (msg.t) {
      case "mm":
      case "ma":
      case "ms":
        return true;
      case "gp": {
        const pad = typeof msg.i === "number" ? msg.i : 0;
        const now = (Array.isArray(msg.buttons) ? (msg.buttons as number[]) : []).map((b) => b >= PRESSED);
        const before = pads.get(pad);
        pads.set(pad, now);
        return before !== undefined && before.length === now.length && before.every((b, i) => b === now[i]);
      }
      default:
        return false; // k, mb and anything unknown
    }
  };

  return {
    send(msg) {
      if (lossy(msg)) {
        const json = JSON.stringify(msg);
        const { read, written } = encoder.encodeInto(json, scratch.subarray(1));
        // read < json.length: it overran the scratch; the wire decides the real limit.
        if (read === json.length && wire.datagram(scratch.slice(0, 1 + written))) {
          if (msg.t === "ma") pendingAbs = { x: msg.x, y: msg.y };
          return;
        }
      }
      // A click must land where the pointer was, and a datagram `ma` can lose or trail the reliable
      // `mb`: the position goes first on the same ordered stream. Relative motion has no position to
      // repeat, so a late `mm` just shifts the cursor slightly; accepted.
      if (msg.t === "mb" && pendingAbs) {
        wire.reliable({ t: "ma", x: pendingAbs.x, y: pendingAbs.y });
        pendingAbs = null;
      }
      if (msg.t === "ma") pendingAbs = null;
      wire.reliable(msg);
    },
    // shortcut: the browser exposes no queue depth for datagrams; upgrade if backpressure needs it.
    bufferedAmount: 0,
  };
}
