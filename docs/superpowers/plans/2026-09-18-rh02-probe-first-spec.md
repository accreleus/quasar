# RH-02 specification — probe-first host readiness

Specification for #210 and #211 under initiative #207. It builds on RH-01 as
promoted to `develop` at `b134085`. The reconciliation of existing behaviour is in
`2026-09-17-rh02-probe-first-reconciliation.md`; the gating decision is ADR 0005;
the vocabulary is in `CONTEXT.md` under "Host readiness". #210 and #211 keep their
own acceptance and stay the tracking issues; this document is what they are built
against.

## Problem Statement

Someone installing Quasar at home learns that their host cannot stream by
launching a game and watching it fail. The console has a readiness card with
about 25 checks, but the checks read proxies: a render node exists, an encoder
element is registered, a firewall rule parses. None of them composites a frame,
encodes one, starts audio or creates an input device. By contract every check is
advisory, so a host that cannot encode still accepts launches, and in a fleet the
scheduler keeps choosing it. A host whose container runtime is missing or
unreachable never registers at all, so the console shows nothing where it should
show the most basic fault. Checks carry no record of where an observation came
from or how old it is. Storage is reported as numbers nobody judges. The word
"reachability" on a host-local firewall check invites the reading that a browser
can reach the host, which the host cannot know.

## Solution

The admin sees, before anyone launches anything, whether the host can really do
the work. After the agent starts it runs host probes: short disposable jobs, each
run where the real path runs, that composite and encode a few frames, open the
GPU the way an application container will, start the audio path and create a
virtual input device. Their results
join the existing readiness checks on the existing readiness card, each with its
source and observation time. A check that rests on evidence and fails blocks the
launches it affects, and the person launching is told the host needs its admin's
attention instead of meeting a black screen. Proxy checks keep informing and
never block. A host with no usable runtime still appears in the console, says
why, and refuses launches until it has recovered. An admin can override a named
failing check for a host; the override stays visible and ends when the check
passes again. Nothing on the card claims that a browser can reach the host.

## User Stories

1. As a self-hoster, I want the console to tell me my GPU cannot encode before I
   launch a game, so that I fix the host instead of debugging a black stream.
2. As a self-hoster, I want each failing readiness check to give me the exact fix,
   so that I do not have to search logs.
3. As a self-hoster, I want a host whose container runtime is missing or
   unreachable to appear in the console with that fault named, so that I know the
   install got as far as the agent.
4. As a self-hoster, I want a host that has not finished its startup cleanup to
   refuse launches, so that my game homes are never mounted twice.
5. As a self-hoster, I want to see when each check was last observed, so that I
   can tell a current result from an old one.
6. As a self-hoster, I want to see where a check's observation came from (a host
   probe, a local read, the container runtime, my own configuration), so that I
   know how much to trust it.
7. As a self-hoster, I want a missing virtual input device reported before launch,
   so that I do not start a game I cannot control.
8. As a self-hoster, I want a failing audio path reported before launch, so that I
   do not discover a silent game mid-session.
9. As a self-hoster, I want an unwritable homes root reported before launch, so
   that a game does not lose its saves.
10. As a self-hoster, I want a warning when homes storage is running low and a
    block only when it is exhausted, so that a nearly full disk does not take my
    host offline.
11. As a self-hoster, I want missing NVIDIA drivers reported, not silently
    installed or worked around, so that I stay in control of my host.
12. As a self-hoster with an NVIDIA card, I want the existing driver-volume path to
    keep working exactly as before, so that RH-02 does not break a working host.
13. As a self-hoster, I want to see whether my container runtime has CDI enabled
    and which devices it discovered, so that I understand how my GPU is exposed.
14. As a self-hoster, I want the network check worded as what my host's firewall
    allows, so that I do not mistake it for proof my browser can connect.
15. As an admin, I want a failing evidence-based check to block only the launches
    it affects, so that one bad GPU does not stop sessions on another.
16. As an admin of several hosts, I want placement to skip a host that is not
    ready and use one that is, so that users are not sent to a broken machine.
17. As an admin, I want proxy checks never to block a launch, so that a false
    negative cannot cause an outage.
18. As an admin, I want to override one named failing check on one host, so that I
    can keep running when I know the check is wrong for my setup.
