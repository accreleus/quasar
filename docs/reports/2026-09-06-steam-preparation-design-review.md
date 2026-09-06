# Steam preparation: implementation prerequisites for #145

Status: implementation authorized. On 2026-09-06 the operator explicitly approved
the behaviors and proposed additive contract changes below, then explicitly waived
the separate Opus review requirement for this issue and instructed implementation.
The investigation below records the baseline; implementation and validation are
being completed separately. Issue #145 remains open until release-live acceptance.

## Existing implementation and gaps

- `control-plane/internal/images/ensure.go` enqueues `template.warmup` on ready
  reports, reading the adopted image reference and version. Its manual-job path
  prefers managed-home images but permits other images. Neither path establishes
  explicit Steam-only preparation eligibility.
- `node-agent/src/session/warmup/job.rs` accepts image ID, registry reference and
  version. It checks configuration, storage and host availability before invoking
  the existing serialized runner. `warmup/mod.rs` already compares reference and
  version for freshness and implements presentation, settling and quiescence.
- `node-agent/src/agent.rs` constructs the runner and its environment-derived
  configuration when the agent connects. It separately creates `template_store`
  only when `QUASAR_HOME_TEMPLATES` is enabled. Enabling a control-plane job alone
  cannot enable consumption or cancel work when a source policy changes.
- `session/home.rs` already seeds only absent/empty managed homes and fails open
  on seeding errors. Eligibility must be checked before resolving its template;
  otherwise an unsupported image with an existing template could still consume it.
- `session/template.rs` already chooses reflink or full copy from a probe on the
  actual template/home paths. The mode is cached when the store is resolved;
  changing roots must replace that store and re-probe. The fallback reason is
  logged, not a durable per-image UI preparation report.
- `web/src/pages/admin/library/SourcesTab.tsx` and `useSourcesData.ts` provide the
  Steam source surface and shared settings/resource mechanism. `SourceRow.children`
  can contain the separately labelled preparation switch without changing the
  source/discovery toggle. Existing image readiness and generic job history must
  not be relabelled as successful preparation.

## Contract decisions required before implementation

1. **Persisted source policy.** Add a Steam-specific preparation boolean, default
   true, to the existing administrator settings read/patch contract and persisted
   settings. This is independent of `library_discovery_enabled`, provider
   enablement, and app launch permission. No existing setting expresses it.
   A proposed field is `steam_preparation_enabled`; its name and semantics need
   approval before editing the frozen settings schema/API.
2. **Deployment policy delivery.** Deliver that source policy and its revision to
   agents on registration, reconnect and each change. Do not put it into the
   existing `config_update.settings` map as an implicit host override:
   `protocol/agent-api.md` explicitly defines that map as *only* administrator
   host overrides, layered over the agent environment. An additive distinct
   policy field/message or separately approved agent HTTP policy endpoint is
   required. Define older-agent behavior and acknowledgement/effective reporting.
3. **Effective status.** Expose per-host/per-image eligibility, desired/effective
   preparation and consumption, override reasons, preparation state, template
   reference/version and known clone mode/reason. Existing arbitrary job results
   can carry preparation outcomes, but no job result exists for an unsupported
   image, disabled consumption or a policy change before work begins. Decide the
   existing image-detail/API extension or approved status projection before
   implementing a UI that promises the complete state set.

These are semantic contract decisions even if an implementation could technically
fit values into an existing generic JSON map. Generic serialization does not grant
permission to change the documented meaning of the map.

## Recommended behavior after approval

- Establish one explicit initial Steam eligibility policy tied to the validated
  adopted Steam image/provider identity. HOME, WorkingDir and managed-home
  metadata alone never opt an image in. Enforce the same decision during enqueue,
  manual execution, reconciliation and consumption; verify stale queued work
  against current identity and policy before running or publishing it.
- Resolve production and consumption separately as eligibility AND source policy
  AND their existing host-specific permission. Absent host flags acquire the
  enabled default; explicit false remains false. Turning the source switch off
  wins over either host permission and never affects discovery or normal launches.
- On disable, stop accepting preparation/seeding and use `WarmupControl` to abort
  only the active preparation. Preserve active user sessions, existing homes and
  published templates. Make the worker consult the current policy/revision before
  publication so an old queued/running job cannot resurrect a disabled policy.
- Reconcile already-adopted eligible images on enable, upgrade to the new default
  and host reconnect, in addition to the existing ready/adoption events. Reuse
  existing deduplication, freshness checks, host priority, serialization, free-space
  limits and sanitization. Re-enabling reuses a matching template.
- Keep clone mode auto and report full-copy fallback honestly. Re-resolve stores
  on relevant root/mount changes. Never move user data or rewrite host paths.
- Present the requested Steam-only switch and existing source controls separately.
  Show preparation status in image details, including Unsupported versus Disabled;
  use effective host status rather than claiming a saved desired value is active.

