# RH-02 #263: the readiness override — acceptance

| | |
|---|---|
| Ticket | #263, specification #252, protocol amendment 11 (#260), ADR 0005 |
| Source | `feature/rh-02-gate`: slice `b458c95`, two card layout fixes found in the visual check `4b8f15f` and `ac5c7ea`. On top of #268, #261 and #262. |
| Not on | `develop`, `main`. No tag, no image publication, no Actions build, no shared stack touched. |
| Protocol pin | `7ecc596`, moved in `b458c95`, the commit that registers the two routes. Verified reachable from `quasar-protocol` `origin/main` before pinning. `web/src/api/schema.d.ts` regenerated from it. The native client needs no change: it reads no host body and calls neither route. |
| Images | `quasar-control-plane` from `b458c95`, `4b8f15f` and `ac5c7ea` through `deploy/build-images.sh control --git-ref … --no-prune --no-latest` (contract 23 passed, 0 failed each; `schema.version=85`). The agent image is #261's, from `97fd0d9`: no agent code changed. |

## What changed

- `internal/readinessgate`, a DB-side package both `agentws` and `crud` call. The report
  write, the two override writes and the verdict recompute share one transaction shape: lock
  the host row, re-read the stored report, evaluate, write, recompute, commit. The recompute
  that #262 put in `agentws` moved here; there is one copy.
- `PUT` and `DELETE /v1/admin/hosts/{id}/readiness-overrides/{check_id}`, wrapped in
  `RequireAuth → RequireAdmin` at registration. `check_id` is validated on the value the mux
  already decoded once, so a literal `%` or `/` after that decode is a `400`.
- The precondition is evaluated on every `PUT`, a repeat included. See "One reading of the
  contract" below.
- Lapse inside the recompute, in the transaction that stores the passing report.
- Audit rows inside the same transaction through a new `audit.RecordTx`, which shares
  `Record`'s bounding. `host.readiness_override.set` joined the destructive-action set.
- The host body serves `readiness_overrides` with `created_by_username` from a join at read
  time, and judges `inert` on the same host snapshot as the gate's `blocking`.
- The card: "Launch anyway", "Overridden by admin", "Withdraw override", a list of inert
  overrides, a stale-report notice. The host page wires them through `useAdminAction` and the
  resource layer behind a confirmation in the console's shared `Modal`. The Fleet row shows
  markers only; the setup wizard is unchanged.

## One reading of the contract

`control-api.md` says `PUT` is idempotent and returns the existing override, and says `409`
when the current report has no check with that id carrying `blocks` and status `fail`, or when
it is enforced by the agent. Both apply to a repeat `PUT` on an override whose check has since
stopped failing or become agent-enforced. The `409` rule is unconditional in the text, so it
is evaluated first: idempotency is for a repeat that would succeed. A refused repeat leaves
the stored row as it is; only a pass lapses it. The first implementation answered `200` there;
the locking review caught it.

## Tests, written first

