---
paths:
  - "control-plane/**"
  - "node-agent/**"
  - "web/**"
---

# The comment bar

A comment earns its place only by stating something the code cannot show. Everything else is
noise a reader has to wade through to reach the one line that mattered.

## What earns a comment

- An **invariant** the type system does not enforce ("callers hold the lock", "list order is
  preference order").
- A **gotcha**: behaviour that surprises, that a plausible edit would break.
- **Why not the obvious way**: the tried-and-rejected alternative, in one sentence.
- A **cross-file contract**: the other half of a pair that cannot point back here
  (a generated file, a SQL twin, a shell script, a wire contract).
- A **safety rule**: fail-open/fail-closed, teardown ordering, a UAF or Xid workaround.

## What does not

- **Narration.** `// loop over the hosts` above a loop over the hosts.
- **Change history.** Git holds it. No "was X, now Y", no "added in PR #…", no dated
  changelogs in source.
- **Review residue.** "As discussed", "per review feedback", "renamed for clarity".
- **Repeated issue tags.** ONE locating tag per concept, at its definition. Not on every
  use site.
- **Essays defending against objections nobody raised.** If no reviewer asked "why isn't
  this a 400?", do not spend nine lines answering.
- **Doc comments restating the name.** `/** The user's ID. */` over `userId: string`.

## Style

Terse. One or two lines is the norm; five is already suspicious.

- No ALL-CAPS headings or shouted words for emphasis.
- No em-dash chains stacking three clauses into one sentence.
- No rule of three — say it once. Restating a point in three registers is the tell.
- No self-congratulation: "that is the seam", "this is the elegant part", "deliberately".
- Client-facing semantics belong in the contract, not in source. Point at it:
  `// semantics: control-api.md §errors`.
- Env vars: name the var, do not document it. `docs/configuration.md` is canonical.

## Compress, never cut

A block containing any of these is **compressed to its constraint**, never deleted:

- The words must / never / fail-open / fail-closed / deadlock / race / leak / panic / UAF / Xid.
- A frozen-contract path (`protocol/*.md`) or a migration number.
- A measured number with units — keep the number and the report path.
- A named test — shrink to naming it: `// guarded by TestCertForRungMatchesPickCert`.
- A `QUASAR_*` env var name.

When compressing, the constraint survives verbatim in meaning. If you cannot keep it in two
lines, move the prose to the contract doc and leave a pointer.

## Examples

**Before** (28 words of narration and history over one line of code):

```go
// CodeCapacityUnavailable (409) is the superseded Phase-1 launch
// capacity-rejection — retained for compatibility but no longer emitted; the
// launch path now returns CodeNoHostAvailable / CodeCapacityExhausted (503).
CodeCapacityUnavailable = "capacity_unavailable" // 409 (superseded, unused)
```

**After** — the constraint is "do not emit this"; the rest is history:

```go
CodeCapacityUnavailable = "capacity_unavailable" // 409, deprecated: do not emit
```

**Before** (an essay restating one fact three ways):

```ts
/**
 * A handful of OpenAPI schemas still ship without `required:` arrays (the
 * session-trace / diagnostic-bundle shapes and the profile-policy/-preferences
 * responses), so `openapi-typescript` emits their properties as optional even
 * though the control-plane always populates them. `Req<T>` restores that
 * requiredness: it recurses through objects/arrays and strips the optional-
 * `undefined` from each property (via the `-?` mapped modifier) while preserving
 * genuine `| null` unions. Every other type below aliases the generated schema
 * directly — this helper is now scoped to just those still-loose schemas and can
 * be dropped once they gain real `required:` declarations upstream.
 */
```

**After** — what the reader cannot see is *why it exists at all*:

```ts
/** Makes generated properties required, preserving `| null`. Only for schemas that
 *  still lack `required:` in openapi.yaml; drop it when they gain one. */
```