## Required evidence

Beyond unit, DB, Rust and UI checks, the release needs an isolated GPU exercise
of ready-event preparation through a new user's actual Steam launch. Record
cold/prepared startup timings, clone mode on the real mount, two independent
writable seeded homes, absence of credentials, disable during active preparation,
re-enable reconciliation, and explicit host opt-outs. An unsupported image with
HOME/WorkingDir is a required negative case. Filesystem microbenchmarks alone do
not establish a faster first Steam launch.

Keep #145 open until the release containing its completed implementation is live
and those behaviors have been validated, per the operator's release-closeout rule.

## Proposed additive contract for approval

This section chooses one concrete design. It is a proposal for human/Opus review,
not permission to edit `protocol/` and not a claim that the behavior is shipped.
Reuse `config_update`, `register`, `capacity`, the administrator settings endpoints,
the image-catalog response and `template.warmup`; introduce no new endpoint or
WebSocket message type.

### Persisted desired policy and revision

Add two columns to the existing `instance_settings` singleton:

| Column | Definition | Meaning |
| --- | --- | --- |
| `steam_preparation_enabled` | `BOOLEAN NOT NULL DEFAULT true` | Independent source-wide gate for both preparation and consumption. |
| `steam_preparation_revision` | `BIGINT NOT NULL DEFAULT 1 CHECK (steam_preparation_revision > 0)` | Monotonic revision of the complete Steam preparation policy, including its adopted-image identity. |

Add the boolean to `PATCH /v1/admin/settings`. Add both fields to the GET/PATCH
response; revision is a **read-only decimal string** on JSON to avoid JavaScript
integer precision loss. PATCH rejects a client-supplied revision. Existing
administrator authorization and audit logging apply. PATCH continues its existing
merge semantics: an omitted boolean leaves the policy unchanged; null is invalid.

```json
{"steam_preparation_enabled": false}
```

The relevant part of the response is:

```json
{"steam_preparation_enabled": false, "steam_preparation_revision": "8"}
```

Increment the revision in the same transaction when the boolean changes or the
eligible adopted Steam image's immutable reference/version changes, including
adoption/removal. A no-op PATCH, repeated ready report, reconnect or unchanged
catalog sync does not increment it. Lock the singleton during revision updates;
read the revision, policy and adopted identity from one consistent snapshot.
The migration enables the desired default for existing installs too, but never
claims that an old agent has applied it. A missing singleton/policy read failure
must not synthesize an agent policy: report unavailable and retry.

After commit, push the complete policy to connected capable agents and reconcile
eligible already-adopted images, using the existing deduplicated jobs mechanism.
A reconnect sends the latest snapshot before preparation or seeding is permitted.
No source switch changes provider/discovery settings, app entitlements, active
user sessions or existing home contents.

### Explicit initial Steam eligibility

For v1, eligibility is an explicit identity predicate, not inference from HOME,
WorkingDir, a name containing Steam, or managed-home metadata alone. All of the
following must hold:

1. Adopted catalog entry `image_id == "steam"`, `kind == "prebuilt"`,
   `library_provider == "steam"` and runtime `managed_home == true`.
2. The adopted immutable registry reference has repository **exactly**
   `ghcr.io/accreleus/quasar-steam` and a valid `sha256` digest. Tags alone and
   repository-prefix lookalikes do not qualify.
3. The requested image version and digest exactly match that adopted identity.
   Catalog offers newer than the adopted version do not silently change it.
4. The host has that exact image ready before preparation starts. Seeding also
   requires a template published for that same digest/version and an absent or
   empty user home under the existing seeding rules.

The published stable catalog was read during this review: its entry is `steam`,
provider `steam`, prebuilt, version `2026.09.02`, at
`ghcr.io/accreleus/quasar-steam@sha256:586c67412921821287a5cf68c390aebbaf00fc750ce390170f219b465460bde5`.
This is provenance evidence, not a proposal to permanently pin that version.
Subsequent adopted digests from the same explicit image identity acquire a new
policy revision and require matching preparation. Custom/fork images are
**Unsupported** in v1 even if they set the same home metadata. Supporting another
image requires an explicit eligibility-policy change and validation.

### Control-plane to agent: separate source policy

Add optional `source_policies` alongside `settings` and `console_config` in
`config_update`. The existing `settings` map retains exactly its current meaning:
sparse host overrides over the environment baseline. The Steam block is instead
a **full authoritative source-policy snapshot**. Unknown source keys are ignored.

```json
{
  "type": "config_update",
  "settings": null,
  "source_policies": {
    "steam_preparation": {
      "revision": "8",
      "enabled": false,
      "images": [
        {
          "image_id": "steam",
          "registry_ref": "ghcr.io/accreleus/quasar-steam@sha256:586c67412921821287a5cf68c390aebbaf00fc750ce390170f219b465460bde5",
          "version": "2026.09.02"
        }
      ]
    }
  }
}
```

