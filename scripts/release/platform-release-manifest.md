# The platform release manifest

The machine-readable half of a stable **platform release** (`CONTEXT.md`): which
component images the release contains, by digest, and the commit they were built
from. It is attached as an asset to the GitHub Release for a `vX.Y.Z` tag, from
the same workflow run that built and promoted those images, so the human-readable
notes and the digests cannot disagree.

Read by people (it is one file, in the release, next to the notes) and by the
control plane: release detection lists the newest GitHub Releases and parses this
asset to learn that a release exists and what it is made of (#110). The contract
is `protocol/control-api.md`: `ReleaseManifest` (format 1) and "Release manifest
format 2".

There are two formats, under two asset names:

| format | asset | components | published by |
|---|---|---|---|
| 1 | `platform-release-manifest.json` | control-plane, node-agent | releases cut before RH-06 |
| 2 | `platform-release-manifest.v2.json` | control-plane, node-agent, recovery-actor, plus a `floor` | the release job now |

`generate-platform-release-manifest.sh` writes **format 2 only**; it no longer
writes format 1. `validate-platform-release-manifest.sh` reads both, dispatching on
`format_version`, because releases already published carry format 1.

> **No format-1 asset is published** (`protocol/control-api.md` "RH06 contract step",
> item 4, in force since RH06-15, #367): its last reader, the Compose updater, is
> retired, and a control plane implementing amendment 14 reads only the v2 asset, so a
> release that carries only the format-1 one is not listed.

> **Not `scripts/release/release-manifest.json`.** That file is the release
> preflight's *inputs* declaration — supported targets, upstream pins, vendored
> patches, required evidence — and is committed, hand-maintained, and read by
> `release-preflight.sh`. This one is *generated per release*, describes
> *outputs*, and is never committed.

## Format 2: `platform-release-manifest.v2.json`

```json
{
  "format_version": 2,
  "version": "0.4.0",
  "prerelease": false,
  "source_commit": "cccccccccccccccccccccccccccccccccccccccc",
  "built_at": "2026-10-01T12:00:00Z",
  "schema_version": 96,
  "components": [
    {
      "name": "control-plane",
      "image": "ghcr.io/accreleus/quasar/quasar-control-plane",
      "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "name": "node-agent",
      "image": "ghcr.io/accreleus/quasar/quasar-node-agent",
      "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    },
    {
      "name": "recovery-actor",
      "image": "ghcr.io/accreleus/quasar/quasar-recovery",
      "digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    }
  ],
  "floor": [
    {
      "name": "node-agent",
      "version": "0.4.0-0"
    },
    {
      "name": "recovery-actor",
      "version": "0.4.0-0"
    }
  ]
}
```

This is `testdata/release/platform-release-manifest.v2.json`, which the Go tests
also read; `fixtures/manifests/valid-v2.json` is a copy the shell test holds equal
to it.

Every format-1 field keeps its name, grammar and meaning (table below). What
format 2 adds:

| field | type | grammar | meaning |
|---|---|---|---|
| `format_version` | integer | exactly `2` | |
| `components` | array | exactly three, in this order | `control-plane`, `node-agent`, `recovery-actor`. The image of `recovery-actor` is `<namespace>/quasar-recovery`; the seed is a mode of the same image, so it is not a component. |
| `floor` | array | exactly two, in this order | `node-agent`, then `recovery-actor`. |
| `floor[].name` | string | `node-agent`, then `recovery-actor` | Fixed set, fixed order. |
| `floor[].version` | string | the grammar of `version`; must not order above `version` by SemVer precedence | The oldest release of that component this release's control plane still manages (`CONTEXT.md` "Floor"). |

No other keys, at either level.

**Where the floor comes from.** The generator reads the two lines
`const FloorNodeAgent = "<semver>"` and `const FloorRecoveryActor = "<semver>"`
from `control-plane/internal/buildinfo/floor.go` (`--floor-source` overrides the
path, for tests). The control plane judges `below_floor` against the same two
constants, so the floor a control plane enforces and the floor its release
publishes cannot disagree. A missing, repeated or malformed line fails the
generator.

## Format 1: `platform-release-manifest.json`

```json
{
  "format_version": 1,
  "version": "0.2.0-rc.1",
  "prerelease": true,
  "source_commit": "cccccccccccccccccccccccccccccccccccccccc",
  "built_at": "2026-09-04T12:00:00Z",
  "schema_version": 74,
  "components": [
    {
      "name": "control-plane",
      "image": "ghcr.io/accreleus/quasar/quasar-control-plane",
      "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "name": "node-agent",
      "image": "ghcr.io/accreleus/quasar/quasar-node-agent",
      "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    }
  ]
}
```

| field | type | grammar | meaning |
|---|---|---|---|
| `format_version` | integer | exactly `1` | The version of **this file format**. Nothing to do with the release's version or the database's. |
| `version` | string | strict semver `X.Y.Z` with an optional `-prerelease` part; no `v` prefix, no `+build` metadata | The release. It is the git tag minus its leading `v`, and it is also the immutable `:X.Y.Z` tag each component image carries. |
| `prerelease` | boolean | true iff `version` has a prerelease part | Derived, never asserted by the caller. A `true` here is the same fact as the GitHub Release's prerelease flag: the `stable` channel ignores it. |
| `source_commit` | string | 40 lowercase hex | The tagged commit every component was built from. One commit, not one per component. |
| `built_at` | string | RFC3339 UTC, `YYYY-MM-DDTHH:MM:SSZ` | When the workflow run started, shared by every image in the run (the `inputs` job's `built_at`, also baked into the images' `org.quasar.built.at` label). |
| `schema_version` | integer | ≥ 1 | The **database migration** version embedded in this release's control plane: `max(NNNN)` over `control-plane/migrations/NNNN_*.up.sql` at `source_commit`. ADR 0002's vocabulary — the number a host must never roll *below*, because the control plane runs migrations forward on boot and crash-loops against a database that is ahead of it. Not `format_version`, and not `version`. |
| `components` | array | exactly two, in this order | The images the release is made of. |
| `components[].name` | string | `control-plane`, then `node-agent` | Fixed set, fixed order. |
| `components[].image` | string | a registry reference with **no tag and no digest** | Consumers compose `image@digest` themselves. A tag is never an identity anywhere in the release path (ADR 0001). |
| `components[].digest` | string | `sha256:` plus 64 lowercase hex | The exact manifest the workflow validated and promoted. |

No other keys, at either level. The validator rejects an unknown top-level key
and an unknown component key, so the format cannot drift silently: a consumer
that parsed a manifest once can keep parsing it.

## How it is produced

The `release` job of `.github/workflows/images.yml`, on a `v*` tag push:

1. `scripts/release/generate-platform-release-manifest.sh` writes the format-2
   manifest from the tag's version, the three build jobs' digest outputs, the
   `inputs` job's shared `built_at`, `GITHUB_SHA`, and `REGISTRY_NS`.
   `schema_version` and the floor are read from the checked-out tree;
   `prerelease` is derived from the version. Nothing is invented.
2. `scripts/release/validate-platform-release-manifest.sh` re-checks it with
   `--expect-format 2`, `--expect-version` and all three `--expect-*-digest`, so
   the file must describe *this* run's artifacts.
3. The release-time check (below) refuses a release that could leave a machine
   unmanageable.
4. The job separately asserts that the promoted `:X.Y.Z` tags resolve to the same
   platform-manifest set as the manifest's digests — the manifest cannot name
   something other than what a puller of that version tag gets.
5. It is uploaded as the release asset `platform-release-manifest.v2.json`, with
   `platform-release-manifest.v2.json.sig` when signing is configured. No
   format-1 asset is uploaded.

The generator and validator run offline, need no Docker, and are covered by
`scripts/release/test-platform-release-manifest.sh`.

## The release-time check

`scripts/release/check-release-compatibility.sh` is ADR 0008's "release-time
check". It takes the candidate manifest, the previously published format-2
manifests, the `org.quasar.recipe` label of every image they name, and the
candidate recovery actor's own recipe windows (`quasar-recovery recipes`), and
refuses the release, printing every reason, when:

- **(a)** the candidate actor's window for `control-plane`, `node-agent` or
  `recovery-actor` does not contain the recipe revision of the candidate's own
  image for that role;
- **(b)** for `node-agent` and `recovery-actor`, a known release whose version is
  between that component's floor and the candidate's version (inclusive, SemVer
  precedence) has an image whose revision the candidate actor cannot render; or
  the previous release's control-plane revision is outside the actor's
  `control-plane` window, so a pre-update control plane could not be restored
  (ADR 0008 Rule B);
- **(c)** a floor orders above the previous release, so a component the previous
  release left fully current would be below the floor after a one-step update;
- **(d)** the previous release's recovery actor does not carry the candidate
  actor image's `recovery-actor` revision. In the hand-over the running actor
  renders its successor, so a machine on the previous release could never take
  this one. Its windows come from that actor's own `quasar-recovery recipes`
  (`--previous-actor-windows`, required whenever a previous release exists).

"Known releases" are the published releases carrying
`platform-release-manifest.v2.json`; format-1 releases are ignored, because no
owned install exists before the first format-2 release. "The previous release" is
the known release ordering highest strictly below the candidate. With no known
format-2 release, (b) checks nothing beyond (a), and (c) and (d) pass. A known manifest
with the candidate's own version is refused.

In CI, `scripts/release/collect-release-compatibility-inputs.sh` gathers the
inputs (`gh release download`, `docker buildx imagetools inspect`, and
`docker run --network none <actor> recipes`); it skips the release's own tag, so
re-running a half-published release works. The check itself is offline and is
covered by `scripts/release/test-release-compatibility.sh` over
`fixtures/compatibility/`.

## How it is consumed

The control plane's release detection reads the newest GitHub Releases for the
repository and, for each, fetches the v2 asset. A release that carries only the
format-1 asset predates owned installs and is not listed, with no fault; one whose v2
asset is missing or unparseable is a release it does not offer. From the manifest it takes:

- `version` + `prerelease` — what to show, and whether the instance's channel
  wants it at all (`stable` skips prereleases).
- `schema_version` — the no-downgrade comparison of ADR 0002.
- `components[].image` + `.digest` — composed into `image@digest` and handed to
  the recovery actor of the target's machine, so a host pulls exactly the bytes the control plane resolved and
  never a floating tag (ADR 0001).

A control plane that predates format 2 fetches only `platform-release-manifest.json`,
so it never sees a format-2 release.

## The detached signature

A release may carry a signature beside its manifest asset —
`platform-release-manifest.v2.json.sig` for format 2,
`platform-release-manifest.json.sig` for format 1: a detached signature over the
asset's exact bytes. It changes nothing here, and a consumer that has never heard
of it keeps parsing the manifest as before. Schema and verification:
`scripts/release/platform-release-signature.md`.

## Versioning rule

Adding, removing or re-typing any key is a `format_version` bump. Consumers must
ignore a manifest whose `format_version` they do not know rather than
best-effort-parsing it — an unknown format is "no release I can apply", which is
a safe answer; a half-understood one is not.