19. As an admin, I want an overridden check to stay visible with its failure and an
    override marker, so that nobody forgets it is there.
20. As an admin, I want an override to lapse when its check passes again, so that
    it cannot hide a later regression.
21. As an admin, I want overrides written to the audit log, so that I can see who
    decided to launch despite a failure.
22. As an admin, I want the agent's own safety refusals to be beyond any override,
    so that no console action can endanger user data.
23. As an admin, I want an inconclusive host probe shown as indeterminate with its
    reason, so that I do not read a timeout as a broken GPU.
24. As an admin, I want an indeterminate probe to leave an existing block or pass
    as it was, so that a flaky runtime neither blocks nor unblocks my host.
25. As an admin, I want host probes never to run on a GPU with a live session, so
    that checking the host never degrades someone's game.
26. As an admin, I want host probes to run again when the runtime image, the
    driver, the GPU set or relevant host settings change, so that results follow
    the host.
27. As an admin, I want a probe to run after a launch fails in a way a probe could
    explain, so that the card catches up with reality.
28. As an admin, I want the setup wizard's host step to show the same readiness
    card, so that first-run and day-two use one vocabulary.
29. As an admin, I want the release preflight to keep reading the same stored
    readiness, so that the Releases and Hosts tabs never disagree.
30. As a user, I want a launch refused for readiness to tell me the host needs its
    admin's attention, so that I do not retry pointlessly or blame my browser.
31. As a user, I want a launch to succeed on another host when mine is not ready,
    so that I can still play.
32. As an operator, I want every host-probe container to be verifiably Quasar-owned
    and removed after its result is captured, so that probes never litter my host
    or touch containers that are not Quasar's.
33. As an operator, I want an interrupted probe reconciled under its original
    operation identity before any new probe runs, so that a lost reply cannot leave
    two probes or an orphan.
34. As an operator, I want an agent restart to finish a previous probe's cleanup,
    so that cleanup survives crashes.
35. As an operator, I want stopping the observation of a probe not to be treated
    as rolling it back, so that the runtime contract holds for probes as for
    sessions.
36. As an operator, I want no engine CLI fallback for probes, so that container
    ownership stays with one interface.
37. As an operator, I want the media probe to be a separate bounded process with
    one stable meaning, so that the later split into a light host agent and media
    workers moves it into the worker without a readiness redesign.
38. As an external Intel tester, I want the readiness card to report what my
    hardware exposes without Quasar claiming it certified, so that my results are
    read honestly.
39. As a maintainer, I want one acceptance harness that injects each prerequisite
    fault on disposable fixtures, so that "appears before launch" is evidence.
40. As a maintainer, I want fresh-install evidence recorded per host class with
    exact images and commits, so that support claims match what was run.

## Implementation Decisions

**Vocabulary.** Host fact, readiness check, host probe, evidence, indeterminate,
readiness override and diagnostic registration are defined in the glossary. The
client measurement formerly called "probe" is a device probe. "Preflight" stays
the release evaluation.

**The readiness check remains the single reported unit.** No second vocabulary
and no new table. Host facts ride on the check they support. The check gains
three optional fields: when it was observed, its source (`host_probe`, `local`,
`runtime`, `operator`), and the workload scope it blocks when failing (absent for
every proxy check). Check ids and statuses stay open strings. `unknown` joins the
status vocabulary for indeterminate results; `skip` keeps meaning "not
applicable". The control plane keeps storing the report verbatim. Durable,
versioned facts and desired/applied generations stay with #216.

**Only evidence gates (ADR 0005).** Blocking scopes:

| Failed evidence | Blocks |
| --- | --- |
| Container runtime unreachable, or startup cleanup not yet succeeded | every launch on the host (agent-enforced, not overridable) |
| Homes root not writable as the app identity, or homes storage exhausted | every launch that mounts a home |
| Media or application-GPU host probe fails on a GPU | launches placed on that GPU |
| Input host probe fails | every launch on the host |
| Audio host probe fails | every launch on the host |

