---
name: quasar-netem
description: Use when a Quasar test needs network impairment — "shape the network", "add latency/loss/a rate cap", "netem", "test ABR/congestion under bad network", "simulate WAN", or when shaping seems to break the stack itself (launches failing under a rate cap). Shapes either the impairment host's own loopback/ingress or the streaming host's egress. Pairs with quasar-session (drive the session being shaped) and quasar-host (operating the stack).
arguments:
  - name: verb
    description: "qnetem verb: apply, ingress, sender, sender-clear, clear, status, bootstrap"
    required: true
  - name: args
    description: "for apply/sender: a level name (mild | moderate | severe) or a literal netem args string; for ingress: src-ip precedes the level/args"
    required: false
---

# Quasar network impairment (qnetem)

```
.claude/skills/quasar-netem/scripts/qnetem <verb> [args]
```

Shapes **only UDP** (the WebRTC media). A flat `netem rate` on an interface
throttles the stack's own TCP — control-plane API/WS, Postgres, SSH — and launches
start failing for non-network reasons (measured, not theoretical).

## Two shaping points, addressed by role
Hosts come from `_shared/hosts.json`; this skill never names one.

| shaping point | role | what it is |
|---|---|---|
| **impairment host** | `aux-infra` | Shapes its own `lo`, or its ingress from a named source, inside a `NET_ADMIN` helper container — no sudo. Only affects traffic that flows through this host (i.e. the headless browser peer). |
| **sender host** | `gpu-test` | Shapes that host's *egress* with native `tc`. Affects **any** client, not just the browser peer — prefer it when the impairment must be what a real viewer sees. |

**Running impairment infrastructure on the `aux-infra` host is sanctioned** even
where building or quality-testing Quasar on that host is not: shaping and the
browser peer are infrastructure, not a test verdict. Anything whose *result* is a
quality judgment still belongs on `gpu-test`.

| verb | does |
|---|---|
| `apply <level\|'<args>'>` | UDP-only shaping on the impairment host's `lo` (host-local stack tests) |
| `ingress <src-ip> <level\|'<args>'>` | shape the impairment host's **ingress** UDP from `<src-ip>` via an IFB redirect (the cross-host path) |
| `sender <level\|'<args>'>` | shape the **sender** host's egress UDP |
| `sender-clear` | remove sender-side shaping only |
| `clear` | remove everything on both hosts. Always safe; run it after any failed test. |
| `status` | active qdiscs on both hosts |
| `bootstrap` | build the `quasar-netem` helper image (alpine+iproute2) if missing |

Roles are overridable per invocation: `QNETEM_IMPAIR_ROLE` · `QNETEM_SENDER_ROLE`.

## Levels
Named levels live in `_shared/hosts.json` under `netem` — pass a name or any
literal netem args string. Defaults ship as:

| level | netem args |
|---|---|
| `mild` | `delay 20ms 2ms loss 0.5% rate 5000kbit` |
| `moderate` | `delay 40ms 10ms loss 1% rate 3500kbit` |
| `severe` | `delay 60ms 15ms loss 2% rate 2400kbit` |

## Hard-won facts
- **Some appliance kernels ship no `sch_netem`** — not even as a module. On such a
  host, sender-side shaping only works if the modules were built and get reloaded
  at boot; otherwise impairment can only be injected at the impairment host
  (`ingress`). Check that host's operator notes: `qhost notes --host gpu-test`.
- Apply impairment **mid-session** (after the browser connects): a hard cap before
  connect floods the cold-start keyframe and nothing ever decodes.
- Jitter beyond ~10% of the base delay makes GCC's delay-gradient detector fire on
  netem's *random* jitter — that tests the estimator, not what you meant to test.
- GCC sawtooths (AIMD) against a brick-wall cap — bounded oscillation between the
  governor floor and the cap is normal physics, not a regression.
- If the impairment host's LAN link is wireless, a "clean" cross-host baseline
  already carries real jitter. Its notes will say so.
