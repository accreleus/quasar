---
status: accepted
date: 2026-09-04
---
# Releases apply control plane first, and the UI never offers a downgrade

The control plane runs its schema migrations forward on boot and cannot start against a
database that is ahead of it, so a control-plane image can never be rolled back below
the database's applied migration version without manual surgery. Applying a platform
release therefore always updates the control plane first, then agents; the admin UI never
offers a release older than the installed control plane; and agents may be updated
independently but never to a release newer than the control plane.

## Considered options

- Ordered, no-downgrade (chosen): the one-way migration rule already exists as an
  operating rule (`docs/upgrading.md`), and this makes the UI unable to violate it.
- Any direction, operator's responsibility: a version picker is what a reader expects,
  and is exactly the path that crash-looped two fleet stacks during AS10-03.

## Consequences

- "Revert" exists only for agents, which carry no migrations, and only back to the
  control plane's release or older.
- A mixed fleet (control plane ahead of some agents) is a normal transient state, which
  the additive agent-api discipline already permits.
- A host whose agent is newer than the control plane is a fault the release surface
  reports, not a state it can create.

## Note (2026-09-25, RH06, #353)

The decision above is unchanged. ADR 0008 records one exception to "never ahead of the control
plane" for the RH06 recovery actor (decision A1): on the control plane's own machine the
recovery actor may lead the control plane while a control-plane replacement is in flight, or
after one failed and was restored. The actor carries no database schema, which is what this
rule protects; agents are unaffected.
