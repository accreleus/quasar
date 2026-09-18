//! Host probes (spec #252): bounded, disposable jobs that exercise a real path and
//! report the result as a retained readiness check. Glossary: `CONTEXT.md` "Host readiness".

pub mod app_gpu;
pub mod audio;
pub mod child;
pub mod container;
pub mod decision;
pub mod launch_failure;
pub mod media;
pub mod orchestrator;
pub mod outcome;
pub mod runner;

/// Listed in the order probes run when several are pending.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum ProbeKind {
    Input,
    Audio,
    Media,
    ApplicationGpu,
}

impl ProbeKind {
    pub const ALL: [ProbeKind; 4] = [
        ProbeKind::Input,
        ProbeKind::Audio,
        ProbeKind::Media,
        ProbeKind::ApplicationGpu,
    ];

    /// One result per GPU, not one per host.
    pub fn per_gpu(self) -> bool {
        matches!(self, ProbeKind::Media | ProbeKind::ApplicationGpu)
    }

    /// A sibling container through the runtime interface, so an uncertain stop or
    /// cleanup must be reconciled before another of its kind starts.
    pub fn is_container(self) -> bool {
        matches!(self, ProbeKind::Audio | ProbeKind::ApplicationGpu)
    }

    /// The check id on a host with no GPU, and the base of the per-GPU ids. The
    /// console groups by this base: web/src/lib/readiness/groups.ts.
    pub fn check_id(self) -> &'static str {
        match self {
            ProbeKind::Input => "input_probe",
            ProbeKind::Audio => "audio_probe",
            ProbeKind::Media => "media_probe",
            ProbeKind::ApplicationGpu => "application_gpu_probe",
        }
    }
}

/// One probe run: a kind, and the GPU index it exercises when the kind is per GPU.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct ProbeTarget {
    pub kind: ProbeKind,
    pub gpu: Option<i32>,
}

impl ProbeTarget {
    pub fn host(kind: ProbeKind) -> Self {
        ProbeTarget { kind, gpu: None }
    }

    pub fn gpu(kind: ProbeKind, index: i32) -> Self {
        ProbeTarget {
            kind,
            gpu: Some(index),
        }
    }

    pub fn check_id(self) -> String {
        match self.gpu {
            Some(index) => format!("{}_gpu{index}", self.kind.check_id()),
            None => self.kind.check_id().to_string(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn check_ids_are_per_gpu_for_gpu_probes_and_per_host_otherwise() {
        assert_eq!(
            ProbeTarget::host(ProbeKind::Input).check_id(),
            "input_probe"
        );
        assert_eq!(
            ProbeTarget::host(ProbeKind::Audio).check_id(),
            "audio_probe"
        );
        assert_eq!(
            ProbeTarget::gpu(ProbeKind::Media, 0).check_id(),
            "media_probe_gpu0"
        );
        assert_eq!(
            ProbeTarget::gpu(ProbeKind::ApplicationGpu, 1).check_id(),
            "application_gpu_probe_gpu1"
        );
    }
}
