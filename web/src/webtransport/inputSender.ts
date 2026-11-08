// Input over WebTransport (protocol/transport-webtransport.md §Input): the same input.md JSON
// as the DataChannel, on one of two paths. A lost datagram is lost, so anything that changes
// held state on the host goes on the control stream, and so does the final state of anything
// that was last sent as a datagram.

import type { InputMsg, InputSender } from "../input/inputSender";
import { CHANNEL_INPUT } from "./framing";

/** The two paths input takes on a WebTransport connection. */
export interface InputWire {
  /** `{type:"input",event}` on the control stream: reliable and ordered. */
  reliable(event: InputMsg): void;
  /** One datagram; false when it does not fit, and the sender then goes reliable. */
  datagram(bytes: Uint8Array): boolean;
}

/** After this long without another datagram, a pad's or the pointer's latest state is sent again reliably. */
export const SETTLE_MS = 100;

const encoder = new TextEncoder();

export function webTransportInputSender(wire: InputWire, settleMs = SETTLE_MS): InputSender & { dispose(): void } {
  /** The last button values sent per pad; every one is a value, triggers are analog. */
  const pads = new Map<number, number[]>();
  /** The latest state of what was last sent as a datagram, until it has been repeated reliably. */
  const settling = new Map<string, { msg: InputMsg; timer: ReturnType<typeof setTimeout> }>();
  let disposed = false;
  const scratch = new Uint8Array(1200);
  scratch[0] = CHANNEL_INPUT;

  const drop = (key: string): InputMsg | undefined => {
    const s = settling.get(key);
    if (!s) return undefined;
    clearTimeout(s.timer);
    settling.delete(key);
    return s.msg;
  };
  const settleLater = (key: string, msg: InputMsg) => {
    drop(key);
    settling.set(key, {
      msg,
      timer: setTimeout(() => {
        settling.delete(key);
        wire.reliable(msg);
      }, settleMs),
    });
  };
  const padKey = (msg: InputMsg) => `gp:${typeof msg.i === "number" ? msg.i : 0}`;

  const lossy = (msg: InputMsg): boolean => {
    switch (msg.t) {
      case "mm":
      case "ma":
      case "ms":
        return true;
      case "gp": {
        // Any button value that moved, a trigger included: the agent drives its analog triggers
        // from the button values. Only a pure stick change is a datagram.
        const pad = typeof msg.i === "number" ? msg.i : 0;
        const now = Array.isArray(msg.buttons) ? [...(msg.buttons as number[])] : [];
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
      // A stale capture handler must not reach a connection that is gone or replaced.
      if (disposed) return;
      if (lossy(msg)) {
        const json = JSON.stringify(msg);
        const { read, written } = encoder.encodeInto(json, scratch.subarray(1));
        // read < json.length: it overran the scratch; the wire decides the real limit.
        if (read === json.length && wire.datagram(scratch.slice(0, 1 + written))) {
          if (msg.t === "ma") settleLater("ma", { t: "ma", x: msg.x, y: msg.y });
          else if (msg.t === "gp") settleLater(padKey(msg), JSON.parse(json) as InputMsg);
          return; // mm and ms are relative: nothing to settle
        }
      }
      // A click must land where the pointer was, and a datagram `ma` can lose or trail the reliable
      // `mb`: the position goes first on the same ordered stream. Relative motion has no position to
      // repeat, so a late `mm` just shifts the cursor slightly; accepted.
      if (msg.t === "mb") {
        const abs = drop("ma");
        if (abs) wire.reliable(abs);
      } else if (msg.t === "ma") drop("ma");
      else if (msg.t === "gp") drop(padKey(msg)); // this reliable state is the newest
      wire.reliable(msg);
    },
    // shortcut: the browser exposes no queue depth for datagrams; upgrade if backpressure needs it (#1154).
    bufferedAmount: 0,
    /** Forgets what was waiting to settle: the connection is going, and the host releases what it held. */
    dispose() {
      disposed = true;
      for (const key of [...settling.keys()]) drop(key);
    },
  };
}
