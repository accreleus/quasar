//! Host probes (spec #252): bounded, disposable jobs that exercise a real path and
//! report the result as a retained readiness check. Glossary: `CONTEXT.md` "Host readiness".

pub mod app_gpu;
pub mod audio;
pub mod child;
pub mod container;
pub mod decision;
pub mod launch_failure;
pub mod media;
pub mod media_probe_dir;
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

/// A codec above the H.264 floor: the only codecs a codec probe runs for. H.264 is what
/// the GPU's own media probe proves, so it has no codec target.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum ProbeCodec {
    H265,
    Av1,
}

impl ProbeCodec {
    /// `None` for H.264, the floor.
    pub fn above_floor(codec: crate::session::Codec) -> Option<Self> {
        match codec {
            crate::session::Codec::H264 => None,
            crate::session::Codec::H265 => Some(ProbeCodec::H265),
            crate::session::Codec::Av1 => Some(ProbeCodec::Av1),
        }
    }

    /// A wire codec string; `None` for h264 and anything unknown.
    pub fn from_wire(codec: &str) -> Option<Self> {
        crate::session::Codec::parse(codec)
            .ok()
            .and_then(Self::above_floor)
    }

    pub fn codec(self) -> crate::session::Codec {
        match self {
            ProbeCodec::H265 => crate::session::Codec::H265,
            ProbeCodec::Av1 => crate::session::Codec::Av1,
        }
    }

    /// Wire vocabulary, as in `sessions.codec` and the check id.
    pub fn as_str(self) -> &'static str {
        self.codec().as_str()
    }
}

/// One probe run: a kind, the GPU index it exercises when the kind is per GPU, and for a
/// codec probe the codec. A codec probe is the media probe run on one GPU for one codec
/// above the floor (CONTEXT.md "Codec probe").
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct ProbeTarget {
    pub kind: ProbeKind,
    pub gpu: Option<i32>,
    pub codec: Option<ProbeCodec>,
}

impl ProbeTarget {
    pub fn host(kind: ProbeKind) -> Self {
        ProbeTarget {
            kind,
            gpu: None,
            codec: None,
        }
    }

    pub fn gpu(kind: ProbeKind, index: i32) -> Self {
        ProbeTarget {
            kind,
            gpu: Some(index),
            codec: None,
        }
    }

    pub fn codec(index: i32, codec: ProbeCodec) -> Self {
        ProbeTarget {
            kind: ProbeKind::Media,
            gpu: Some(index),
            codec: Some(codec),
        }
    }

    /// The GPU's H.264 media probe that a codec probe queues behind; itself otherwise.
    pub fn media_floor(self) -> Self {
        ProbeTarget {
            codec: None,
            ..self
        }
    }

    pub fn check_id(self) -> String {
        match (self.gpu, self.codec) {
            (Some(index), Some(codec)) => {
                format!("{}_gpu{index}_{}", self.kind.check_id(), codec.as_str())
            }
            (Some(index), None) => format!("{}_gpu{index}", self.kind.check_id()),
            (None, _) => self.kind.check_id().to_string(),
        }
    }

    /// What a `fail` on this target's check blocks (protocol/agent-api.md `readiness`).
    /// A GPU-scoped kind with no index names nothing to block, so it carries none. A codec
    /// probe never blocks: its failure takes the codec off that GPU, not the GPU.
    pub fn blocks(self) -> Option<crate::messages::ReadinessBlocks> {
        if self.codec.is_some() {
            return None;
        }
        match self.kind {
            ProbeKind::Media | ProbeKind::ApplicationGpu => self
                .gpu
                .map(|index| crate::messages::ReadinessBlocks::gpu(index, "control_plane")),
            ProbeKind::Input | ProbeKind::Audio => {
                Some(crate::messages::ReadinessBlocks::host("control_plane"))
            }
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
        assert_eq!(
            ProbeTarget::codec(0, ProbeCodec::H265).check_id(),
            "media_probe_gpu0_h265"
        );
        assert_eq!(
            ProbeTarget::codec(2, ProbeCodec::Av1).check_id(),
            "media_probe_gpu2_av1"
        );
    }

    #[test]
    fn a_codec_probe_never_blocks_and_queues_behind_its_gpus_media_probe() {
        let target = ProbeTarget::codec(1, ProbeCodec::Av1);
        assert_eq!(target.blocks(), None);
        assert_eq!(target.media_floor(), ProbeTarget::gpu(ProbeKind::Media, 1));
        assert!(ProbeTarget::gpu(ProbeKind::Media, 1).blocks().is_some());
    }

    #[test]
    fn only_codecs_above_the_floor_have_a_codec_probe() {
        assert_eq!(ProbeCodec::from_wire("h264"), None);
        assert_eq!(ProbeCodec::from_wire("h265"), Some(ProbeCodec::H265));
        assert_eq!(ProbeCodec::from_wire("hevc"), Some(ProbeCodec::H265));
        assert_eq!(ProbeCodec::from_wire("av1"), Some(ProbeCodec::Av1));
        assert_eq!(ProbeCodec::from_wire("vp9"), None);
    }
}
