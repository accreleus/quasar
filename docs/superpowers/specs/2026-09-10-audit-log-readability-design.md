# Audit log readability — names beside ids, and a detail pane a human can read

Status: implemented on `feat/audit-log-readability`, awaiting operator review.
Date: 2026-09-10.

## The problem

`/admin/audit` renders identifiers and nothing else. A `session.launched` row
shows `session 85d0b6a9` in the Target column and, when expanded:

```
{
  "app_id": "8b1116c8-fc33-4c01-9110-11601bab6be7",
  "host_id": "4daeaa27-8f6f-4dad-aaae-475f1d304916"
}
```

Nothing on that screen says *Steam* or *gpu-test*. An operator reading their own
audit log has to hand-resolve every uuid against another page.

Three defects, in order of how much they hurt:

1. **No names anywhere.** Not for the target, not for the ids nested in
   `details`.
2. **The Detail pane is raw JSON.** `design_handoff_v3` never specified JSON — the
   mock's expanded pane is multi-line `key=value` text. The current
   implementation also prefixes three `action/target/actor` header lines that
   its own comment attributes to the mock; the mock has none.
3. **`ACTION_LABELS` has drifted badly.** 9 of its 25 keys name actions the
   server never emits (`app.create`, `user.create`, `invite.create`,
   `session.force_stop`, `settings.update`, …), and ~30 actions that *are*
   emitted have no entry, so most rows fall through to `humanise()`.

## Principle

**Never show an id without its name; never show a name without its id.** Ids
stay — they are what you paste into a query. The name is what tells you what
happened. Both, on every surface.

## Design

Three parts. Parts 2 and 3 need no contract change; part 1 does.

### 1. Server: a derived `names` map per activity item

`AdminActivity` grows one additive, derived, read-only field:

```
names: { "<id>": "<display name>" }
```

It contains the target's name (keyed by `target_id`) plus a name for every id
found in `details` that the resolver knows how to look up. Derived at read time
and never stored — the same rule `actor_username` and `severity` already follow,
and the same amendment class as the 2026-08-28 UI v3 amendment that introduced
them.

