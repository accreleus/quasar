---
status: accepted
date: 2026-11-06
---
# GE-Proton trust rules

A compatibility tool is code the host runs inside every session that uses it, and GE-Proton is a
community build Quasar does not produce. We decided the rules under which a host will download and
run one (spec #1063, research #1064, decided with the owner on 2026-11-06). They are an **explicit
exception** to the Steam preparation policy's rule that the agent never accepts arbitrary
executable commands, host paths or credentials (`protocol/agent-api.md`), so they carry their own
normative contract text. Tool structure is ADR 0012.

## The rules

- **The project and the URL.** The only source is the GE-Proton project's own GitHub releases,
  `GloriousEggroll/proton-ge-custom`. The wire carries **only the release tag**; the control plane
  and the agent build the download URL themselves (`https://github.com/GloriousEggroll/proton-ge-custom/releases/download/<tag>/<asset>`)
  and never accept a URL, host or path from an admin or a peer.
- **HTTPS on every hop, redirects on an allowlist.** Redirects are followed only to an exact
  allowlist of GitHub's release-asset hosts (`release-assets.githubusercontent.com`,
  `objects.githubusercontent.com`; the contract fixes the list), at most 5 hops, with no credentials
  forwarded. A redirect anywhere else, or to plain HTTP, refuses the install.
- **Two ways in.** The **recommended tool list** ships with each Quasar release and carries the
  **SHA-512** per release, so the recommended path fetches no checksum at all. For an **arbitrary
  tag** an admin types, the **control plane** fetches the release's `<asset>.sha512sum` once
  (size-capped, under the same HTTPS, redirect-allowlist and hop rules as the tarball), checks that
  the file name inside it equals the asset, and records the SHA-512 in
  the fleet's desired state. Every agent then verifies its tarball against that recorded value, so
  every host installs identical bytes and no host fetches or trusts a checksum itself. A tarball
  that does not match the pinned SHA-512 fails that host's install: no retry from another source,
  no re-fetch of the checksum, and the failure shows in Fleet ▸ Jobs with the expected and actual
  digests.
- **The digest is SHA-512.** GE-Proton publishes `<asset>.sha512sum` in GNU format, so the pin is
  SHA-512, not SHA-256 as the spec first said (C00). Asset naming changed with GE-Proton 11: take
  `<tag>-x86_64.tar.gz`, falling back to `<tag>.tar.gz` for older tags.
- **Extraction is bounded and confined.** The archive is unpacked into a temporary directory, under
  caps on the download size, the unpacked bytes and the entry count, and moved into place with an
  atomic rename. Only regular files, directories and symlinks that resolve inside the tool
  directory are kept; `..` and absolute paths, hardlinks, device nodes and FIFOs are rejected;
  setuid and setgid bits are cleared. A disk check runs *before* download and refuses an install
  the host lacks the space for (a release is roughly 0.5 GB packed and 1.6 GB unpacked).
- **A tool name is a fixed grammar.** The first character is `[A-Za-z0-9]`, the rest
  `[A-Za-z0-9._-]`, at most 64 characters, and `.` and `..` are refused. That covers Valve's
  lowercase underscored names and GE's. It is checked when an admin enters a name and again in the
  Steam wrapper, because a name is written into Steam's config. The `compatibilitytool.vdf` key
  that Steam will use as the tool's name is checked with the same grammar, and an install is
  refused if that key collides with a Valve tool name (`proton_*`).
- **Every enable, install, mapping change and removal is audited**, admin-only per the
  authorization rules (invariant 6). An audit entry records the actor, the tag, the SHA-512 and the
  hosts. Install progress and failures appear in Fleet ▸ Jobs.

## What this does and does not protect

A checksum file in the same release as the asset proves the download was not truncated or altered
in transit and is the file the release published, and recording it once makes every host agree. It does **not** prove the release is benign: whoever
controls a release controls both the asset and its checksum. That is why the recommended list is
pinned in a Quasar release (the digest travels inside the release, not fetched), why
an arbitrary tag is an admin's deliberate act on the audit log, and why uploading arbitrary tool
tarballs is out of scope.

## Considered options

- **Any URL with an admin-supplied checksum.** Provenance becomes whatever the admin pasted; the
  rule "GE-Proton project releases only" is simple to state and to audit.
- **Pinned recommended list only.** Safest, but an admin could not use a GE-Proton release that
  fixes their game until the next Quasar release.
- **Trust the download without a checksum.** Rejected: a truncated or tampered download would run
  every user's games.
