//! The recovery actor's library (RH-06, `docs/rh06/2026-09-24-architecture.md` §5.1).
//!
//! - [`actor`] is the deep module: [`actor::Actor::resume`] installs and completes this
//!   machine's services, [`actor::Actor::status`] reports its inventory.
//! - [`recipe`] is the pure recipe book (ADR 0008): each role's container shape by
//!   revision. [`probe`] detects the GPU through a disposable container.
//! - [`engine`] is the engine port: the real Docker adapter and an in-memory fake.
//! - [`machine`] is the machine-state layout; [`server`] the agent socket.
//! - [`trust`] is the port of the Go updater's release trust gates (namespace allowlist,
//!   digest and component rules, request validation, ADR 0003 signatures). Its definition
//!   of "the same behaviour" is the shared golden vectors in
//!   `testdata/recovery/trust-vectors/`, which the Go updater also runs.
//! - [`socket`] is the sockets' request/status shapes, pinned against their Go twin
//!   (`control-plane/internal/actorsocket`) by `testdata/recovery/socket/`.

pub mod actor;
pub mod engine;
pub mod identity;
pub mod machine;
pub mod probe;
pub mod recipe;
pub mod server;
pub mod socket;
pub mod trust;
