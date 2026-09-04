---
status: accepted
date: 2026-09-04
---
# A platform release is trusted by its pinned digest, not by a signature

Quasar can pull and apply its own platform releases (control plane, node agent) from the
org's public registry. What proves a release is genuine before a host pulls it: the
registry's TLS plus the sha256 digest the control plane resolved from the release
manifest, pinned end to end so an agent pulls exactly the digest the control plane saw
and never a floating tag. No signature verification (cosign or similar) in v1.

## Considered options

- Digest pinning only (chosen): the resolver, HTTPS-only client, and registry allowlist
  already exist for catalog images; a digest is immutable and the same one is what the
  release manifest, the admin UI, and the host all name.
- Digest pinning plus manifest signatures: stronger against a compromised registry
  account, but adds a signing key to the release path, a verifier to the agent, and a
  key-rotation story none of which exists yet. Filed as a follow-up, not a v1 gate.

## Consequences

- A tag is never an identity anywhere in the release path; anything that names a
  release image names a digest.
- Adding signatures later changes the verifier, not the shape: the manifest already
  carries the digests a signature would cover.