One map rather than a `target_name` scalar, because the ids that matter most in
practice are *inside* `details` (the screenshot's `app_id`/`host_id`), and a map
covers the target and the nested ids with one field and one rule.

**Per item, not per response.** `useOverviewData.ts` keeps only `activity.items`,
so a response-level map would be silently dropped by the Overview's Recent
Activity card. Per-item duplicates a few short strings per page and nothing else.

**Resolution is an explicit allowlist, not a uuid-shaped heuristic.** A heuristic
cannot work here:

- `run_id` means `job_runs` under `job.run` and `platform_apply_runs` under
  `platform.apply.cancel` — the same key, two tables.
- `subject_id` is a user id only when the sibling `subject_type == "user"`.
- `entitlement_id`, `attempt_id`, `capture_id` are uuids with no name worth
  showing; probing them is pure cost.
- `session.failed` writes free text into `reason` and `state_detail`, which can
  contain a uuid. A heuristic would query on those.

So: a `target_type` (+ action) → kind table, a detail-key → kind table, and a
uuid shape check before any value reaches a `::uuid` cast — an unparseable value
reaching that cast is a 500, which the handler already guards against for query
parameters.

**Structure** follows the `stream_plan.go` discipline already established in this
repo — pure decision, then I/O, then attach:

- `collectRefs(items) []ref` — pure, no I/O, exhaustively unit-testable.
- `(*Store).resolveNames(ctx, refs)` — one `WHERE id = ANY($1)` per kind present
  on the page. A page is ≤100 rows and there are ~11 kinds, so it is at most ~11
  primary-key lookups, not N+1.
- `attachNames(items, resolved)` — pure.

**A miss falls back to the name recorded at write time.** When an entity has been
hard-deleted, the lookup finds nothing; the resolver then seeds
`names[target_id]` from the stamped `details` key (`name`, `node_name`,
`username`, `app_name`). One rule, applied server-side, so every consumer sees
"the current name, else the name it had when this happened".

Kinds and their name columns:

| kind | source |
|---|---|
| app | `apps.name` |
| host | `hosts.node_name` |
| user | `users.username` |
| session | `apps.name · users.username` via three LEFT JOINs |
| runtime_preset | `runtime_presets.name` |
| stream_profile / launch_profile | `*.display_name` (text slug id) |
| image | `image_catalog.display_name` (text slug id) |
| job | `jobs.name` (text slug id) |
| platform_release | `version`, else a 12-char `source_commit` |
| platform_run | its release's label |
| host_enrollment | `node_name`, else `note` |

`invite`, `secret` and `instance` have no name: an invite's code is deliberately
never recorded, and a secret's `target_id` already *is* its name.

### 2. Server: stamp the name where read-time resolution can never work

Six delete/tombstone sites already stamp a name. Two gaps:

- `user.deleted` writes no details at all — the row is gone by the time anyone
  reads the audit entry, so the username is unrecoverable. Capture it before the
  delete.
- `launch_profile.delete` stamps a name only when a pre-delete `Get` happened to
  succeed. Mirror what `stream_profile.delete` does.

And one addition that is not a gap but is worth its two lines: `session.launched`
stamps `app_name` and `host_name` alongside the ids it already writes. An app can
be hard-deleted, and when it is, every historical launch of it should still say
which app it was.

No contract change — `details` is free-form jsonb.

### 3. Web: names on every surface, and a readable detail pane

New pure module `web/src/pages/admin/audit/describe.ts`:

- `ACTION_LABELS` rebuilt from the real emitted vocabulary (~57 actions), with
  a drift test that fails if a label names an action the server does not emit.
- `nameFor(item, id)` — `names[id]`, else the stamped detail name, else nothing.
- `targetLabel(item)` — resolved name, else stamped name, else `"{type} {id:8}"`.
  Shared with the Overview card, which currently has its own copy of that rule.
- `summaryLine(item)` — the one-line `key=value` the mock puts in the Detail
  column, with names substituted for ids: `app=Steam host=gpu-test`.
- `detailLines(item)` — the expanded pane: a leading human sentence, then the
  `key=value` lines with ids **in full**, each annotated with its name:
  `app_id=8b1116c8-fc33-4c01-9110-11601bab6be7 (Steam)`.

Rendering stays inside the mock's shapes (`<pre class="aud-pre">`, the
`key=value` form, the existing columns). Two deliberate deviations from
`design_handoff_v3`, both flagged for review:

- The expanded pane opens with a human sentence before the `key=value` lines.
  The mock has only `key=value`. This is the operator's explicit ask for "a
  useful message a user can look at and understand"; it uses the same `<pre>`
  and adds no new style.
- The three `action/target/actor` header lines the current code emits are
  **removed**. They were never in the mock, and the sentence plus the annotated
  lines carry strictly more.

Nothing is lost by dropping the JSON: `detailLines` renders *every* key in
`details` generically, with objects and arrays as compact JSON on their own line,
so an unknown key from a future action still shows up.

CSV export gains a `target_name` column; `details` stays raw JSON so export
fidelity is unchanged.

## Explicitly out of scope

- **`q=` searching over names.** The contract fixes `q` to
  action / target_id / actor username and documents why it excludes `details`.
  Searching names means joining ~11 tables into the `WHERE` with an unindexed
  `ILIKE`, plus another amendment, for a filter that would then disagree with
  server paging. Separate ticket.
- **Server-side sentences.** The sentence is a rendering. Putting it on the wire
  would make every UX iteration a contract amendment.
- **Auditing `app.create` / `app.update` / `user.create`,** which are not audited
  at all today. Real gap, unrelated ticket.
- **Fixing `library.scan.force`'s `target_type=library` (it carries an app id)
  and `platform` naming two tables** at the emitter. Existing rows keep the old
  values either way, so the resolver maps them instead.

## Verification

- `make test-go`, `make test-db` (auth is DB-touching), `make test-web`.
- Live on the local Hermes stack: real audit rows produced through the admin API,
  then the rendered page checked in a browser against the mock.