`images` contains at most the one eligible adopted Steam identity in v1; `[]`
means no supported image is currently adopted, not permission to inspect arbitrary
images. Keep the list when `enabled` is false, so an agent can report its preserved
matching template and disabled effective behavior. The agent independently
validates the exact repository/image identity and fails closed for preparation
if the snapshot is malformed. It must not accept arbitrary executable commands,
paths, credentials or preparation scripts in this block.

Send the block immediately after `registered`, after policy/adoption changes and
on reconnect. On one connection, a lower revision is ignored; a duplicate is
idempotent; the same revision with different content is a protocol error for this
feature (stop preparation/seeding and report it, without ending user sessions).
On reconnect, invalidate the previous connection's authorization and accept the
first valid full snapshot as authoritative; this permits a deliberate database
restore with an older revision. A `config_update` lacking this optional block
later on the same connection preserves the last snapshot, so an unrelated host
setting update cannot reset policy.

Before the first valid snapshot, and while disconnected, a new agent permits no
new preparation or template consumption. Cold application launch remains usable.
The control-plane jobs dispatcher must not hand out `template.warmup` until the
host has acknowledged the current snapshot. Queued jobs carry
`policy_revision`, `image_id`, `registry_ref` and `version` in the existing
job-specific params object; the worker checks all four against current policy
before work, at phase boundaries and immediately before atomic publication.
A stale job becomes deferred/cancelled through existing job outcomes, never a
new template publication. Job params alone cannot authorize preparation.

Effective booleans are resolved separately:

```text
preparation_enabled = eligible AND source_enabled AND host_warmup_permission
consumption_enabled = eligible AND source_enabled AND host_template_permission
```

For capable agents, absent `QUASAR_TEMPLATE_WARMUP` and `QUASAR_HOME_TEMPLATES`
mean permission granted; explicit recognized false values remain opt-outs.
Malformed explicit values deny the corresponding permission and report an
invalid-host-setting reason. The source switch wins over explicit host true.
The initial feature adds no new host override-map keys. Changes to host environment
still require container recreation; source-policy changes are live.

On disable, close the admission gate first, abort only the active preparation
through `WarmupControl`, and acknowledge the new revision after publication can
no longer occur. Preserve published templates and user homes. A seeding operation
already committed before receipt belongs to an existing home and is not undone;
one that has not committed must recheck the revision before installing its result.
Changing the template or home root invalidates the cached store and clone probe.

### Agent acknowledgement and effective image status

Add this optional capability to `register`:

```json
{"source_policy_versions": {"steam_preparation": 1}}
```

Version `1` promises the entire policy behavior above, including live disable,
consumption gating, revision checks and status reporting. Persist the current
connection's advertisement in a nullable `hosts.source_policy_versions JSONB`
column. Replace it on every register; absence clears previously advertised
support rather than retaining it from a newer agent. On disconnect it is historical
only: dispatch also requires a connected agent and a current acknowledgement.

Add optional `source_preparation` to `capacity`:

```json
{
  "source_preparation": {
    "steam": {
      "policy_revision": "8",
      "images": [
        {
          "image_id": "steam",
          "registry_ref": "ghcr.io/accreleus/quasar-steam@sha256:586c67412921821287a5cf68c390aebbaf00fc750ce390170f219b465460bde5",
          "version": "2026.09.02",
          "preparation_enabled": false,
          "consumption_enabled": false,
          "reason": "source_disabled",
          "state": "disabled",
          "template": {
            "registry_ref": "ghcr.io/accreleus/quasar-steam@sha256:586c67412921821287a5cf68c390aebbaf00fc750ce390170f219b465460bde5",
            "version": "2026.09.02"
          },
          "clone_mode": "copy",
          "clone_reason": "The template and home paths do not support reflink",
          "detail": "Prepared template preserved; new homes start without seeding"
        }
      ]
    }
  }
}
```

This is a full feature snapshot whenever included: an explicit empty image array
clears previous rows. Absence on an ordinary capacity report means no update;
absence of capability on a fresh register invalidates previous effective reports.
The sender does not choose a host ID: bind every report to the authenticated agent
connection. Accept rows only for that host and the adopted image identity; reject
malformed/oversized reports. V1 permits one image row and bounds human text fields
to 1,024 characters. Store receipt time server-side, not a client clock.

`policy_revision` acknowledges only an applied snapshot. While a newer desired
revision is pending, retain the previous effective report but project
`policy_pending: true`; do not render the desired boolean as effective. States
are `waiting_image`, `queued`, `preparing`, `ready`, `deferred`, `failed` and
`disabled`. The current source policy is applied even when its work is deferred.
`ready` requires a published matching template, not merely a pulled image or a
succeeded historical job. `template` and `clone_mode` are nullable when unobserved;
`clone_mode` is `reflink` or `copy`, measured on the actual paths. Report a copy
fallback without labelling the preparation failed.

