//! Library surface for the Quasar node-agent. The binary entry point lives in
//! `main.rs` and consumes this crate; exposing the modules here also lets the
//! Criterion benches (`benches/`) and any integration tests exercise internal
//! APIs such as `session::metrics::SessionMetrics`.

pub mod agent;
/// Shared download/lock/backoff machinery for the artifact provisioners
/// (`nvidia_volume`, `cuda_runtime`).
pub mod artifact;
/// Build stamps + install-mode discovery: what this agent is and how it got here.
pub mod buildinfo;
pub mod capacity;
pub mod config;
mod container_ownership;
pub mod cp_http;
pub mod cp_tls;
/// Runtime-provisioned CUDA userspace (NVRTC) — what registers the `cuda*`
/// GStreamer elements on an NVIDIA host (#545).
pub mod cuda_runtime;
pub mod ddc;
/// Diagnostic registration (#256): what a host withholds while its startup cleanup is
/// unresolved, and when it resumes. See `CONTEXT.md` "Diagnostic registration".
pub mod diagnostic;
mod encoder_compatibility;
pub mod enrollment;
/// The per-GPU codec advertisement rule (#301): registry plan × driver-compatibility
/// exclusion × codec-probe verdict → each GPU's codec set, and the host union.
mod gpu_codecs;
/// `host.xid` / `host.gpu_fault`: the kernel's own GPU fault records, off `/dev/kmsg`.
pub mod gpu_identity;
pub mod gpu_kmsg;
/// GPU-vendor detection backing the `QUASAR_ENCODER` auto-default.
pub mod gpu_vendor;
pub mod health;
pub mod host_probe;
pub mod images;
pub mod jobs;
/// Log spans + the WARN/ERROR `token=` convention (`.claude/rules/agent-logging.md`).
pub mod logging;
pub mod memstat;
pub mod messages;
pub mod nvidia_volume;
/// Shared owner-marked runtime-dir entry mechanics (udev export, media probe
/// dir): see the module doc.
mod owned_entry;
pub mod policy;
pub mod policy_catalog;
pub mod readiness;
pub mod release;
pub mod session;
pub mod vram;

pub mod runtime;
pub mod source_policy;

/// Lookup-closure builder for tests of env-reading pure cores.
#[cfg(test)]
pub(crate) mod test_env;
#[cfg(test)]
pub(crate) mod test_lease;
