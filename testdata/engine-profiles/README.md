# Engine profiles (RH-07, #406)

`profiles.json` is the published table of engine profiles (CONTEXT.md "Engine profile"): for
each platform, container engine and engine mode, whether Quasar calls it **supported**,
**experimental** or **unsupported**, in one sentence why, and for an unsupported one the
profiles to use instead. Decisions D4 and D5 in `docs/rh07/2026-09-28-decisions.md` set the
statuses; the owner's rulings on #406 settle the rest:

- Rootful Docker is supported on every platform, Unraid included, and is never blocked.
- Docker rootless, Podman rootless and Podman rootful are experimental on every Linux
  distribution (owner, 2026-10-01): Docker and Podman behave the same across
  distributions, and testing has been on Fedora (uCore and Workstation).
- Unraid ships only rootful Docker, so its other rows are unsupported, and so is an engine
  the agent cannot name (`unknownEngine`). Unsupported blocks: the quick start generates
  nothing and enrollment refuses the profile by name.

Each reader is held to it by a test:

- the node agent's `runtime_engine` readiness check:
  `node-agent/src/readiness/tests/engine_profiles.rs` probes every row with each of its
  platform's samples;
- the site: `site/src/data/engine-profiles.js` imports this file (`profileFor(platform,
  engine, mode)`), tested by `site/src/data/engine-profiles.test.js`.
- the enrollment script: `deploy/enroll-host.sh` carries this table as shell records between
  its `engine profiles (generated)` markers; `deploy/test-enroll-host.sh` fails when they
  differ (`--write-profiles` regenerates them), reads every sample's `os-release` as its
  platform, and refuses every unsupported row by name.

A status changes here first, in the same change as the agent's `engine_profile()` and the
evidence that justifies it.

## Shape

- `platforms`: the platform ids the quick start uses (`ubuntu` means Ubuntu 24.04 only),
  each with sample hosts. A sample carries the host's `os-release` lines and what each
  engine reports about the OS in its `/info`: Docker's `OperatingSystem` is the host's
  `PRETTY_NAME`, Podman's is its `ID`, and both report `VERSION_ID` as `OSVersion`.
- `profiles`: one row per platform, engine and mode, with `status`, `reason` and
  `alternatives`. An alternative with no `platform` means the same machine.
- `unknownEngine`: the row for an engine that is neither Docker nor Podman.

## How a host is matched

The agent reads the host's `os-release` (mounted at `/host/etc/os-release`) and matches
`ID` or `ID_LIKE`, so Fedora's image-based editions (Silverblue, Bazzite, uCore) are Fedora.
Enterprise Linux, which also lists `fedora` in `ID_LIKE`, is not. Ubuntu, or a system whose
`ID_LIKE` names it, counts only at `VERSION_ID` 24.04; other Ubuntu releases are `other`.

Without that mount the agent falls back to the engine's report, which cannot see a
derivative's family. A sample whose report lands elsewhere says so in `withoutOsRelease`:
Podman on Bazzite reports only `bazzite`, so from the report alone it reads as `other`.

The uCore and Unraid samples, and Podman's `OperatingSystem`/`OSVersion` form, were read
from real hosts. The others follow each distribution's published `os-release`.
