---
status: accepted
date: 2026-11-06
---
# WebTransport is transport #2, and media may connect to the agent

Quasar adds **WebTransport** as the second implementation of the transport slot
(`GLOSSARY.md`: transport), beside WebRTC. The browser opens a WebTransport session to the node
agent, directly or through the operator's edge proxy, and media and input travel as QUIC datagrams
in a framing the contract defines (`protocol/transport-webtransport.md`, amendment 54, spec #1024).
Standard codecs decode with WebCodecs and a presenter Quasar owns draws every frame. We decided
this, and the reword of `CLAUDE.md` invariant 6 that goes with it, with the owner on 2026-11-06.

## Why a second transport

WebRTC puts the browser's jitter buffer, congestion control and decode path between the encoder and
the screen. libwebrtc's playout floor (about 30 ms, measured before the public move) cannot be tuned
from JavaScript, its congestion response is whatever `rtpgccbwe` decides, receiver-side FEC on RTP
is a dead end, and the `<video>` element hides decode and presentation. The 0.6 headline codec,
PyroWave (ADR 0011), has no WebRTC payload at all. Invariant 3 anticipated this: the transport is an
interface, `webrtcbin` was implementation #1. The architecture doc's rule against switching the
transport framework without numbers is met by keeping WebRTC the default: WebTransport ships opt-in
and experimental, and its gate is parity with WebRTC on median glass-to-glass latency, present
sigma p95 and freezes, on a clean link and every netem shape.

## The invariant 6 reword

Invariant 6 said the node agent is never contacted directly by a client. That is no longer true of
**media**, and it was already only half true: WebRTC media flows browser to agent over ICE today.
The invariant now says: **the API and all control go to the control plane and never to the agent;
the media transport may connect to the agent, or an edge in front of it, with a short-lived token
the control plane mints.** The control plane still decides everything: whether a session uses
WebTransport at all (the stream plan), which host and endpoint, and who may connect. The agent
authorizes nothing on its own; it redeems a single-use transport token it was handed as a hash.
Admin and role enforcement are untouched.

## What the decision commits us to

- **WebRTC stays the default and the guaranteed floor.** Every launch response still carries
  signaling coordinates, and any failure to connect, negotiate or keep the stream alive falls back
  to WebRTC (*transport fallback*). A host without a working endpoint is never offered
  WebTransport; it is off unless an admin turns it on per profile.
- **Transport is chosen once, at launch, by the stream plan**, after the codec, in the pure
  planning function. The compositor, encoder and ABR ladder are identical on both transports, so a
  fallback never re-encodes. The one exception is PyroWave, which is WebTransport-only.
- **Two token kinds that never cross**: `signaling` redeems at the control plane's signal
  endpoint, `transport` at the agent. A reconnect or takeover mints a fresh transport token.
- **One QUIC listener per agent, one UDP port to open per host.** In direct mode the agent
  self-signs a short-lived ECDSA P-256 certificate and the control plane passes the browser its
  hash; in edge mode the browser trusts the proxy's public certificate and gets no hash. The
  endpoint URL is opaque to the browser.
- **Browser reach is narrow on purpose.** Chrome on Windows, macOS and Linux must pass; every
  other browser falls back to WebRTC.

## Considered options

- **Relay media through the control plane.** Keeps invariant 6 as written, but puts every session's
  media through a service that holds no GPU state, adds a hop to the latency path, and makes the
  control plane a bandwidth bottleneck. Rejected.
- **A Quasar-run QUIC relay now.** The WebTransport counterpart of TURN, for peers the edge proxy
  cannot reach. Because the URL is opaque and the session is named by the token, it slots in later
  without a contract change. It ships in 0.7 with RH-08, not 0.6.
- **Keep WebRTC only and tune it.** Cannot remove the playout floor, cannot carry PyroWave.

Browser and proxy facts behind this (as of 2026-11): WebTransport is in Chrome and Safari 26.4,
iOS Safari reportedly lacks datagrams, `serverCertificateHashes` needs certificates valid 14 days
or less, and Caddy's WebTransport passthrough is experimental.