| Owner | Tests |
|---|---|
| Fable, before any implementation | `internal/readinessgate/gate_db_test.go`: create, repeat (same `created_at`, no second audit row) and clear (idempotent), each recomputing the verdict in the same call and only for its scope; eight refusals that store nothing, audit nothing and move no column (no such check, a proxy, passing, `unknown`, a warning, agent-enforced, an empty report, never reported); a repeat refused once the check is agent-enforced, `unknown` or gone, with the row held; an unknown host; lapse on pass and only on pass (held on fail, `unknown`, `warn`, `skip`, an empty report, a vanished id), audited with a null actor, and a later failure blocks again while another override survives; the served list with the author, `inert` after a rename, an inert override withdrawn, and both author fields null once the account is deleted; a `PUT` racing a report 25 times in both directions under a 15 s bound, failing on any error but a refusal or on a stale column; the recompute inside a caller's transaction. |
| Fable, before any implementation | `internal/crud/readiness_override_http_test.go`: `200` with exactly five keys, the repeat, the host body never hiding the failing check, `204` twice; four `409`s with a message (the agent one must say so); `404` on an unknown host for both methods; seven malformed ids as `400` for both methods and three well-formed ones that never are; a non-admin is `403` and no token is `401` for a real host, an unknown host, a malformed check id and a malformed host id, with no row written. `internal/audit/severity_readiness_test.go`: `set` is `warn`, `cleared` and `lapsed` are `info`. Two of these tests were wrong as first written (a session left live in #262's homes test; the `{"host": …}` envelope ignored here); in both cases the implementing model reported it instead of editing the test. |
| Sonnet | `TestFindBlocking` for the pure helper the override path uses. |
| Haiku | 17 card tests: nothing new without the new props; "Launch anyway" only with a handler, a gate entry, not overridden and not agent-enforced; the overridden state (the Fail glyph kept, the marker text and title, "Blocks launches" gone, the withdraw button); a null author; the pending state; inert overrides with and without a handler; the stale notice; "Needs attention" kept. |
| Sonnet | 5 host-page tests: the props reach the card, confirm then set then refetch, a declined confirmation, withdraw then refetch, a `409` with the page still usable. |

## Review of authorization and locking (Opus, read-only)

Eleven questions, each answered with file and line. Authorization: both routes are wrapped at
the one registration site, the mux is method-exact so no other verb reaches the handlers, a
non-admin is refused before any lookup or validation with no database access at all, the actor
comes only from the authenticated context, no other path can set or clear an override (an
agent can only make one lapse, the safe direction), and neither the `503` nor any
non-admin endpoint carries check detail. Locking: every entry point takes the host row lock
first; no path leaves the derived columns older than the stored report or overrides; the
precondition and the insert cannot be separated by a report; two concurrent `PUT`s yield one
row and one audit row. Acted on:

- the repeat-`PUT` precondition, above;
- the served `inert` flag came from a later statement than the gate's `blocking`, so one
  response could call an entry both overriding and inert; it now uses the one snapshot;
- this slice's own test inserted a GPU row before taking the host lock, the order that
  inverts against admission; it now models the capacity write, and `Recompute` documents the
  obligation;
- the handlers discarded `UserFromContext`'s `ok`; they now fail closed.

Filed, not fixed here: #271. Admission's recheck locks the GPU row then the host row, and the
capacity write has always locked the host row then every GPU row, on every report. A lost
deadlock on the admission side is a `500` on a launch. It predates RH-02, this work adds no
edge to it, and no occurrence was observed. It is a placement-loop change that wants its own
failing test.

## Code review of the whole branch (standards and spec, two reviewers)

Run on `56b0c76` after the slices were gated. Three findings were real and are fixed in the
commit after this record's first version:

- **A held override could become invisible.** Only a pass lapses an override, so one is
  still stored while its check is `warn`, `unknown` or `skip`. Such a check is not in
  `readiness_gate.blocking` and its override is not inert, and the card rendered the marker
  and the withdraw control only from `blocking`. `homes_free_space` reaches it: exhausted,
  overridden, then merely low. The override would have sat unseen and lifted the next
  failure. The card now shows the marker and the withdraw control for any stored, non-inert
  override on a check, and keeps "Can block launches" beside it. Three component tests,
  written first.
- **A repeated check id could lapse an override that was lifting a failing twin.** Ids are
  agent-owned and stored verbatim. With one entry passing and one failing under the same id,
  the override lapsed and the scope read unblocked with no override stored until the next
  report. A lapse now needs a pass and no entry with that id still failing. One verdict test,
  written first.
- **`window.confirm` was a new idiom.** This record first said no shared confirm component
  exists; that was wrong. The console's idiom is the shared `Modal` with a state flag and
  `Button`s in its footer, as the session page's terminate confirmation does. The host page
  now uses it, and `window.confirm` is gone from `web/src`.

Also fixed from the standards pass: two comments that cited a function this slice had moved,
a comment carrying change history, and shouted words. Left as they are, as judgement calls:
the default window defined in three packages with a cross-reference, the thin `_inner`
wrappers in the agent's runtime and storage checks, and the repeated `None` fields on
`ReadinessCheck` literals.

## Gates at `b458c95` (rerun after the review fixes)

| Gate | Result |
|---|---|
| `make verify` | 432 pass, 0 warn, 0 fail |
| `make test-go` | pass, including the OpenAPI route drift test at pin `7ecc596` |
| `make test-db` (fresh ephemeral Postgres, `-p 1`) | pass, 4 of 4 stages |
| `make test-web` | pass: schema drift, typecheck, 236 test files, production build |
| `make preflight` | exit 0 |
| the race test, `-count=3 -race` | pass |
| leak scan (tree, operator patterns) | clean |

All six gates, `make test-rust` included (1715 passed), were run again on `56b0c76`, and
`verify`, `test-go`, `test-db`, `test-web` and `preflight` once more on the code-review fixes.
All pass.

## Live evidence: AMD test host, 2026-09-19

Shared-host version preflight: zero containers and zero compose projects, read again
immediately before the first mutation. A disposable stack under its own compose project,
fresh database at schema 85, removed afterwards with its volumes and roots. The fault is
#262's: no Vulkan driver and no VA driver in the agent container.

| Step | Observed |
|---|---|
| Blocked | The gate lists `media_probe_gpu1`. Launch: `503 host_not_ready`. |
| The override refused | `encoder_codecs` (a failing proxy): `409`. `runtime_endpoint` (passing): `409`. An id the report lacks: `409`. `bad%20id`: `400 validation_failed`. |
| The override set, twice | `200` both times with the same `created_at`, `created_by_username` the admin, `inert: false`. |
| After it | `blocking` still lists the check, now `overridden: true`; `readiness_overrides` has the entry; `gpus.readiness_blocked = f`. |
| Launch | `201`, placed on that GPU. The session then fails on the host with the agent's own words: no encoder on this host can produce the codec. The fault is real, so the evidence was right and the override only lifted the gate, which is all it claims to do. |
| The fault cleared | The probe passes and the control plane removes the override: `readiness_overrides: []`, and an audit row `host.readiness_override.lapsed`, actor null, `info`. |
| The card | `rh02-263/overridden-check-amd.png`: the Fail glyph, the summary and the fix all still shown, "Overridden by admin" in `chip chip-warning chip-sm` in place of "Blocks launches", title "Overridden by admin on …", "Withdraw override" under the text. `rh02-263/blocking-check-with-control-amd.png`: "Blocks launches" and "Launch anyway". |
| Withdrawn through the console | The button's `DELETE`, the host re-read, and the tile back to "Blocks launches" with "Launch anyway"; the gate `overridden: false`. |
| Audit trail | `set` (`warn`, the admin), `lapsed` (null actor, `info`), `set`, `cleared` (`info`, the admin), each with `node_name` and `check_id`. |

Two things the run showed without being asked:

- A missing audio sidecar image makes the audio probe inconclusive. It reported `unknown`,
  the gate listed nothing, and nothing was blocked.
- With the render node made unreadable to the application identity, the proxy
  `dri_node_app_access` failed while the evidence check `application_gpu_probe_gpu1` still
  passed, because the probe container really could open the GPU. Nothing was blocked. That is
  ADR 0005's point. The device modes were recorded first and restored straight after (`777`,
  confirmed again at teardown).

The first build put the buttons in the tile's heading row, where a 360 px tile wrapped the
title to three lines; `4b8f15f` moves them under the text and `ac5c7ea` spaces the row with
`--s2`. Both screenshots are from the final image.

## Not verified

- A launch that *succeeds* under an override. It needs a check that fails while the host can
  in fact run the session, and every definitive fault that could be staged was real. The
  admission half is shown (`503` before, `201` and placed after).
- An inert override on a live stack: it needs the agent to rename a check. DB tests cover it,
  and the card's inert list is covered by component tests.
- NVIDIA: not run, for the reason in #262's record. Intel: not validated, nothing claimed.

## Models

The failing tests for the gate, the HTTP contract and the severity rule, every diff review,
the gates, the pin, the evidence and this record: Fable 5.1. Backend and its helper test,
card component, API client, host-page wiring and its tests: Sonnet 5. Card tests and the
changelog entry: Haiku 4.5. The authorization and locking review: Opus 5. Review edits by
Fable: the four fixes above, the two corrected tests, the changelog's audit severities (it
said withdrawing is `warn`), the two layout fixes, and comments cut to the repo's bar.