**Enforcement.** Control-plane admission excludes a host or GPU that readiness
blocks for the requested workload. The verdict is computed once per readiness
report (and once per override change) by a pure Go function over the report and
the host's overrides, and stored as derived blocked scopes for the host and its
GPUs. Admission reads those derived values through the one filter renderer its
candidate and recheck queries already share, so the two cannot disagree, and it
ignores them when the report is stale or absent, failing open as the live
free-VRAM veto does. The filter is not part of the totals probe: as with the
free-VRAM veto, a readiness-only rejection is diagnosed by a second query (the
candidate query with only the readiness filter removed). When that finds a GPU,
readiness is the sole reason and the launch is refused with a new retryable
`503 host_not_ready`; otherwise the existing refusals stand. The agent does not
evaluate the same checks a second time. It refuses launches only for its own
safety states.

**Readiness override.** Per host and per check id, in its own table (the next
migration after 0084; the host-settings knob catalog has no map type and is not
stretched to hold one). Admin-only through the existing middleware, audited,
always rendered with the failing check, and deleted by the control plane when a
later report shows that check passing. Check ids are agent-owned and may be
renamed; an override for an id the host no longer reports is inert and is shown
as such, so a rename re-blocks until the admin decides again. That is the safe
direction and is accepted.

**Host probes.** A host probe runs where the path it proves runs. Today the
compositor, encoder and virtual input live in the agent's own container and only
the application and the audio sidecar are sibling containers; #212/#213 move the
media path into worker containers later. Four probes:

- **Media** — composites and encodes a few frames per GPU, using the production
  GPU binding and effective-encoder resolution (an encode-only headless path
  already exists and is extended with the compositor source). It runs as a
  bounded child process of the agent from the agent's own binary, as the EGL
  self-test does, so a driver crash cannot take the agent down and the result
  describes the container sessions really use. When workers exist the same
  subcommand runs in the worker container through the runtime interface; the
  check id and meaning do not change. (The owner approved this placement on
  2026-09-18, revising the earlier "sibling container" answer after review
  showed where the media path really runs.)
- **Application GPU access** — a disposable sibling container through the runtime
  interface, given GPU access by the same code that prepares a session's
  application container, running the existing EGL self-test. It generalises
  today's NVIDIA-only sibling EGL test to every vendor. The runtime interface's
  NVIDIA GPU diagnostic profile is widened into one closed GPU probe profile
  (vendor device access, groups, driver volume, no network); the locked-down
  general profile is unchanged and no open-ended request type is added. The
  session application request type is not reused: its naming and home
  bookkeeping belong to sessions.
- **Audio** — the existing audio sidecar profile started under a probe identity:
  the sidecar starts and its socket appears, then it is stopped and removed.
- **Input** — the existing virtual-input self-test run as a bounded child
  process, because the agent itself opens the input device and publishes the
  nodes into its own namespace; a sibling container would prove nothing.

The agent binary rejects an unknown subcommand with an error instead of starting
as an agent, and a container probe always uses the running agent's own image
identity, so a probe can never boot a second agent. Probe container names get
their own owned prefix, added to ownership verification and startup recovery.

**Probe lifecycle.** A container probe is a journaled helper operation with a
stable operation identity, verified ownership and tracked cleanup. One probe
runs at a time per host, and the audio probe's recovery of stale sidecars is
part of that single flight. The agent's probe orchestrator owns the deadline:
the command is bounded inside the container as today, and past the deadline the
orchestrator issues an explicit stop and then cleanup. A timeout while
*observing* is not the container's outcome and is never read as one. A stop or
cleanup whose outcome is unknown is reconciled under the same identity, and no
new probe of that kind starts while an earlier one is unreconciled. Dropping
observation never stops, removes or rolls back anything. Startup recovery
finishes interrupted probe cleanup. There is no engine CLI fallback. A child
process probe is killed at its deadline and leaves nothing behind. Any
inconclusive outcome reports `unknown` with a reason and leaves the last
definitive result in force.

**Probe results survive the periodic report.** The 15-second local refresh
merges into the last probe-derived and safety checks; it no longer replaces the
whole report. If the refresh itself fails, earlier blocking checks are kept, so
a refresh error can never unblock a host.

**When probes run.** After agent start, outside the registration handshake
window, reporting on a later capacity message. Again when a probe input changes
(agent image identity, driver version or driver-volume identity, GPU device set,
relevant host settings) and after a launch failure a probe could explain. A
media probe takes the same local encode reservation a session takes, so it
cannot overlap a session on that GPU; a launch that arrives during a probe
pre-empts it, and the pre-empted probe is indeterminate. No timer. No admin
re-check action in RH-02; an agent restart re-runs probes.

