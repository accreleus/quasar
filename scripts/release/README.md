# scripts/release — release-evidence gates

Release preflight, the supply-chain library and manifest, SBOM generation, image scanning, the Vulkan encoder runtime probe, and their offline contract tests (mock docker). Catalogue: `docs/developer-tooling.md`.

The tag-push release lane (`.github/workflows/images.yml`) also lives here: `changelog-section.sh` extracts a version's `CHANGELOG.md` section as the release notes, and `generate-platform-release-manifest.sh` / `validate-platform-release-manifest.sh` produce and check the published `platform-release-manifest.json` asset — schema in `platform-release-manifest.md`. That asset is **not** `release-manifest.json`, which is the preflight's committed inputs declaration.

`new-release-signing-key.sh` / `sign-platform-release-manifest.sh` / `verify-platform-release-manifest.sh` are the optional detached release signature — schema in `platform-release-signature.md`, operator procedure in `docs/upgrading.md`. The release job signs only when the `QUASAR_RELEASE_SIGNING_KEY` secret exists; the normative verifier is the updater's, in `control-plane/internal/updater/signature.go`. **No key, public or private, belongs in this repository** — the keygen refuses to write inside the tree, and the contract test generates every key it uses at run time.

`release-cut.sh` (`make release VERSION=x.y.z`, #109) is what pushes the tag that triggers that lane: it moves `CHANGELOG.md`'s `## Unreleased` section into a dated section, commits and tags on `main`, and pushes both, refusing on a dirty/behind tree, a non-semver or not-strictly-newer version, or an empty `## Unreleased`. Its changelog rewrite is exposed as a pure `--transform` mode (stdin in, stdout out, no git) for fixture testing, and doc: `docs/upgrading.md` "Cutting a release".

Run the contract tests by hand (no verify stage runs them):

```bash
for test in scripts/release/test-*.sh; do bash "$test"; done
```

`test-release-publication-gate.sh` (also run by `make verify` and the image workflow release gate) verifies that GitHub Release publication waits
for the control-plane, node-agent and separately promoted updater. The release
manifest still contains only the first two; the release notes advertise all three.
