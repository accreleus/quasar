# #128 live gate — a session survives a control-plane restart

**Date:** 2026-09-08 · **Host role:** aux-infra (self-contained stack: its own control
plane, node agent and Postgres) · **Build:** control plane `quasar-control-plane`
built from `adb3b73`, schema 77, image contract passed. Node agent unchanged from the
phase-1/2 build (this phase is web-only; the SPA is baked into the control-plane image).

## What was being tested

The 2026-09-08 05:15 run of this same gate **failed**: phases 1 (control plane holds
and reconciles) and 2 (agent holds for a grace window) both worked, and then the
browser ended the session itself. Its signalling socket had closed, it treated that as
a session failure, minted replacement coordinates, and re-seated them — which destroyed
the `RTCPeerConnection` that was still carrying media. The agent saw the transport
disappear and stopped the session.

```
05:15:43  control plane stopped
05:16:55  control plane started                          (72 s outage)
05:16:56  agent reconnected, token=session-grace-cleared        OK
05:17:04  agent: "peer transport gone"                          FAIL   (outage +81 s)
05:17:14  session ended, "peer disconnected: data channel closed"
```

Phase 4 separates signalling health from media health, so a signalling close
re-attaches the socket in place instead of rebuilding the transport.

## Method

1. Launched a real Chrome peer session through the harness: profile `1080p60`,
   h264 constrained-baseline, 1920x1080@60, 8000 kbps.
2. Waited for `state=running` and `state_detail="app presented"`.
3. Stopped the control-plane container, held it down, started it again.
4. Left the session running and observed the agent, the control plane, and the
   session row.

## Result — PASS

Container record, which is the authority on the outage window:

```
stopped   12:19:53
started   12:21:06          73 s outage
```

Agent:

```
12:19:53  connection lost with 1 running session(s); holding them for 90s
          while the control plane comes back        token="sessions-held-for-grace"
12:19:54  reconnecting in 1s
12:19:56  reconnecting in 2s
12:19:58  reconnecting in 4s
12:20:00  reconnecting in 5s     <- the 5 s cap while holding, doing its job
   ...    (5 s thereafter)
12:21:11  sent register / reconnected as host
12:21:11  control plane returned within the grace window; held sessions continue
                                                     token="session-grace-cleared"
12:21:11  carried 1 running session(s) across the control-plane connection
                                                     token="sessions-survived-reconnect"
```

Control plane:

```
12:21:13  signaling WS established  session_id=<sid>  host_id=<host>
```

That line is the fix. It is the browser re-attaching its signalling socket — the
control plane counted **two** `signaling WS established` for this session across the
run, the original attach and this one — and unlike every previous run it is not
followed by a teardown.

Session row, minutes after the outage:

```
state=running  state_detail="app presented"  ended_at=NULL
```

**Agent teardown lines across the entire run: 0.** No `peer transport gone`, no
`ending session cleanly`. The 05:17 failure mode does not occur.

### The browser kept decoding

The acceptance line for #128 is that the `<video>` keeps playing through the outage.
The client's own telemetry says it did. Browser-sourced `session_metrics` rows for this
session:

```
12:19:50   fps=60          <- last sample before the stop
12:19:52   fps=59.99  (agent)
   ...     no rows          <- telemetry POSTS to the control plane, which is down
12:21:06   fps=60          <- first sample after it returns
12:21:07   fps=60
12:21:08   fps=60
```

Across the whole run: **159 browser samples, fps min 58 / max 66, and zero samples at
fps=0** — 129 of those samples are after the outage.

The gap in the series is the control plane being unable to receive posts, not the video
stopping. The shape of the resumption is the evidence: the first sample on the far side
is already **60 fps**, not a ramp from zero. A destroyed-and-rebuilt peer connection
would have to renegotiate, re-establish ICE and DTLS, and refill the decoder, which
shows up as a climb. There is no climb, because it is the same peer connection that was
never torn down. Note it resumes at 12:21:06, five seconds *before* the agent finished
re-registering — the media path was never involved in the control plane's recovery at
all, which is the entire point of the change.

## What this does and does not prove

Proven: the agent holds a session across a control-plane outage and reconnects inside
its grace window; the control plane does not reap it; the browser re-attaches
signalling **without** destroying the peer connection; and the session is still
`running` afterwards. Those were the three actors #128 named, and all three now behave.

Not proven by this run: the reconnect ordering where the browser's mint succeeds
*before* the agent has re-registered (the control plane answers 503 and the client
retries; covered by unit tests, not exercised live here), and the recovery of a media
path that fails *during* an outage. The second is worth a follow-up run with the
impairment harness.

## Notes from the run, unrelated to the fix

- The stack's node agent had been reporting itself unhealthy for ~16 hours while
  working perfectly. A second, decommissioned agent container on the same host owned
  the health port, so both containers' probes were answered by the wrong one. Filed as
  #152.
- The control plane logs `dev agent reaper: cannot demote the last admin` every ~70 s
  on this stack. Cosmetic here — an artefact of a dev identity being the only admin —
  but it is a warning-level line on a loop.
