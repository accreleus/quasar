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
/// 3 (RH-07 #402): least privilege. No host `/dev` mount, no `NET_ADMIN` or `SYSLOG`,
/// `/dev/kmsg` only when the host allows kernel-log reads, and `label=disable`. This agent
/// reads GPU faults only through that optional grant; without NET_ADMIN its firewall-reading
/// media reachability check reports that it cannot read the firewall (#403 replaces it).
/// 4 (#461): console mode no longer gives this agent `SYS_ADMIN`, the sound device or the
/// console PipeWire socket; the console session's container holds the screen, the input
/// and the sound device (ADR 0009). The agent is told whether the host has a sound device
/// (`QUASAR_HOST_SOUND`) instead of reading its own `/dev/snd`.
pub const RECIPE_REVISION: u32 = 4;
