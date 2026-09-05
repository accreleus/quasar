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
pub mod cp_http;
pub mod cp_tls;
/// Runtime-provisioned CUDA userspace (NVRTC) — what registers the `cuda*`
/// GStreamer elements on an NVIDIA host (#545).
pub mod cuda_runtime;
pub mod ddc;
pub mod enrollment;
/// `host.xid` / `host.gpu_fault`: the kernel's own GPU fault records, off `/dev/kmsg`.
pub mod gpu_kmsg;
/// GPU-vendor detection backing the `QUASAR_ENCODER` auto-default.
pub mod gpu_vendor;
pub mod health;
pub mod images;
pub mod jobs;
/// Log spans + the WARN/ERROR `token=` convention (`.claude/rules/agent-logging.md`).
pub mod logging;
pub mod memstat;
pub mod messages;
pub mod nvidia_volume;
pub mod readiness;
pub mod release;
pub mod session;
pub mod vram;
