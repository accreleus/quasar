//! The recipe revision this node-agent image needs (ADR 0008, `CONTEXT.md` "Recipe").
//!
//! A recovery actor creates this agent's container from a recipe compiled into the actor,
//! and refuses a revision it does not carry. `deploy/build-images.sh` stamps this constant
//! as the image label `org.quasar.recipe` by reading the line below, so keep its exact
//! shape: `pub const RECIPE_REVISION: u32 = <n>;`.
//!
//! Bump it only when this agent starts needing a new mount, environment input, port,
//! device or capability, and add that revision to the actor's recipe book
//! (`crates/quasar-recovery/src/recipe`) in the same commit; a code change alone never
//! bumps it. The actor's test `every_revision_the_tree_declares_is_carried_by_the_book`
//! reads this file and fails if the book does not carry it.

/// 2: reads `ENROLLMENT_TOKEN_FILE`, the local enrollment token a combined host's agent is
/// given.
pub const RECIPE_REVISION: u32 = 2;
