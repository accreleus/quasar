# RH-06 console surfaces — mockups (#354)

**Status: awaiting owner approval (#354).** Until the owner's approval is recorded on
#354, no RH-06 slice may land the UI part of a surface below. Non-UI work can go ahead.

- Mockup: [`../fleet-rh06-v3.html`](../fleet-rh06-v3.html). It is a standalone reference
  page, built the same way as `releases-v3.html`. Each specimen is one surface in one state.
- Section renderer: [`../assets/pages-rh06.js`](../assets/pages-rh06.js). It uses the
  `console-v3.css` classes and the `ui.js` helpers (`head`, `tabs`, `chip`, `sdot`, `bar`,
  `menu`, `tableCard`, `icon`).
- Screenshots: this directory, 41 specimens plus two narrow captures, about 4 MB in
  total. Each PNG is one specimen cropped to its element. Desktop PNGs are 1080 px wide
  (a 1440 px viewport; the page is 1160 px wide with 40 px padding). The `narrow-*` PNGs
  are 820 px wide (a 900 px viewport). They were rendered with headless Chromium
  (Playwright 1.49.1) from the HTML above, at device scale 1.
- Product authority: `docs/rh06/2026-09-24-decisions.md` (D1–D17, R1, A1–A3; R1 overrides)
  and `docs/rh06/2026-09-24-architecture.md`. The specification is #352.

Machine names, versions, digests, addresses and the enrollment string are placeholders.
The stack snippet's field names are illustrative until RH06-07 fixes them (D11).

The sample fleet is one consistent story. Where specimens name the same machine, they
agree with each other.

| Machine | Shape | Story |
|---|---|---|
| living-room-pc | Combined host | runs the control plane; database is Quasar's own; seed declared in an external manager |
| gpu-host-2 | GPU host | no seed found |
| gpu-host-3 | GPU host | node agent and recovery actor below the floor |
| gpu-host-4 | GPU host | node agent older than the control plane; seed started by the one-line command; the Remove host specimens |
| study-pc | GPU host | owner conflict |
| gpu-host-5 | GPU host | offline |
| gpu-host-6 | GPU host | just enrolled from Add host (the "not reported yet" specimen) |
| attic-server | Control-only host | a separate install using the operator's own database (Releases ▸ Installed and the migrating Developer apply variants only) |

## Where each surface lives

| Surface | Placement |
|---|---|
| Service inventory | Host page: a new card, "Services on this machine". Fleet ▸ Hosts: a new Services column in the expanded row. Fleet ▸ Releases ▸ Installed: a "This machine" block. A control-only host has no host row, so its inventory appears only there. |
| Seed-missing / owner-conflict warnings | Host page, above the services card, as a `.note.warn`. A chip on the host's row in the table. The owner conflict also appears as a Targets reason on Releases. |
| Must update before it can be managed | Host page `.note.warn` with the only action, Update. A "must update" chip on the row. Its row menu offers only Open host, Update, Drain and Remove host. Targets on Releases lists it as "included". |
| Add host | Dialog opened from Fleet ▸ Hosts. The **Enroll host** button is renamed **Add host** (D10/D13). |
| Backup on a migrating update | The Update Quasar dialog. The refusal shows as the Releases banner. |
| Restore command | Replaces the Releases update banner after a failed migrating update. The failed attempt also appears in Apply history. |
| Remove host | The GPU host's services card footer (danger button, with the image-detail pattern's hint), plus the existing row-menu item. Opens a confirmation dialog. |
| Developer apply | A Releases rail card that opens a drawer, in the drawer pattern of `pages-library.js`. |

## Surface → state → screenshot

