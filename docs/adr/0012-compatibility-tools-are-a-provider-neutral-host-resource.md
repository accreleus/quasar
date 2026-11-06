---
status: accepted
date: 2026-11-06
---
# Compatibility tools are a provider-neutral host resource

A **compatibility tool** (`GLOSSARY.md`) is what runs a Windows game on a Linux host: today a
Proton build, either Valve's or GE-Proton. We decided that Quasar models it as a **host resource
that knows nothing of Steam**: an identity (kind `proton`, family `valve` or `ge`, version or tag),
a source, and a per-host install state. A **library provider** binds an installed tool its own way.
The Steam provider writes Steam's per-game mapping; a later launcher provider would point its own
Proton variable at the same directory. Installing GE-Proton once serves every provider. Spec #1063,
research #1064, decided with the owner on 2026-11-06. The trust side is ADR 0013.

## Why not leave it to Steam

Steam in each user's home picks its own default, so a game that needs a particular Proton can only
be fixed by streaming the game, opening Steam's properties and changing it once per user, and the
choice is lost when the user's home is replaced. Nothing in Quasar could install GE-Proton on a
host at all. Valve's own builds are hidden from the library as clutter, and a host had no shared
place a tool could live.

## What the decision commits us to

- **Valve's builds are never installed by Quasar.** They are Steam apps each home downloads itself;
  Quasar only maps a game to them. The library denylist that hides them from game tiles stays.
- **GE-Proton is fleet-wide desired state.** Enabling a release makes it desired on every host; each
  agent converges and reports its own install state, and a host offline during the install catches
  up on reconnect. There is no per-host tool selection. Installs use the existing host-scoped jobs
  framework (modelled on the template warm-up job), so no new agent message shape is needed; the
  agent API gains normative text only for the narrow download-and-run exception in ADR 0013.
- **Placement treats "tool not installed here" as an ineligibility reason.** This is required, not
  tidy: C00 showed Steam does not validate a mapping, and a mapping to an absent tool silently
  launches the Windows executable with no tool at all. A launch no host can serve because the tool
  is still installing says so.
- **Tools live in an agent-owned directory mounted read-only into sessions**, outside the home root
  (C00: a preset mount under the home root is refused as ambiguous), with Steam's extra
  compatibility-tools path variable pointing at it. Steam names a tool by the key in the tarball's
  `compatibilitytool.vdf` (`GE-Proton11-12-x86_64`, arch suffix included), not by the release tag,
  so Quasar stores and maps by that Steam name.
- **Mappings are keyed by the game's Steam appid, never the per-user derived tile.** There is a
  fleet default (nullable) and a per-game mapping with a `forced` flag. Proton prefixes stay in
  each user's own home whichever tool runs the game.
- **The mapping fills only where the user has chosen nothing, unless forced.** Steam writes a
  user's own pick in exactly the shape an admin fill has (`priority "250"`), so "absent" cannot
  tell them apart after the first fill. The Steam wrapper therefore keeps a sidecar record of what
  it wrote and treats any entry that differs from it as the user's choice.
- **The binding is written before Steam starts, and only then.** Steam never re-reads the file and
  overwrites an edit made while it runs, and a stopping Steam rewrites its old mapping on exit. A
  hot-loaded (parked) Steam started before a change is therefore superseded, and the stop finishes
  before the wrapper writes.
- **The data model must not assume Steam.** Non-Steam providers are not built in 0.6.

## Considered options

- **Per-game choice in Steam only (status quo).** Per user, lost with the home, no install path.
- **Admin picks a tool per host.** Makes every placement and mapping reason about which host has
  what; a fleet that converges on one set is simpler and a host that lags is simply ineligible.
- **Users choose in the Quasar UI.** Out of scope; they can still choose inside Steam.
- **Bake the mapping into the home template.** Cloned homes would inherit a stale choice; the
  mapping is applied at launch instead.
- **Install GE-Proton through Steam.** GE-Proton is not a Steam app.
