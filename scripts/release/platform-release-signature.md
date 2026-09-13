# `platform-release-manifest.json.sig` — the detached release signature

The optional second asset of a platform release: a detached signature over the
exact bytes of `platform-release-manifest.json`. A host configured to verify
signatures fetches both from the release and refuses to apply a release whose
signature does not check out.

Signing what it covers: **the manifest, not each image.** The manifest already
names every component digest (`scripts/release/platform-release-manifest.md`),
and a digest is the identity of an image's bytes — so a signature over the
manifest transitively covers the images, and per-image signatures would add a
second thing to verify and nothing to the guarantee. This is ADR 0001's own
observation ("the manifest already carries the digests a signature would
cover"), made load-bearing; the decision record is
`docs/adr/0003-release-signatures.md`.

**Detached, and a separate asset.** A `signatures` field inside the manifest
would be a `format_version` bump, and the manifest's own versioning rule says a
consumer must *ignore* a manifest whose format it does not know — so adding one
would make every already-deployed control plane stop seeing releases. Detached,
the signed bytes are the asset byte for byte, no canonicalisation rule is
needed, and a consumer that has never heard of signing keeps working.

## Example

```json
{
  "format_version": 1,
  "signatures": [
    {
      "algorithm": "ed25519",
      "key_id": "quasar-release-2026",
      "signature": "8bZ0…<base64 of the 64-byte signature>"
    }
  ]
}
```

## Fields

| field | type | grammar | meaning |
|---|---|---|---|
| `format_version` | integer | exactly `1` | The version of **this** document. Not the manifest's, not the release's. A verifier refuses a version it does not know. |
| `signatures` | array | at least one entry | Every signature over the manifest. More than one is the rotation overlap: a release signed by the outgoing and the incoming key verifies on hosts that trust either. |
| `signatures[].algorithm` | string | `ed25519` | An entry in an algorithm the verifier does not know is **skipped**, not fatal, so a second algorithm can be published beside this one. |
| `signatures[].key_id` | string | 1–64 of `[A-Za-z0-9._-]` | A **label**. It picks which trusted key is tried first and it appears in messages; it is never what makes a signature good, because whoever wrote the document chose it. |
| `signatures[].signature` | string | standard base64 of 64 raw bytes | Ed25519 over the manifest asset's exact bytes — the whole file, including its trailing newline. |

No other keys, at either level: the updater decodes with unknown fields
disallowed, so a document carrying one is refused everywhere.

## How it is produced

`scripts/release/sign-platform-release-manifest.sh`, from the `release` job of
`.github/workflows/images.yml`, immediately after the manifest is generated and
validated. The private key is the repository secret
`QUASAR_RELEASE_SIGNING_KEY` (a PKCS#8 PEM) and the label is the repository
variable `QUASAR_RELEASE_SIGNING_KEY_ID`.

**Signing is optional.** With no secret configured the step writes nothing, the
asset is not uploaded, and the release publishes unsigned exactly as before.
The signer verifies its own output with the public half of the key that made it
before exiting 0, so an unverifiable signature is never uploaded.

## How it is verified

The per-host **updater**, in `control-plane/internal/updater/signature.go`,
alongside the registry-namespace allowlist and for the same reason: it is the
last thing between a digest and a `docker compose pull`.

It fetches both assets **itself**, over HTTPS, from
`QUASAR_UPDATER_MANIFEST_BASE_URL` (default the org's releases) using the
`release.version` the apply request carries. Fetching rather than being handed
them is the point: a signature relayed along the same path as the digests would
be supplied by the party it is meant to constrain.

Verification is two halves, and both must pass:

1. **The signature.** At least one entry must verify under at least one key in
   `QUASAR_UPDATER_TRUSTED_KEYS`.
2. **The binding.** The signed manifest must name the version, images and
   digests this request is asking to install. Without this half, one genuine
   release would authorise any digest set at all.

The mode ladder (`off` / `verify` / `require`), the failure semantics, and the
rotation procedure are in `docs/configuration.md` and `docs/upgrading.md`.
`scripts/release/verify-platform-release-manifest.sh` is the same check as a
script, for the pipeline's self-test and for an operator checking a release by
hand; the updater's Go verifier is the normative one.

## Versioning rule

Adding, removing or re-typing any key is a `format_version` bump, and a verifier
must refuse a version it does not know rather than best-effort-parsing it. A new
*algorithm* is not a bump: it is another entry in `signatures`, which existing
verifiers skip.