| # | Surface | State | Screenshot |
|---|---|---|---|
| – | Fleet ▸ Hosts (context) | normal: shapes, attention chips, Services column, Add host | [`hosts.png`](hosts.png), [`narrow-hosts.png`](narrow-hosts.png) |
| 1 | Service inventory | normal: combined host, Quasar's own database | [`inv-combined.png`](inv-combined.png) |
| 1 | Service inventory | normal: GPU host (no database, no control plane), agent older than the control plane | [`inv-gpu.png`](inv-gpu.png) |
| 1 | Service inventory | normal: control-only host, operator's own database (Releases ▸ Installed; version, owner and state per service) | [`inv-control-only.png`](inv-control-only.png) |
| 1 | Service inventory | unknown: control-only host, not reported yet | [`inv-control-only-unknown.png`](inv-control-only-unknown.png) |
| 1 | Service inventory | error: control-only host, recovery actor not answering | [`inv-control-only-error.png`](inv-control-only-error.png) |
| 1 | Service inventory | unknown: not reported yet | [`inv-unknown.png`](inv-unknown.png) |
| 1 | Service inventory | error: recovery actor not answering (last report shown with its time) | [`inv-error.png`](inv-error.png) |
| 2 | Seed-missing warning | normal | [`seed-missing.png`](seed-missing.png) |
| 2 | Seed-missing warning | unknown: not checked yet | [`seed-unknown.png`](seed-unknown.png) |
| 2 | Owner-conflict warning | normal, with Details open (the only place `owner_conflict` appears) | [`conflict.png`](conflict.png) |
| 2 | Owner-conflict warning | error: still in the way after Check again | [`conflict-error.png`](conflict-error.png) |
| 3 | Must update before it can be managed | normal: node agent and recovery actor below the floor | [`floor.png`](floor.png) |
| 3 | Must update before it can be managed | normal: recovery actor only (ends no sessions) | [`floor-actor.png`](floor-actor.png) |
| 3 | Must update before it can be managed | unknown: version not reported | [`floor-unknown.png`](floor-unknown.png) |
| 3 | Must update before it can be managed | error: update failed; the recovery actor moved first and stays on the new release, and the node agent was put back | [`floor-failed.png`](floor-failed.png) |
| – | Fleet ▸ Releases (context) | normal: migrating release, Installed inventory, Targets reasons, Developer apply card | [`releases.png`](releases.png), [`narrow-releases.png`](narrow-releases.png) |
| 4 | Add host | normal: one-line command tab (default), before creating; node name and expiry | [`add-options.png`](add-options.png) |
| 4 | Add host | normal: pinned-key one-liner created | [`add-ready.png`](add-ready.png) |
| 4 | Add host | normal: "Using Dockge or Arcane? Paste this stack instead" | [`add-stack.png`](add-stack.png) |
| 4 | Add host | unknown: certificate not read yet | [`add-loading.png`](add-loading.png) |
| 4 | Add host | error: could not create the command | [`add-error.png`](add-error.png) |
| 4 | Add host | error: page not on HTTPS (kept from today's dialog) | [`add-http.png`](add-http.png) |
| 5 | Migrating update: backup | normal: Quasar's own database (dump first, refused without it) | [`update-own.png`](update-own.png) |
| 5 | Migrating update: backup | normal: operator's own database, not confirmed (Update disabled) | [`update-external.png`](update-external.png) |
| 5 | Migrating update: backup | normal: operator's own database, confirmed | [`update-external-checked.png`](update-external-checked.png) |
| 5 | Migrating update: backup | unknown: free space not reported | [`update-unknown.png`](update-unknown.png) |
| 5 | Migrating update: backup | error: not enough free space | [`update-space.png`](update-space.png) |
| 5 | Migrating update: backup | error: refused after starting, dump failed (Releases banner); the control plane was not replaced and the recovery actor is already on the new release (A1) | [`update-refused.png`](update-refused.png) |
| 6 | Failed migrating update: restore | normal: Quasar's own database; the command names the dump, the copy names the version it returns to | [`restore-own.png`](restore-own.png) |
| 6 | Failed migrating update: restore | unknown: dump not reported yet | [`restore-unknown.png`](restore-unknown.png) |
| 6 | Failed migrating update: restore | variant: operator's own database (**PROPOSED**, see open question 1) | [`restore-external.png`](restore-external.png) |
| 7 | Remove host | normal: confirmation | [`remove-confirm.png`](remove-confirm.png) |
| 7 | Remove host | normal: in progress (waiting for sessions) | [`remove-progress.png`](remove-progress.png) |
| 7 | Remove host | unknown: host not connected | [`remove-offline.png`](remove-offline.png) |
| 7 | Remove host | error: removal did not finish | [`remove-failed.png`](remove-failed.png) |
| 8 | Developer apply | normal: filled | [`devapply.png`](devapply.png) |
| 8 | Developer apply | empty | [`devapply-empty.png`](devapply-empty.png) |
| 8 | Developer apply | error: namespace refused, tag instead of digest | [`devapply-error.png`](devapply-error.png) |
| 8 | Developer apply | normal: control-plane digest that migrates, Quasar's own database (dump first, stop if it cannot be taken) | [`devapply-migrating.png`](devapply-migrating.png) |
| 8 | Developer apply | normal: control-plane digest that migrates, operator's own database (confirmation required) | [`devapply-migrating-external.png`](devapply-migrating-external.png) |

## Surface → implementing RH-06 ticket

Assignments follow each ticket's "What to build" and acceptance lines.

| Surface | Ticket | Why |
|---|---|---|
| Service inventory: GPU host (installed, unknown and older-agent states) | #357 RH06-05: Recovery actor installs and reports a GPU host's agent | "The host card shows installed, unknown and older-agent states per the mockup." |
| Service inventory: combined and control-only machines, database mode (Quasar's own / operator's own) | #361 RH06-09: Combined and control-only install | "The inventory matches the approved mockup"; "shows database mode external". |
| Seed version, "not found" row | #358 RH06-06: Seed mode and first-install bootstrap | "The console displays the seed version or 'no seed found' per the mockup." |
| Recovery-actor version change; actor-first ordering in Targets | #362 RH06-10: Recovery actor replaces itself | "The console shows the actor's version change." |
| Seed-missing warning | #366 RH06-14: Race guard, remove host, uninstall and reconfigure | "Console warnings … match the mockups." The seed state itself comes from #358. |
| Owner-conflict warning (host page, row chip, Releases Targets reason) | #366 RH06-14 | Race guard: "raised as `owner_conflict`, and blocks eligibility". |
| Must update before it can be managed | #365 RH06-13: Publish format-2 releases with floors and new edge tags | "`below_floor` is evaluated by the pure planner and shown per the mockup." |
| Add host dialog (both tabs) | #359 RH06-07: One-line enrollment command and Add host | "The modal matches the approved mockup." |
| Backup on a migrating update (dialog, refusal banner) | #364 RH06-12: Migrating update: dump, restore and external-backup confirmation | "Console states match the mockup." |
| Failed migrating update: restore command | #364 RH06-12 | Same ticket: the printed `restore` command. |
| Releases view on an owned install (non-migrating update) | #363 RH06-11: Control-plane replacement without a migration | "The Releases view matches the mockup." |
| Remove host (confirmation, progress, offline, failure) | #366 RH06-14 | "Console warnings and remove host match the mockups." |
| Developer apply | #360 RH06-08: Agent replacement through the relay, and developer apply | "Developer apply is admin-only … UI matches the mockup." |
| Developer apply of a migrating control-plane digest (backup rule) | #360 RH06-08 for the drawer; #364 RH06-12 for the dump and confirmation it reuses | The same Decision 14 database rule as a migrating release. |

Tickets with no console surface of their own:
- #353 RH06-01 (contracts). It supplies the identifiers these surfaces read: `below_floor`,
  `owner_conflict`, `backup_failed`, `backup_unconfirmed`, `external_backup_confirmed`,
  install mode `owned`, and the actor and seed identity fields.
- #355 RH06-03 and #356 RH06-04 (runtime crate, trust-gate port).
- #367 RH06-15. It retires the Compose updater, so the Releases preflight labels for
  `updater_stack_dir` / `updater_overlays` go away with it.
- #368 RH06-16 (acceptance).

## Copy and identifiers

- The wording uses `CONTEXT.md` terms: Seed, Recovery actor, Service owner, External
  manager, Combined host / GPU host / Control-only host, Enrollment, Attempt, Fleet run,
  Replacement, Platform release.
- What the operator acts on appears in the copy: the commands they run (in copyable
  blocks) and the name of a container they must remove. Wire identifiers — check ids
  (`owner_conflict`), eligibility and failure reasons (`below_floor`, `backup_failed`),
  attempt ids, container labels — appear only under a closed **Details** disclosure
  (`.diag`). This follows how the audit log treats key/value detail.
- Stated wherever it applies: migrating updates are never applied unattended (D14), and an
  agent update ends that host's sessions (D9). A recovery-actor-only update ends none.
- Every update moves the machine's recovery actor first (Decision 13). So a failure at a
  later step (a dump refused, a node agent that never became healthy) leaves the actor on
  the new release. The copy says so; it does not claim "nothing changed". On the control
  plane's own machine, the actor running one release ahead is expected (A1).
- The glossary term is **External manager**. The Add host tab is labelled "Dockge or
  Arcane", the two managers RH-06 supports, and its body opens with the spec's line,
  "Using Dockge or Arcane? Paste this stack instead."

## What is new to the handoff, and why

The handoff has no centred dialog, copyable-command block or diagnostic disclosure. These
are composed from existing tokens and parts. The CSS lives in the `<style>` of
`fleet-rh06-v3.html`, with a comment for each block:

| Class | Built from | Product equivalent |
|---|---|---|
| `.modal`, `.modal-head/-body/-foot` | the drawer's `dw-head/-body/-foot` spacing on a `--r-feature` panel with the palette's glass | `.modal` in `web/src/styles/primitives.css` (same derivation) |
| `.snippet` | `--surf-inset`, `--line`, `--r-control`, mono `--t-xs` | `.enroll-snippet` in `web/src/styles/admin/fleet.css` |
| `.diag` | the `.rel-e` disclosure from releases-v3 and the `.aud-pre` mono readout | none yet |
| `.stage`, `.spec*` | mock framing only: a stand-in scrim (the `.dscrim` colour, `oklch(0.05 0.01 267/.52)`) and gallery captions | not to be ported |

**Flag: no pattern to borrow.** The centred dialog has no handoff precedent. The product's
existing `Modal` is the closest match, and this follows it.

## Visual verification

The mockup and the existing mocks were rendered with the same headless Chromium in a
1440 px viewport and compared side by side:

- Fleet ▸ Hosts against `admin-console-v3.html#/fleet/hosts`.
- The host page against `#/fleet/hosts/c2059601`.
- The drawer against the runtime-preset drawer (`#/library/presets`).
- Releases against `releases-v3.html` / `releases-v3.png`.

Typography (IBM Plex Sans/Mono, the `--t-*` scale), table heads, rows and bars, tabs,
segmented controls, buttons, notes, cards, the drawer's `fsec` grid and the rail cards all
match. They are the same classes. The key surfaces were also rendered in a 900 px viewport, and the
cards wrap as the existing mocks do.

Deviations and observations:
- **State chips render neutral in every v3 mock.** In `console-v3.css` a later
  `.chip{background;border-color;color}` rule overrides the earlier `.chip-warning`,
  `.chip-success` and so on at equal specificity. `releases-v3.png`'s PRE-RELEASE chip is
  grey for this reason. The RH-06 chips keep their intended `chip-warning` class and render
  the same way. This was left alone: fixing it restyles every existing mock.
- On Releases the split grid stays two columns in a 900 px viewport, because the split is inline, as
  in releases-v3.

## Data the mock assumes — needs a source in RH06-01 or the implementing slice

The mock shows these values, but no approved contract field carries them yet. Each needs
a field in the RH06-01 amendment (#353), or the implementing slice must supply it (or drop
it from the UI).

| Value shown | Where | Likely owner |
|---|---|---|
| The operator's database address (`db.example:5432`) and a "reachable" state | inventory (host page, Releases ▸ Installed), backup dialog, restore variant | #361 / #353 (inventory status) |
| Quasar's own Postgres version ("Postgres 16.4") | host-page inventory | #361 / #353 |
| Per-service state (running / not found / as of HH:MM) and the time of the last report | every inventory specimen | #357 / #361 / #353 (status shape) |
| The seed's owner kind (external manager vs `docker run`) | inventory Owner column | #358 (see open question 4) |
| Machine shape per host (Combined / GPU / Control-only) | Hosts rows, inventory | #357 / #361 / #353 (host body) |
| The dump's size and the machine's free space ("about 1.4 GB; 212 GB free"; "0.6 GB is free") | backup dialog, Developer apply | #364 / #353 (preflight `backup_space`) |
| The dump's name and the time it was taken | restore card | #364 / #353 (attempt dump reference) |
| That a *developer-apply* control-plane digest migrates, and its schema step (88 → 91) | Developer apply migrating states | #360 / #364 (image-carried schema version) |
| Allowed namespaces listed "set on each machine" | Developer apply | #360 (the allowlist lives with the actor; the console needs to read it) |
| Removal progress (sessions still to end, longest duration, who started it) | remove-progress | #366 |
| "Seed not checked yet" (the actor's seed check has not run since it restarted) | seed-unknown | #358 / #366 |
| Per-component outcomes inside one attempt (recovery actor succeeded, node agent restored) | floor-failed, update-refused Details | #360 / #363 / #353 (`release_state` components) |

## Open questions for the owner

1. **The restore commands — both PROPOSED, not spec. Decide in #364 / #352.**
   - *Quasar's own database* (`restore-own`). The mock uses the architecture's shape: the
     seed image run with `restore --dump <dump>`
     (`… quasar-recovery@sha256:… restore --dump 2026-09-25T1402Z-schema-88`). It names
     the dump but not the version. The version it returns to (v0.5.2, the release the dump
     was taken under) is stated in the copy beside it. #354 asks for "the exact one-line
     restore command naming the dump and the version it returns to". **Decide** whether
     the command itself must also carry the version (for example a `--to <version>`
     flag). If so, the actor has to refuse a version that does not match the dump.
   - *Operator's own database* (`restore-external`). D7/R1 define `restore` only against
     Quasar's own dump. The mock proposes: the operator restores their own backup with
     their own tools, then runs `restore --to <version>`. That starts the named control
     plane only if the database's schema matches it, and refuses otherwise. **Neither the
     flag nor this flow is in the spec.** Confirm it, replace it, or state that this case
     prints no command.
2. **Removing an offline GPU host.** The mock refuses (Remove disabled, with an
   explanation), because the actor must be reachable to remove containers, and R1 cut
   "forget this host's credentials". Confirm, or specify what should happen.
3. **"Offered only an update" (D9).** D9's wording restricts only release actions: "is
   offered only an update" (so no revert). **The mock goes beyond that.** It also hides
   Settings and Local console (RH-05 host policy) while a host is below the floor, on the
   reading that "cannot be managed" covers them too. Drain and Remove stay available.
   Confirm the wider reading, or narrow it to release actions only. That would restore
   Settings and Local console in `floor*.png`.
4. **Seed owner display.** The mock shows "External manager" for a stack-managed seed and
   "You (docker run)" for one started by the one-line command. This assumes the recovery
   actor can tell the two apart (for example from the seed's container labels). If it
   cannot, one label ("External manager") covers both.
5. **Add host rename.** Today's "Enroll host" becomes "Add host" (D10/D13 wording). The
   dialog's first action is "Create command" (today: "Mint enrollment string").
