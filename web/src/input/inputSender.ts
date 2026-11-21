// The seam input capture sends through: one interface, one implementation per transport.
// The DataChannel's is here; WebTransport's is webtransport/inputSender.ts. The message
// is the protocol/input.md JSON either way.

export type InputMsg = Record<string, unknown>;

export interface InputSender {
  send(msg: InputMsg): void;
  /** Bytes queued and not yet on the wire, if the transport exposes a byte count. */
  readonly bufferedAmount: number;
  /** For transports that expose writer capacity independently of queued bytes. */
  readonly backpressured?: boolean;
}

export function dataChannelSender(ch: RTCDataChannel): InputSender {
  return {
    send: (msg) => {
      if (ch.readyState === "open") ch.send(JSON.stringify(msg));
    },
    get bufferedAmount() {
      return ch.bufferedAmount;
    },
  };
}
