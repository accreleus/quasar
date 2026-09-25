//! Quasar's container-runtime layer, shared by the node agent and the recovery
//! actor (#355, RH-06): the engine facade, ownership labels, container
//! self-inspection and the durable-state primitives the agent's journals commit
//! through.
//!
//! GStreamer-free and CUDA-free by construction — the actor is a small static
//! binary in a slim image. Nothing here may depend on `gstreamer*`, `glib` or a
//! CUDA crate; `cargo tree -p quasar-runtime` is the check.
//!
//! - **Engine facade.** [`RuntimeConfig`] resolves the one explicit Unix endpoint
//!   and refuses every selector it cannot honour; [`RuntimeClient`] runs each
//!   operation on its own executor under admission, a deadline and cancellation
//!   ([`Operation`]); [`docker`] is the typed Bollard adapter, the credential
//!   loader and the error classification into [`ErrorKind`].
//! - **Read-only inspection.** Container, image, storage and engine facts
//!   ([`ContainerInspection`], [`EngineFacts`], ...), the agent/daemon path
//!   translation, and [`self_inspection`] — which container this process is.
//! - **Ownership.** The [`ownership::LABEL`] every managed container carries and
//!   the rule that proves a container is ours.
//! - **Durable state.** [`DurableFile`] (write-temp, fsync, rename, fsync parent)
//!   and [`StateLease`] (a non-blocking exclusive `flock`).

mod client;
mod config;
pub mod docker;
mod durable;
mod engine;
mod error;
mod inspection;
pub mod ownership;
pub mod self_inspection;

pub use client::{Operation, RuntimeClient, ENGINE_INSPECTION_BUDGET};
pub use config::RuntimeConfig;
pub use durable::{DurableFile, LeaseError, StateLease};
pub use engine::{ApiVersion, CdiFacts, EngineFacts, EngineInfo, API_FLOOR};
pub use error::{ErrorKind, RuntimeError};
pub use inspection::{
    agent_path_for_daemon_path, daemon_path_for_agent_path, ContainerInspection, DaemonHostPath,
    DaemonImage, EngineStorage, ImageMetadata, Mount, MountKind,
};

#[cfg(test)]
mod client_tests;
#[cfg(test)]
mod inspection_tests;
