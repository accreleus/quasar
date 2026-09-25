//! The recipe revision the recovery-actor image needs (ADR 0008). `deploy/build-images.sh`
//! stamps it as the image label `org.quasar.recipe` by reading the line below, so keep its
//! exact shape: `pub const RECIPE_REVISION: u32 = <n>;`. Bump it only when the actor's own
//! container needs a new mount, environment input, port, device or capability.

pub const RECIPE_REVISION: u32 = 1;
