# `platform-release-manifest.json` — the platform release manifest

The machine-readable half of a stable **platform release** (`CONTEXT.md`): which
component images the release contains, by digest, and the commit they were built
from. It is attached as an asset to the GitHub Release for a `vX.Y.Z` tag, from
the same workflow run that built and promoted those images, so the human-readable
notes and the digests cannot disagree.

Read by people (it is one file, in the release, next to the notes) and by the
control plane: release detection lists the newest GitHub Releases and parses this
asset to learn that a release exists and what it is made of (#110). The protocol
amendment documents the same shape as `ReleaseManifest` (#106).

> **Not `scripts/release/release-manifest.json`.** That file, which has lived here
> since the release-evidence work, is the release preflight's *inputs* declaration
> — supported targets, upstream pins, vendored patches, required evidence — and is
> committed, hand-maintained, and read by `release-preflight.sh`. This one is
> *generated per release*, describes *outputs*, and is never committed. Two
> different things; the longer name is the published one.

## Example

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

## Fields

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

1. `scripts/release/generate-platform-release-manifest.sh` writes it from the
   tag's version, the two build jobs' digest outputs, the `inputs` job's shared
   `built_at`, `GITHUB_SHA`, and `REGISTRY_NS`. `schema_version` is read from the
   checked-out tree; `prerelease` is derived from the version. Nothing is invented.
2. `scripts/release/validate-platform-release-manifest.sh` re-checks it with
   `--expect-version` and both `--expect-*-digest`, so the file must describe
   *this* run's artifacts.
3. The job separately asserts that the promoted `:X.Y.Z` tags resolve to the same
   platform-manifest set as the manifest's digests — the manifest cannot name
   something other than what a puller of that version tag gets.
4. It is uploaded as the release asset `platform-release-manifest.json`.

Both scripts run offline, need no Docker, and are covered by
`scripts/release/test-platform-release-manifest.sh`.

## How it is consumed

The control plane's release detection reads the newest GitHub Releases for the
repository and, for each, fetches this asset. A release whose asset is missing or
unparseable is a release it does not offer. From the manifest it takes:

- `version` + `prerelease` — what to show, and whether the instance's channel
  wants it at all (`stable` skips prereleases).
- `schema_version` — the no-downgrade comparison of ADR 0002.
- `components[].image` + `.digest` — composed into `image@digest` and handed to
  the updater, so a host pulls exactly the bytes the control plane resolved and
  never a floating tag (ADR 0001).

## Versioning rule

Adding, removing or re-typing any key is a `format_version` bump. Consumers must
ignore a manifest whose `format_version` they do not know rather than
best-effort-parsing it — an unknown format is "no release I can apply", which is
a safe answer; a half-understood one is not.
