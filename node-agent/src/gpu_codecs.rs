//! Per-GPU codec advertisement (#301): what codec set each GPU has proven, and the
//! host-level union sent as `capacity.codecs` (agent-api.md amendment 12). Three
//! layers feed the rule from elsewhere — the registry plan
//! (`session::pipeline::probe_codec_support` on this GPU's own render node), the
//! driver-compatibility exclusion (`encoder_compatibility::excluded_codecs`), and the
//! codec-probe verdict held under the current stack (`host_probe::outcome::CodecEvidence`) —
//! composed here as pure functions so the safety property (only a codec proven on the
//! current stack is advertised above H.264) is unit-tested without a GPU or a registry.

use std::collections::BTreeSet;

use crate::host_probe::ProbeCodec;
use crate::session::Codec;

/// The advertisement rule. A usable GPU (`encode_slots_total > 0`) always has H.264,
/// the floor, whatever its plan says. Every other codec needs to be in the registry
/// `plan`, outside the driver-compatibility `excluded` set, and `proven` — a codec-probe
/// pass on this GPU under the current stack. An unusable GPU advertises nothing.
pub(crate) fn gpu_codec_set(
    plan: &BTreeSet<Codec>,
    excluded: &BTreeSet<Codec>,
    proven: impl Fn(Codec) -> bool,
    usable: bool,
) -> BTreeSet<Codec> {
    if !usable {
        return BTreeSet::new();
    }
    let above_floor = plan
        .iter()
        .filter(|&&codec| codec != Codec::H264 && !excluded.contains(&codec) && proven(codec))
        .copied();
    std::iter::once(Codec::H264).chain(above_floor).collect()
}

/// `capacity.codecs`: the union over usable GPUs, never empty — H.264 is the floor even
/// for a host with no usable GPU (agent-api.md amendment 12, "never empty in practice").
pub(crate) fn host_codec_set<'a>(
    gpu_sets: impl IntoIterator<Item = &'a BTreeSet<Codec>>,
) -> BTreeSet<Codec> {
    gpu_sets
        .into_iter()
        .fold(BTreeSet::from([Codec::H264]), |mut acc, set| {
            acc.extend(set.iter().copied());
            acc
        })
}

/// The above-floor codecs a codec probe should target on this GPU: the registry plan
/// (layer 1) minus the driver-compatibility exclusion (layer 2). Never gated on a
/// verdict — the verdict is what a codec probe produces, not an input to scheduling
/// it. H.264 has no target: its own media probe is the evidence for it.
pub(crate) fn probeable_codecs(
    plan: &BTreeSet<Codec>,
    excluded: &BTreeSet<Codec>,
) -> BTreeSet<ProbeCodec> {
    plan.iter()
        .filter(|codec| !excluded.contains(codec))
        .filter_map(|&codec| ProbeCodec::above_floor(codec))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn set(codecs: &[Codec]) -> BTreeSet<Codec> {
        codecs.iter().copied().collect()
    }

    #[test]
    fn a_usable_gpu_always_has_the_h264_floor() {
        for plan in [set(&[Codec::H264]), BTreeSet::new(), set(&[Codec::H265])] {
            assert_eq!(
                gpu_codec_set(&plan, &BTreeSet::new(), |_| false, true),
                set(&[Codec::H264]),
                "{plan:?}"
            );
        }
    }

    #[test]
    fn every_other_codec_needs_a_proven_pass_and_the_plan() {
        let plan = set(&[Codec::H264, Codec::H265]);
        assert_eq!(
            gpu_codec_set(&plan, &BTreeSet::new(), |_| false, true),
            set(&[Codec::H264]),
            "no pass on this stack ⇒ h264-only"
        );
        assert_eq!(
            gpu_codec_set(&plan, &BTreeSet::new(), |c| c == Codec::H265, true),
            set(&[Codec::H264, Codec::H265])
        );
        assert_eq!(
            gpu_codec_set(&plan, &BTreeSet::new(), |_| true, true),
            set(&[Codec::H264, Codec::H265]),
            "a pass for a codec outside the plan is not advertised"
        );
    }

    #[test]
    fn compatibility_exclusion_precedes_the_probe() {
        let plan = set(&[Codec::H264, Codec::Av1]);
        let excluded = set(&[Codec::Av1]);
        assert_eq!(
            gpu_codec_set(&plan, &excluded, |_| true, true),
            set(&[Codec::H264])
        );
    }

    #[test]
    fn an_unusable_gpu_advertises_nothing() {
        let plan = set(&[Codec::H264, Codec::H265, Codec::Av1]);
        assert!(gpu_codec_set(&plan, &BTreeSet::new(), |_| true, false).is_empty());
    }

    #[test]
    fn host_set_is_the_union_over_usable_gpus() {
        let gpu0 = set(&[Codec::H264, Codec::H265]);
        let gpu1 = set(&[Codec::H264, Codec::Av1]);
        let pinned_out = BTreeSet::new();
        assert_eq!(
            host_codec_set([&gpu0, &gpu1, &pinned_out]),
            set(&[Codec::H264, Codec::H265, Codec::Av1])
        );
    }

    #[test]
    fn host_set_is_never_empty() {
        assert_eq!(host_codec_set([]), set(&[Codec::H264]));
        assert_eq!(host_codec_set([&BTreeSet::new()]), set(&[Codec::H264]));
    }

    #[test]
    fn probeable_codecs_excludes_the_floor_and_the_compatibility_layer() {
        let plan = set(&[Codec::H264, Codec::H265, Codec::Av1]);
        let excluded = set(&[Codec::Av1]);
        assert_eq!(
            probeable_codecs(&plan, &excluded),
            BTreeSet::from([ProbeCodec::H265])
        );
    }
}
