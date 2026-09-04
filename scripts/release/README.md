# scripts/release — release-evidence gates

Release preflight, the supply-chain library and manifest, SBOM generation, image scanning, the Vulkan encoder runtime probe, and their offline contract tests (mock docker). Catalogue: `docs/developer-tooling.md`.

The tag-push release lane (`.github/workflows/images.yml`) also lives here: `changelog-section.sh` extracts a version's `CHANGELOG.md` section as the release notes, and `generate-platform-release-manifest.sh` / `validate-platform-release-manifest.sh` produce and check the published `platform-release-manifest.json` asset — schema in `platform-release-manifest.md`. That asset is **not** `release-manifest.json`, which is the preflight's committed inputs declaration.

Run the contract tests by hand (no verify stage runs them): `bash scripts/release/test-*.sh`.
