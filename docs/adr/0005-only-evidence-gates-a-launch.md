---
status: accepted
date: 2026-09-18
---
# A readiness check blocks a launch only when it rests on evidence

Host readiness was advisory by contract: no failing check could affect
registration, admission or scheduling, because the checks read proxies (a file
exists, a firewall rule parses) and refusing sessions on a false negative was
judged worse than a red card. RH-02 (#210) needs a failed prerequisite to stop
the affected launches before the user meets a black screen. We decided that a
readiness check may block only when it rests on **evidence**: the result of a
host probe that exercised the real path with what a session would be given, or a
definitive local observation (container runtime unreachable, homes root
unwritable). Proxy checks stay advisory for the original reason.

The owner approved this on 2026-09-18 during RH-02 planning. It requires an
amendment to the frozen `agent-api.md` / `control-api.md` / `schema.md` wording;
until that amendment is signed off in `quasar-protocol`, no gating code lands.

## Considered options

- **Keep everything advisory.** Rejected: #210's acceptance cannot be met, and a
  host that cannot encode keeps accepting launches in a multi-host fleet.
- **Let any failing check block.** Rejected: it reintroduces the false-negative
  outage the advisory rule was written to prevent.
- **Gate on the agent only.** Rejected: placement would still choose the broken
  host. The control plane excludes blocked hosts at admission, beside the live
  free-VRAM veto and failing open the same way on a stale or absent report; the
  agent refuses only for its own safety states, which no override lifts.

## Consequences

- An indeterminate host probe neither sets nor clears a block.
- Admin overrides are per host and per check, remain visible, and lapse when the
  check next passes.
- A launch refused solely for readiness gets its own retryable refusal, distinct
  from "no host available".
