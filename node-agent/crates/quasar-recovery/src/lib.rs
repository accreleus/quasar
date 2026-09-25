//! The recovery actor's library (RH-06, `docs/rh06/2026-09-24-architecture.md` §5.1).
//!
//! - [`trust`] is the port of the Go updater's release trust gates (namespace allowlist,
//!   digest and component rules, request validation, ADR 0003 signatures). Its definition
//!   of "the same behaviour" is the shared golden vectors in
//!   `testdata/recovery/trust-vectors/`, which the Go updater also runs.
//! - [`socket`] is the control socket's request/status shapes, pinned against their Go
//!   twin (`control-plane/internal/actorsocket`) by `testdata/recovery/socket/`.

pub mod socket;
pub mod trust;
