/**
 * Mic m-line selection (spec §3.4) — the rule that decides which transceiver
 * answers the host's microphone offer.
 *
 * Getting this wrong is unrecoverable at runtime: the host caps each PC at one
 * offer, so answering the wrong m-line (or none) loses the mic for the whole
 * session with no renegotiation available. Hence the SDP-driven rule is tested
 * as a pure function rather than relying on a browser behaviour.
 */

import { describe, expect, it } from "vitest";
import { micMidFromOffer, selectMicTransceiver } from "./session";

/** A two-m-line audio offer: host→client audio (sendonly) + mic (recvonly). */
const OFFER_WITH_MIC = [
  "v=0",
  "o=- 0 0 IN IP4 0.0.0.0",
  "s=-",
  "t=0 0",
  "a=group:BUNDLE 0 1",
  "m=audio 9 UDP/TLS/RTP/SAVPF 111",
  "c=IN IP4 0.0.0.0",
  "a=mid:0",
  "a=sendonly",
  "a=rtpmap:111 opus/48000/2",
  "m=audio 9 UDP/TLS/RTP/SAVPF 111",
  "c=IN IP4 0.0.0.0",
  "a=mid:1",
  "a=recvonly",
  "a=rtpmap:111 opus/48000/2",
].join("\r\n");

/** Pre-amendment / mic-not-granted: a single sendonly audio m-line. */
const OFFER_WITHOUT_MIC = [
  "v=0",
  "o=- 0 0 IN IP4 0.0.0.0",
  "s=-",
  "t=0 0",
  "m=audio 9 UDP/TLS/RTP/SAVPF 111",
  "a=mid:0",
  "a=sendonly",
].join("\r\n");

describe("micMidFromOffer", () => {
  it("returns the mid of the recvonly audio m-section", () => {
    expect(micMidFromOffer(OFFER_WITH_MIC)).toBe("1");
  });

  it("returns null when no audio m-section is recvonly", () => {
    expect(micMidFromOffer(OFFER_WITHOUT_MIC)).toBeNull();
  });

  it("ignores a recvonly VIDEO m-section", () => {
    const sdp = ["v=0", "m=video 9 UDP/TLS/RTP/SAVPF 96", "a=mid:0", "a=recvonly"].join("\r\n");
    expect(micMidFromOffer(sdp)).toBeNull();
  });

  it("does not treat a sendrecv (default) audio section as the mic slot", () => {
    const sdp = ["v=0", "m=audio 9 UDP/TLS/RTP/SAVPF 111", "a=mid:0"].join("\r\n");
    expect(micMidFromOffer(sdp)).toBeNull();
  });

  it("handles bare-LF SDP as well as CRLF", () => {
    expect(micMidFromOffer(OFFER_WITH_MIC.replaceAll("\r\n", "\n"))).toBe("1");
  });

  it("handles a non-numeric mid", () => {
    const sdp = ["v=0", "m=audio 9 UDP/TLS/RTP/SAVPF 111", "a=mid:mic", "a=recvonly"].join("\r\n");
    expect(micMidFromOffer(sdp)).toBe("mic");
  });
});

describe("selectMicTransceiver", () => {
  const transceivers = [{ mid: "0" }, { mid: "1" }];

  it("picks the transceiver whose mid matches the recvonly audio m-line", () => {
    expect(selectMicTransceiver(OFFER_WITH_MIC, transceivers)).toBe(transceivers[1]);
  });

  it("returns null when the offer carries no mic m-line", () => {
    expect(selectMicTransceiver(OFFER_WITHOUT_MIC, transceivers)).toBeNull();
  });

  it("returns null when no transceiver carries the mic mid (mids not yet assigned)", () => {
    expect(selectMicTransceiver(OFFER_WITH_MIC, [{ mid: null }, { mid: null }])).toBeNull();
  });
});
