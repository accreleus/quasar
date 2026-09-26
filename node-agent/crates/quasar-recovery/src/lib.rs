//! The recovery actor's library (RH-06, `docs/rh06/2026-09-24-architecture.md` §5.1).
//!
//! - [`actor`] is the deep module: [`actor::Actor::resume`] installs and completes this
//!   machine's services and settles an interrupted attempt, [`actor::Actor::status`]
//!   reports its inventory, [`actor::Actor::submit`] admits a replacement. The attempt
//!   machinery is [`submit`], [`journal`], [`settle`] (the D8 table) and [`replace`].
//! - [`recipe`] is the pure recipe book (ADR 0008): each role's container shape by
//!   revision. [`probe`] detects the GPU through a disposable container.
//! - [`engine`] is the engine port: the real Docker adapter and an in-memory fake.
//! - [`machine`] is the machine-state layout; [`server`] the agent socket.
//! - [`seed`] is the seed mode (ADR 0007): the frozen `seed.json`, labels and actor
//!   profile, and the loop that keeps a recovery actor in existence. [`bootstrap`] is the
//!   install inputs the seed checks and its first actor reads.
//! - [`shutdown`] is SIGTERM/SIGINT handling for both modes.
//! - [`trust`] is the port of the Go updater's release trust gates (namespace allowlist,
//!   digest and component rules, request validation, ADR 0003 signatures). Its definition
//!   of "the same behaviour" is the shared golden vectors in
//!   `testdata/recovery/trust-vectors/`, which the Go updater also runs.
//! - [`socket`] is the sockets' request/status shapes, pinned against their Go twin
//!   (`control-plane/internal/actorsocket`) by `testdata/recovery/socket/`.

pub mod actor;
pub mod bootstrap;
pub mod dump;
pub mod engine;
pub mod explain;
pub mod handover;
pub mod identity;
mod install_control;
pub mod journal;
pub mod machine;
pub mod operator;
pub mod probe;
pub mod race_guard;
pub mod recipe;
pub mod reconfigure;
mod remove;
pub mod replace;
pub mod seed;
pub mod server;
pub mod settle;
pub mod shutdown;
pub mod socket;
pub mod submit;
pub mod trust;
pub mod uninstall;
