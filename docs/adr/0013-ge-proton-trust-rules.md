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

- **Source: github.com release assets of the GE-Proton project only, over HTTPS.** The download URL
  must be a github.com release asset of that project; redirects are followed only to GitHub's own
  release-asset host, and the checksum is what proves the bytes. Any other source is refused.
- **Two ways in, two ways to verify.** The **recommended tool list** ships with each Quasar
  release and carries a pinned digest per release, so the recommended path does not depend on
  fetching a checksum at all. An **arbitrary tag** an admin types is accepted only after the
  download matches the checksum file published in the same GitHub release.
- **The digest is SHA-512.** GE-Proton publishes `<asset>.sha512sum` in GNU format, so the pin is
  SHA-512, not SHA-256 as the spec first said (C00). The agent checks that the file name inside the
  checksum file equals the asset it downloaded. Asset naming changed with GE-Proton 11: take
  `<tag>-x86_64.tar.gz`, falling back to `<tag>.tar.gz` for older tags.
- **Bounded and safe.** A size cap on the download, safe extraction (no absolute paths, no links
  that escape the tool directory), and a disk check *before* download: an install is refused
  when the host lacks the space (a release is roughly 0.5 GB packed and 1.6 GB unpacked).
- **A tool name is a fixed grammar** (`A-Z a-z 0-9 . _ -`, covering Valve's lowercase names with
  underscores and GE's) checked when an admin enters it and again in the Steam wrapper, because a
  name is written into Steam's config.
- **Every enable, install, mapping change and removal is audited**, and admin-only per the
  authorization rules (invariant 6). Install progress and failures appear in Fleet ▸ Jobs.

## What this does and does not protect

A checksum file in the same release as the asset proves the download was not truncated or altered
in transit and is the file the release published. It does **not** prove the release is benign: whoever
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
