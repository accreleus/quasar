---
status: accepted
date: 2026-09-25
---
# Container shapes are compiled into the recovery actor as recipes, and the actor moves first

On an owned machine the recovery actor creates every platform service's container through the
Engine API. Something has to say what each container looks like — its mounts, environment
inputs, ports, devices and capabilities — and a release changes the image, so the shape and the
image can drift apart. We decided that container shapes are **recipes compiled into the recovery
actor**, one per role (`control-plane`, `node-agent`, `postgres`, `recovery-actor`), keyed by a
**recipe revision**; that each platform image names the revision it needs in one image label; and
that **the recovery actor moves first** inside every attempt, so the actor that renders a release's
control plane or node agent is always that release's actor (#352 decisions 5 and 13).

## The rules

- **The recipe image label.** Every platform image carries `org.quasar.recipe=<integer>`, stamped
  by `deploy/build-images.sh` from a constant in the service's source tree, and asserted by
  `deploy/image-contract.json` (a tightening). A revision bumps only when a container needs a new
  mount, environment input, port, device or capability; a code change alone does not bump it.
- **A service's versioned specification** is recipe revision + machine inputs + image digest.
  Machine inputs only ever grow, each with a default, so a new revision never needs an operator
  step. Nothing else — no template shipped in an image, no service-specification table — can
  change a container's shape.
- **Rule A — the actor moves first.** When a target's recovery actor is not on the release, the
  planner orders its components `[recovery-actor, <service>]`; the actor hands over to its
  successor, and the successor renders the service. On a combined host the actor moves in the
  control-plane step, so the host step names only the node agent. A revert runs the other way,
  `[node-agent, recovery-actor]`: the newer actor puts the agent back, then hands itself back.
- **Rule B — a window.** Every recovery actor renders every recipe revision from its release's
  **floor** up to its own. That covers an agent or actor revert, the restore of a pre-update
  control plane, and an ADR 0004 restore of a kept container.
- **Rule C — refuse before stopping.** An actor asked to render a revision it does not carry
  (reachable only by a hand-built developer apply that omits the actor) fails
  `recipe_unsupported` after the pull and before anything stops: nothing changed.
- **The A1 exception to ADR 0002's "never ahead"** (decision A1,
  `docs/rh06/2026-09-24-decisions.md`). On the control plane's own machine the recovery actor may be **one release ahead** of the
  control plane while a control-plane replacement is in flight, or after one failed and was
  restored. A developer apply may, in the same two situations, put it ahead by a **branch commit**
  rather than by one release; that is within A1's intent (`protocol/control-api.md` amendment 14,
  "Developer apply"). Nowhere else: what is offered
  (host eligibility, revert targets) still never puts an agent or an actor above the control
  plane. ADR 0002's reason for "control plane first" is the database schema, and the actor carries
  none, so moving it first does not weaken that rule.
- **The release-time check.** `scripts/release/` refuses to publish a release whose recovery actor
  does not support the recipe labels of that release's control-plane and node-agent images, whose
  actor window does not reach the release's floor, or whose floor would strand a component the
  previous release could still manage — so no one-step update can leave a host unmanageable.

## Considered options

- **Compiled recipes (chosen).** The authority over host access stays in reviewed actor code; an
  image can only select among shapes the actor already has. Because the actor moves first and both
  are built from one commit, the coupling costs one more file touched per shape change, not an
  extra release.
- **Image-carried templates** (designs 2 and 3 of the RH06 architecture,
  `docs/rh06/designs/2-declarative-service-specs.md` and `docs/rh06/designs/3-rust-runtime-reuse.md`,
  compared in `docs/rh06/2026-09-24-architecture.md`). They remove the "every
  shape change is an actor change" coupling, but add a template vocabulary and format discipline
  that becomes a long-lived contract, and move the authority boundary into a document an image
  supplies. Recorded as the path to take if container shapes start changing faster than releases,
  or if third parties build Quasar-compatible images.
- **Fully rendered service specifications in Postgres** (design 2,
  `docs/rh06/designs/2-declarative-service-specs.md`). The largest contract, and a
  second desired-state system beside RH05's host policy. Revive if operators need per-service
  configuration beyond machine inputs.
- **Keeping D9's "the actor is never ahead" literally** (`docs/rh06/2026-09-24-decisions.md`). Every container-shape change would then
  take two releases, or need the template language above.

## Consequences

- A container-shape change is an actor change in the same commit, with a golden rendered
  specification per role, revision and GPU vendor; while the Compose path remains for contributors,
  a parity test compares the rendered node-agent and control-plane specifications with the
  Compose definitions.
- A developer apply that moves an image without the actor that supports it fails
  `recipe_unsupported`, safely.
- The floor is part of every release (`platform-release-manifest.v2.json`), and a host whose agent or
  actor is below the installed control plane's floor reads `below_floor` for anything but an update
  (`protocol/control-api.md` amendment 14).

This decision was approved through the #353 contract amendment: an Opus contract review returned
APPROVED on round 4, and the owner's standing approval on #352, widened on #353, is the sign-off.