**Diagnostic registration.** A failed startup cleanup or an unusable runtime no
longer ends the process. The agent enters a diagnostic mode that withholds
everything the exit used to prevent: homes garbage collection, the driver-volume
and CUDA provisioners, image pulls and pruning, and host probes. Its health
endpoint reports not ready. It registers, reports the runtime fact as a blocking
check, refuses every launch itself, and retries the cleanup under the original
operation identities. When the cleanup succeeds it resumes normal startup. The
protection of managed homes is unchanged.

**Storage.** Two new checks computed on the agent: homes root writable as the app
identity (a write test, blocking), and homes free space (`warn` under a
configurable floor defaulting to 5 GiB, `fail` and blocking only when
exhausted). Template and image storage warn only.

**Runtime and CDI facts.** New checks report the runtime endpoint's reachability,
negotiated API version and the capabilities RH-01 already discovers, and whether
CDI is enabled with which devices the engine discovered. CDI is observed only.
GPU injection keeps the NVIDIA device request plus the Quasar driver volume, with
host-injected drivers taking precedence. Missing drivers are reported; nothing is
installed or bypassed beyond the existing driver-volume provisioner.

**Host readiness is not browser reachability.** The firewall check keeps its id
and is reworded as the host's inbound firewall posture. The card states that
readiness is host-local. Browser connectivity diagnostics remain #223.

**Console.** The existing readiness card is extended in its current idioms:
observation time and source per check, a "blocks launches" marker, the override
control and marker, and groups for runtime, storage and audio. No design mock
covers readiness; the owner approved extending the card without a restyle, with a
design pass possible later. The setup wizard and Fleet surfaces inherit the
change through the shared card. The user-facing launch error gets wording for
`host_not_ready`.

