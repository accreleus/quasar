//! The shared result shape for the two container host probes (#259): application-GPU
//! access and audio. Kept here rather than duplicated in each so their outcome mapping
//! stays comparable.

use crate::runtime::HelperResult;

/// How a container-probe run ended, before [`super::app_gpu::outcome`] or
/// [`super::audio::outcome`] turns it into a [`super::outcome::ProbeOutcome`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Observed {
    /// The probe container ran to completion under observation.
    Exited(HelperResult),
    /// The orchestrator's own deadline passed while observing.
    Deadline,
    /// A session launch pre-empted the probe.
    Preempted,
    /// An earlier probe of this kind is still unreconciled; nothing was created.
    Busy,
    /// The runtime API returned an error this probe cannot interpret further.
    RuntimeError(String),
    /// The audio sidecar's socket became connectable.
    SocketReady,
    /// The audio sidecar's socket never became connectable within its wait budget.
    SocketTimeout,
}

/// One probe run's whole result. `reconciled` is false when a stop or cleanup left an
/// uncertain outcome — the runner must reconcile this kind before its next run.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ContainerProbeEnd {
    pub observed: Observed,
    pub reconciled: bool,
}
