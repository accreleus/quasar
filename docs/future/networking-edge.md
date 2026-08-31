# Networking edge — internet-facing media relay (STUB / deferred design)

**Status:** stub design, not scheduled. Captured for when we take Quasar past the LAN.
**Deferred out of:** Phase 3 (multi-host scheduling stays LAN). The architecture doc lists
"TURN/relay scaled as a separate component" under Phase 3; we split it into its own
networking phase so Phase 3 can prove multi-host *scheduling* without the internet-facing
networking sub-track.
**Tier when scheduled:** Opus (transport/latency path + security boundary; frozen-contract
amendment).

> This is a design sketch, not a ticket set. It records the topology and the open
> decisions so the work can be picked up cold. No implementation here.

## Problem

Once Quasar runs over the internet (not a LAN), two requirements collide:

1. **Users must never address a host.** A client hits one front door and gets a stream;
   it never knows or types which GPU host runs its app. *(Already satisfied — see below.)*
2. **GPU hosts must not be exposed to the internet.** You don't want the box running an
   untrusted game container to be inbound-reachable from the public internet.

The first is already solved by the day-one architecture. The second is the gap this design
closes.

## The two planes (why #1 is already solved)

Signaling and media route on completely separate planes:

- **Signaling plane (control).** Browser → control plane → node-agent. The browser opens
  `wss://<control-plane>/v1/signal?token=…` — the same origin as the API. The control
  plane validates the single-use session token, resolves the session's assigned host, and
  relays offer/answer/ICE to that agent over the persistent agent WebSocket. Per
  architecture invariant #1, **the node-agent has no public address and is never contacted
  directly** (`protocol/signaling.md` §P1-D). Multi-host changes only *which* agent the
  relay targets (`sess.HostID`) — the client still hits one front door.
- **Media plane (data).** After ICE establishes, H.264 video + the input DataChannel flow
  **peer-to-peer between the browser and that host's `webrtcbin`** — they do *not* traverse
  the control plane (`protocol/signaling.md:108-112`). The control plane is a signaling
  broker, **never a media proxy** (proxying game video through the API server would wreck
  latency and scaling).

So the client only ever addresses the control-plane/web origin. The host's address travels
inside SDP/ICE, discovered automatically — the user never sees it. **This is true today,
even at N>1.** What's missing is keeping that host address *private* over the internet.

## Today (LAN): direct host candidates

ICE produces **host candidates** = the node-agent's private LAN IP; the browser, on the
same network, connects straight to it. The node-agent's `webrtcbin` currently has
`stun-server` plumbable but set to `None` for production sessions (LAN-only). Fine for LAN,
unroutable from a remote browser.

## Why STUN alone is not enough

STUN (server-reflexive candidates) punches a NAT hole so the browser connects to the host's
*public* ip:port. It works but:
- it **puts the host's address in the SDP** and requires the host to accept inbound UDP —
  the GPU box is effectively on the internet (the posture we reject);
- it **fails behind symmetric NAT / CGNAT**.

## End-state: TURN relay, hosts stay private

Both the node-agent **and** the browser connect *outbound* to a **TURN relay**, which
forwards media between them. Neither endpoint is inbound-reachable.

```
            public edge (only these two face the internet)
   ┌─────────────────────┐        ┌──────────────────────┐
   │ control-plane / web │        │   TURN relay (edge)  │
   │   HTTPS + WSS       │        │  UDP/TCP/TLS 443     │
   └─────────┬───────────┘        └─────┬──────────┬─────┘
   signaling │ (token, relay)    media  │          │ media
   ┌─────────▼───────────────────────── ▼ ─┐   ┌── ▼ ─────────┐
   │  browser / native client              │   │ node-agent   │  ← private subnet,
   └───────────────────────────────────────┘   │ (webrtcbin)  │     OUTBOUND-ONLY
                                                └──────────────┘
```

- **GPU hosts live on a private subnet, outbound-only.** They dial out to (a) the
  control-plane WSS (already the node-initiated model — NAT-friendly) and (b) the TURN
  relay. Nothing inbound ever reaches them.
- **Only two services face the internet:** the control-plane/web origin and the TURN relay.
  Both hardened, both horizontally scalable.
- **The control plane mints short-lived TURN credentials per session** and hands
  `ice_servers` to both ends — to the browser in the launch/signaling response, to the
  node-agent in `session_assign`. (This is the contract amendment, below.)
- **Relay-only host concealment (optional, stronger):** set the browser's
  `iceTransportPolicy: "relay"` and don't gather host candidates on the agent, so the host's
  private IP **never appears in the SDP** at all.

## Properties worth keeping in mind

1. **TURN can't see the media.** SRTP/DTLS terminates end-to-end between the browser and
   `webrtcbin`; the relay forwards opaque encrypted packets. A compromised relay leaks no
   video — it's a dumb forwarder, not a MITM.
2. **TURN is latency-sensitive and regional.** Phase 0 found WAN is propagation-bound, so a
   single central relay adds a detour. Deploy TURN **per-region**, near the GPU hosts /
   client regions. This is *why* it's "a separate, scaled component," not a control-plane
   feature.
3. **Generalizes to native clients (Phase 5).** Same "private hosts + public relay edge"
   topology; the transport interface abstracts WebRTC-vs-custom-UDP, but the trust boundary
   is identical. The relay for an ICE-based native client can be the same TURN; a bespoke
   UDP transport would need an analogous relay.

## Contract amendments this will need (additive, Opus + sign-off)

- **`agent-api.md`** — carry `ice_servers` (URLs + ephemeral credentials) in
  `session_assign` so the agent configures `webrtcbin`'s `stun-server` / `turn-server`.
- **`control-api.md`** — the launch/signaling response returns the browser's `ice_servers`;
  define the per-session credential TTL and refresh story.
- **`schema.md`** — wherever TURN realm/secret config and per-session credential issuance
  are recorded (likely server config + minted-on-the-fly, not stored plaintext).
- **`signaling.md`** — *unchanged.* The offer/answer/ice message shapes don't change; only
  the candidates inside them do. (If an implementation thinks it needs a new signaling
  message, stop and escalate.)

## Open decisions (resolve when scheduled)

- **Relay implementation:** coturn (battle-tested, `use-auth-secret` time-limited creds) vs.
  a bespoke Go/Rust relay (tighter integration, more to own). Lean coturn first.
- **Credential minting:** control-plane mints short-lived HMAC creds (coturn REST/ephemeral
  scheme) per session — confirm TTL vs. session length and the refresh path for long
  sessions.
- **ICE policy:** `relay`-only (max host concealment, always pays the relay hop) vs.
  STUN-preferred-with-TURN-fallback (lower latency when a direct path exists, but may expose
  the host's reflexive address). Likely a per-deployment policy.
- **Regional placement / steering:** how a session is matched to the nearest relay; whether
  the scheduler's host choice and the relay choice are co-located.
- **Where it sits vs. Phase 4 (K8s):** TURN as a Deployment + the control-plane as the
  credential issuer fits the K8s packaging story — decide whether this lands before, with,
  or after K8s.

## Out of scope (here)
The multi-host scheduling itself (Phase 3). Kubernetes packaging (Phase 4). The native
client + custom UDP transport (Phase 5) — though this topology is a prerequisite for taking
either over the internet.