**Contract amendment.** Drafted as amendment 11 (`quasar-protocol` PR 22, awaiting
the owner's sign-off; the contract text wins where it is more precise than this
section — it also settles that swap is not gated, that freshness is the report's
age, and that the host body's verdict field is `readiness_gate.blocking`). One
`quasar-protocol` amendment, requiring Opus review
and the owner's explicit sign-off before any gating code lands: reword the
advisory clauses to the evidence rule in all three contracts; add the three
optional check fields to the documented and OpenAPI check shape; regularise the
status vocabulary to what agents already send (`warn`, `provisioning`) plus
`unknown`, noting that release preflight already treats an unrecognised status
as unknown and never blocks on it; add `host_not_ready` to the enumerated
errors; add the admin-gated override endpoints and table. Facts, host probes,
storage checks, CDI and diagnostic registration need no amendment and do not
wait for it.

**Fact and policy separation in existing code.** Where RH-02 touches a check that
mixes the two, the observation is separated from the verdict. It does not
refactor the rest: encoder default selection, the driver-volume provisioning
trigger and the boot gate keep their behaviour.

## Testing Decisions

A good test here states a prerequisite fault or a runtime event and asserts what
an operator, a launching user or the runtime would observe. It does not assert
which function produced it. Boundaries, confirmed by the owner, all existing:

1. **Agent checks** — the fake-root boundary the readiness checks already use: a
   pure function of the probe environment. New facts, storage, CDI, runtime checks
   and the mapping from a host-probe outcome to a check are tested here.
2. **Container-probe lifecycle** — the scripted Docker Engine API double used by the
   helper tests: deadline then stop then cleanup, lost create/start/stop/remove
   replies, dropped observation, journal recovery after restart, ownership
   refusal, single-flight. The existing ignored real-Docker tests gain one case
   for the probe profile.
3. **Gate decision** — a pure Go function tested like the stream plan and release
   plan, plus one DB-backed admission test showing a blocked host skipped, another
   host chosen, `host_not_ready` when none remains, and candidate and recheck
   agreeing. Override lapse gets a DB-backed test. The OpenAPI drift test covers
   only the route surface, so the new error code and check fields are asserted
   by handler tests.
4. **Console** — the readiness card's component tests and the group-membership pin
   test, which lists every check id and must gain each new one.
5. **End to end** — one new acceptance harness that injects each fault on
   disposable fixtures (runtime stopped, homes root read-only, input device
   withheld, GPU withheld from the probe) and asserts the check, the block and the
   refusal through the control-plane API.

TDD applies to every slice: the failing behavioural test first, at these
boundaries. Hardware acceptance begins with AGENTS.md's shared-host version
preflight: record deployed component identities, schema, stack identity and
active sessions, and recheck before any mutation.

## Out of Scope

Worker extraction (#212/#213), running-session adoption (#214), pinned session
versions (#215), durable versioned host facts and desired state (#216/#217),
updater redesign (#218/#219), Podman and rootless certification (#220/#221), TURN
and browser connectivity diagnostics (#222/#223). Using CDI for injection. An
admin re-check action. A readiness redesign. Intel certification: Intel paths are
preserved and reported, validated only by external testers under the existing
procedure, and no Intel coverage is claimed.

## Further Notes

**Evidence plan for #211.** The owner made these hosts available: `gpu-test`
(NVIDIA, a Linux VM) and the maintainer workstation (AMD, standard Linux), plus a
fresh deployment on the Unraid appliance, which has an AMD card and also hosts
the `gpu-test` VM. That gives NVIDIA on standard Linux, AMD on standard Linux and
AMD on Unraid. A native Unraid/NVIDIA install is not available, because the
NVIDIA card is passed through to the VM. #211's "fresh Unraid/NVIDIA" line is
therefore met only in part, and the record must say so. The Unraid run is best
effort and does not block the increment; any switch of the appliance between its
own stack and the VM is asked of the owner first. Outside testers may be brought
in later.

**Delivery.** Work branches from and integrates only into
`initiative/resilient-host-architecture`. Promotion to `develop` needs the owner's
separate approval after acceptance. No release tag, image publication or
deployed-stack change is part of this specification.

## Implementation sequence

Published 2026-09-18 as #252 (this specification) and thirteen slices, native
sub-issues of #210/#211 with blocking edges:

| Slice | Issue | Blocked by |
| --- | --- | --- |
| Report merge (first) | #255 | — |
| Storage checks | #253 | — |
| Runtime and CDI checks, host-local wording | #254 | — |
| GPU probe profile in the runtime interface | #258 | — |
| Protocol amendment (owner sign-off) | #260 | — |
| Diagnostic registration | #256 | #254, #255 |
| Input and media host probes | #257 | #255 |
| Application-GPU and audio host probes | #259 | #255, #258 |
| Provenance and freshness on the card | #261 | #260 |
| Admission gate and `host_not_ready` | #262 | #257, #259, #260, #261 |
| Readiness override | #263 | #262 |
| Fault-injection acceptance harness | #264 | #256, #262 |
| Hardware and fresh-install evidence | #265 | #263, #264 |

## Agent coordination for implementation

The primary agent (Fable) owns coordination, test design, review of every diff,
integration and publication. It delegates bounded work to cheaper models and
escalates per `CLAUDE.md` "Model tiering". Workers never commit, push, merge or
touch the tracker; each gets one deliverable and explicit file ownership, and no
two workers edit the same files at once.

| Slice | Implementer | Why |
| --- | --- | --- |
| #255 report merge | Sonnet | small, pure, clear spec |
| #253 storage checks | Sonnet (web group: Haiku) | routine check plus a pinned list |
| #254 runtime/CDI checks, wording | Sonnet (wording and web group: Haiku) | reads existing discovery |
| #258 GPU probe profile | Opus | ownership, journals, uncertain outcomes |
| #256 diagnostic registration | Opus | startup safety, home protection |
| #257 input and media probes | Opus for the media path, Sonnet for scheduling and mapping | GStreamer pipeline, crash isolation |
| #259 app-GPU and audio probes | Sonnet, Opus review of lifecycle | built on #258 |
| #260 amendment | Opus drafts, owner signs | frozen contract |
| #261 card provenance | Sonnet (card tests: Haiku) | pass-through plus rendering |
| #262 admission gate | Opus | scheduling, concurrency, migration |
| #263 override | Sonnet, Opus review of authorization | server-enforced admin surface |
| #264 harness | Sonnet | shell harness on existing conventions |
| #265 evidence | primary agent | live hosts, judgment, owner contact |

A slice escalates to Opus when its ticket is ambiguous, when it touches a frozen
interface, security or concurrency, or when a cheaper model has failed it twice.
The model actually used is reported; an unavailable model is reported, never
silently substituted.