Machine-readable reasons are `none`, `source_disabled`, `host_warmup_disabled`,
`host_templates_disabled`, `host_permissions_disabled`, `host_setting_invalid`,
`image_not_ready`, `host_busy`, `storage_unavailable`, `stale_policy` and
`preparation_failed`. Both effective booleans remain explicit so a preparation-only
or consumption-only host is unambiguous. `detail` carries bounded actionable text;
UI logic never parses it. Emit on policy application, preparation phase/terminal
changes, relevant store changes and reconnect, coalesced into the existing capacity
report loop rather than sending unbounded updates.

Persist the last report and server receipt time in new nullable
`hosts.source_preparation JSONB` and `hosts.source_preparation_reported_at TIMESTAMPTZ`
columns. The report is operational status, not scheduler authorization. Agent
policy admission remains authoritative. No credentials, user-home paths or user
identities appear in the report.

### Existing administrator image response

Extend each `ImageHostState` under `GET /v1/admin/images` (and the shared sync/action
responses using that shape) with nullable `steam_preparation`. The control plane
joins desired policy with the authenticated host's last report and projects:

```json
{
  "host_id": "11111111-1111-4111-8111-111111111111",
  "node_name": "gpu-host",
  "state": "ready",
  "steam_preparation": {
    "eligible": true,
    "supported": true,
    "desired_enabled": false,
    "desired_revision": "8",
    "applied_revision": "8",
    "policy_pending": false,
    "preparation_enabled": false,
    "consumption_enabled": false,
    "state": "disabled",
    "reason": "source_disabled",
    "template": {"version": "2026.09.02", "registry_ref": "ghcr.io/accreleus/quasar-steam@sha256:586c67412921821287a5cf68c390aebbaf00fc750ce390170f219b465460bde5"},
    "clone_mode": "copy",
    "clone_reason": "The template and home paths do not support reflink",
    "reported_at": "2026-09-06T12:00:00Z"
  }
}
```

The projection also admits `unsupported`, `unknown` and `pending_policy` states.
An ineligible image returns `eligible:false`, `state:"unsupported"`,
`reason:"unsupported_image"`, effective booleans false, regardless of HOME metadata.
A legacy agent returns `supported:false`, `state:"unknown"`,
`reason:"agent_upgrade_required"` and **null** effective booleans; absence of an
agent report is not evidence that templates are disabled. A capable agent awaiting
its first policy reports `pending_policy` and no applied revision. Offline hosts
retain receipt time but set `policy_pending:true`; the UI labels the report as
last observed. Unknown clone information stays null.

The image list's existing `hosts` coverage is preserved; do not synthesize a ready
image row just to show policy. The Steam source switch uses administrator settings
for its saved desired value and summarizes pending/unsupported hosts from this
projection. Image installation state remains separate from preparation state.

### Mixed-version behavior

A new control plane sends the block and dispatches preparation only after an agent
advertises version 1 and acknowledges the current revision. It cancels/skips old
queued preparation jobs for agents that cannot honor the policy. An old agent may
still seed from an existing template under its own environment flags; report that
as **unknown / agent upgrade required**, never as the source switch successfully
disabling consumption. Do not stop its user sessions to enforce this feature.
A new agent connected to an older control plane never receives source policy and
therefore uses cold homes without automatic preparation; explicit host true does
not bypass the missing policy. Upgrade the control plane first, then agents, using
the existing platform-update order. This limitation must appear beside the switch
whenever legacy agents are present.

### Exact frozen sections requiring review

- `protocol/control-api.md`: administrator `GET/PATCH /v1/admin/settings` field
  semantics; `GET /v1/admin/images` shared `ImageHostState` extension.
- `protocol/agent-api.md`: `register` capability advertisement and its reconnect
  reset; `config_update` distinct `source_policies` snapshot and ordering;
  `capacity` optional `source_preparation` acknowledgement/status semantics.
- `protocol/schema.md`: `instance_settings` boolean/revision and the three `hosts`
  capability/status columns; transaction/revision and connection-reset semantics.
- `protocol/openapi.yaml`: settings request/response properties and read-only
  revision; `ImageHostState.steam_preparation` and its typed status schema.
  Regenerate existing Go/web API drift consumers after the approved submodule pin.
- The existing `template.warmup` job-specific params/result contract documentation
  must record `policy_revision` and stale-work handling; do not reinterpret generic
  job success as complete image readiness.

No change is proposed to session assignment, image installation states, discovery
policy, home paths or a user's existing data. Approval of these concrete additive
semantics is the remaining design prerequisite for #145 implementation.
