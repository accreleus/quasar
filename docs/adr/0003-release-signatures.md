---
status: accepted
date: 2026-09-08
---
# A platform release may carry a detached ed25519 signature, verified by the updater

> *RH06-15 (#367):* the verifier is now the recovery actor's port of this one
> (`node-agent/crates/quasar-recovery/src/trust`, held to the frozen answers in
> `testdata/recovery/trust-vectors`); the Go updater that implemented it is retired. The
> decision below is unchanged.

> *#621 (0.6.4), amending "Default off" and "`verify` is the transition rung" under
> Consequences:* signing is now **on by default**. Every release is signed by the org key
> `quasar-release-2026`, held as a secret of the `release` GitHub environment, which only a
> run on a `v*` tag can use; the recovery actor compiles in its public half as the default
> `QUASAR_UPDATER_TRUSTED_KEYS` and defaults `QUASAR_UPDATER_SIGNATURE_MODE` to `verify`.
> One rule changes in `verify`: a request that names a version and finds no signature (a 404)
> is refused `signature_missing`, so deleting a release's `.sig` no longer
> downgrades it to unverified. A request that names no version is still applied with the WARN,
> and the rest of the "not an enforcement boundary" consequence stands for it. A machine that
> recorded a mode keeps it; one that cannot reach the release host directly over HTTPS runs
> `off` (`docs/upgrading.md` "Signing platform releases"). A blank
> `QUASAR_UPDATER_TRUSTED_KEYS` now means the org key, never an empty list, so the "no trusted
> keys refuses every apply" consequence below can no longer arise; an explicit list replaces the
> default. `signature_missing` and `signature_invalid` are now emitted on a default machine, not
> only after an operator turns verification on. Only a run on a `v*` tag can use the `release`
> environment, and a tag release of the org repository with no key fails rather than publishing
> unsigned.

ADR 0001 chose digest pinning alone and filed signatures as a follow-up, noting
that "the manifest already carries the digests a signature would cover" and that
adding them later "changes the verifier, not the shape". This is that follow-up.
It adds the format and the verifier and ships them **off by default**: turning
signing on requires a signing key in the release pipeline, which is an operator
action.

## What is signed

**The release manifest, and nothing else.** The manifest names every component
image and its `sha256:` digest; a digest is the identity of an image's bytes, so
a signature over the manifest transitively covers every image in the release.
Per-image signatures (cosign-style, in the registry) would add a second artifact
to fetch and verify, and a second failure mode, for a guarantee already implied
by the first. Signing the manifest is sufficient.

The signature is **detached**, published as a second GitHub Release asset,
`platform-release-manifest.v2.json.sig`. Not a field inside the manifest: the
manifest's versioning rule requires a consumer to *ignore* a manifest whose
`format_version` it does not know, so adding a field would be a bump that makes
every already-deployed control plane stop seeing releases. Detached, the signed
bytes are the asset byte for byte, no canonicalisation rule is needed, and every
existing consumer keeps working. Format:
`scripts/release/platform-release-signature.md`.

## Considered options

- **Detached ed25519 over the manifest, public keys in host configuration
  (chosen).** `crypto/ed25519` is in the Go standard library — no dependency, no
  external service, no trust root to keep fresh, no algorithm parameters to get
  wrong. Signing is `openssl pkeyutl -sign -rawin`, available on any host and on
  every CI runner. Verification is offline apart from fetching the two assets.

- **Sigstore / cosign, keyless.** Stronger provenance on paper: the signature is
  bound to a workflow identity and logged in a transparency log. The cost is a
  live dependency on Fulcio, Rekor and a TUF trust root that must be refreshed,
  plus an identity policy to express and maintain — inside a per-host updater
  that in some installs cannot reach the public internet at all. For a
  self-hostable product whose operator may also want to sign their own fork's
  releases, that is a large amount of moving infrastructure to make one
  signature check work. Rejected for now. A verifier skips a `signatures`
  entry whose `algorithm` it does not know, so a second algorithm that fits the
  existing entry shape (`algorithm`, `key_id`, `signature`) can be published
  beside ed25519 without a format bump. Anything that needs a new field, such as
  a certificate or a transparency-log bundle, does need one: the verifier
  refuses unknown fields on purpose (a signature format that ignores what it
  does not understand is the weaker choice), so a new field is a
  `format_version` bump that older verifiers refuse.

- **cosign with a key pair.** Closer, but `cosign sign-blob --key` is an
  ECDSA/ed25519 detached signature with extra packaging, and the registry-side
  variant (`cosign sign` on the images) attaches signatures to a derived *tag*,
  which sits badly with ADR 0001's "a tag is never an identity". A binary in the
  updater image and a second format bought nothing over the stdlib.

## Where the check lives

**In the updater**, beside the registry-namespace allowlist it already enforces,
because that is the last point between a digest and a `docker compose pull`.

The updater fetches the manifest and the signature **itself**, over HTTPS, from
a base URL that is host-local configuration, using the `release.version` the
apply request already carries. It does not accept them from the caller: a
signature relayed along the same path as the digests would be supplied by the
party it is meant to constrain. Two halves must both pass — the signature must
verify under a trusted key, and the signed manifest must name the version,
images and digests this request is asking to install. Without the second half a
single genuine release would authorise any digest set.

This also needs no change to `agent-api.md`: `release_apply` already carries
`release.version`, which is all the URL needs.

## Rotation

`QUASAR_UPDATER_TRUSTED_KEYS` is a list, and the signature document is a list.
A release can be signed by the outgoing and the incoming key at once, and a host
can trust both, so the two sides roll independently and no release and no host
has to move on the same day. The `key_id` is a label: it picks which key is
tried first and it appears in messages, but any signature by any trusted key
verifies, because whoever writes the document chooses the label.

## Consequences

- Default off. An existing install applies releases exactly as it did.
  *(Superseded by the #621 amendment at the top: on by default.)*
- `verify` is the transition rung: a bad signature is refused, a release that
  publishes none is not. `require` closes it.
  *(Superseded by the #621 amendment at the top: a named release with no signature is now refused.)*
- **`verify` is not an enforcement boundary, and must not be described as one.**
  The version whose signature the updater goes looking for comes from the apply
  request, so the party being constrained chooses it. A request naming no
  version, or one never published, produces "no signature exists" — which
  `verify` applies. A compromised control plane therefore bypasses `verify`
  entirely without touching the network. A null version is not even anomalous:
  edge releases carry one, and so does a revert with no release id. `verify`
  buys exactly one thing — it catches a *signed* release that has been tampered
  with — and it exists so a fleet can turn signing on while unsigned releases
  are still in flight. `require` is the boundary. Every unverified apply under
  `verify` logs a WARN naming the version, so the gap is visible from outside
  the host rather than silent.
- Under `require`, the digest set is bound to a signed manifest, but nothing
  binds it to a *recent* one: a compromised control plane can still install an
  older genuine signed release, including a known-vulnerable one. Reverts are a
  feature, so this is inherent rather than an oversight — but it is the limit of
  what signing alone buys.
- Fail closed on ambiguity. A fetch that could not be completed is never read as
  "unsigned", and a host told to verify with no trusted keys refuses every apply
  rather than checking nothing. (Since #621 a blank key list is the org key, so a
  host can no longer be left with no trusted keys.)
- Under `require`, a release with no `version` — an edge build, or a revert to a
  build the instance can no longer name — is refused. Reverting to such a build
  needs the manual recipe or a temporary drop to `verify`.
- The updater gains an outbound HTTPS dependency, but only when verification is
  on.
- Two new failure identifiers, `signature_missing` and `signature_invalid`,
  reach the admin UI. They are not yet in `openapi.yaml`'s `ApplyFailureReason`
  enum; adding them is an additive amendment needing sign-off, and until then
  they travel the contract's documented path for an identifier a consumer does
  not recognise (stored and rendered verbatim). Nothing emits them until an
  operator turns verification on.
