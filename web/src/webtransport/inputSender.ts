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
  datagramState?(): Pick<WritableStreamDefaultWriter, "desiredSize" | "ready"> | null;
  controlDesiredSize?(): number | null;
}

/** After this long without another datagram, a pad's or the pointer's latest state is sent again reliably. */
export const SETTLE_MS = 100;

const encoder = new TextEncoder();
const full = (size: number | null | undefined) => size != null && size <= 0;

export function webTransportInputSender(wire: InputWire, settleMs = SETTLE_MS): InputSender & { dispose(): void } {
  /** The last button values sent per pad; every one is a value, triggers are analog. */
  const pads = new Map<number, number[]>();
  /** The latest state of what was last sent as a datagram, until it has been repeated reliably. */
  const settling = new Map<string, { msg: InputMsg; timer: ReturnType<typeof setTimeout> }>();
  let pendingMotion: InputMsg | undefined;
  let scheduledReady: Promise<void> | undefined;
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
        if (key === "ma" && pendingMotion?.t === "ma") pendingMotion = undefined;
        wire.reliable(msg);
      }, settleMs),
    });
  };
  const settleAbsolute = (msg: InputMsg) => settleLater("ma", { t: "ma", x: msg.x, y: msg.y });
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

  function send(msg: InputMsg): void {
    // A stale capture handler must not reach a connection that is gone or replaced.
    if (disposed) return;
    if (msg.t === "mm" || msg.t === "ma") {
      if (msg.t === "mm" && pendingMotion?.t === "ma") drop("ma");
      pendingMotion = undefined;
      const writer = wire.datagramState?.();
      if (writer && full(writer.desiredSize)) {
        pendingMotion = { ...msg }; // capture mutates and reuses its motion objects
        if (msg.t === "ma") settleAbsolute(msg);
        const ready = writer.ready;
        // One callback per readiness promise, including when a closed writer reports zero.
        if (scheduledReady !== ready) {
          scheduledReady = ready;
          void ready.then(() => {
            if (pendingMotion && wire.datagramState?.() === writer) send(pendingMotion);
          }, () => {
            pendingMotion = undefined;
          });
        }
        return;
      }
    }
    if (lossy(msg)) {
      const json = JSON.stringify(msg);
      const { read, written } = encoder.encodeInto(json, scratch.subarray(1));
      // read < json.length: it overran the scratch; the wire decides the real limit.
      if (read === json.length && wire.datagram(scratch.slice(0, 1 + written))) {
        if (msg.t === "ma") settleAbsolute(msg);
        else if (msg.t === "gp") settleLater(padKey(msg), JSON.parse(json) as InputMsg);
        return; // mm and ms are relative: nothing to settle
      }
    }
    // A click must land where the pointer was, and a datagram `ma` can lose or trail the reliable
    // `mb`: the position goes first on the same ordered stream. Relative motion has no position to
    // repeat, so a late `mm` just shifts the cursor slightly; accepted.
    if (msg.t === "mb") {
      if (pendingMotion?.t === "ma") pendingMotion = undefined;
      const abs = drop("ma");
      if (abs) wire.reliable(abs);
    } else if (msg.t === "ma") drop("ma");
    else if (msg.t === "gp") drop(padKey(msg)); // this reliable state is the newest
    wire.reliable(msg);
  }

  return {
    send,
    get backpressured() {
      return !disposed && (full(wire.datagramState?.()?.desiredSize) || full(wire.controlDesiredSize?.()));
    },
    // desiredSize is writer capacity and can count chunks. Capture's byte metric requires bytes.
    bufferedAmount: 0,
    /** Forgets what was waiting to settle: the connection is going, and the host releases what it held. */
    dispose() {
      disposed = true;
      pendingMotion = undefined;
      for (const key of [...settling.keys()]) drop(key);
    },
  };
}
