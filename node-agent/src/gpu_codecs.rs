//! Per-GPU codec advertisement (#301): what codec set each GPU has proven, and the
//! host-level union sent as `capacity.codecs` (agent-api.md amendment 12). Three
//! layers feed the rule from elsewhere — the registry plan
//! (`session::pipeline::probe_codec_support` on this GPU's own render node), the
//! driver-compatibility exclusion (`encoder_compatibility::excluded_codecs`), and the
//! retained codec-probe verdict (`host_probe::outcome::codec_probe_verdict`, #300) —
//! composed here as pure functions so the safety property (only a proven codec is
//! advertised) is unit-tested without a GPU, a registry or a live probe.

use std::collections::BTreeSet;

use crate::host_probe::ProbeCodec;
use crate::session::Codec;

/// The advertisement rule. H.264 is the floor: admitted whenever the registry `plan`
/// builds it. Every other codec needs an explicit `Some(true)` from `verdict` — `None`
/// (no probe yet, or indeterminate) and `Some(false)` are not advertised, so a GPU
/// stays h264-only until its codec probes have run. `excluded` (the driver-
/// compatibility layer) removes a codec before `verdict` is even consulted, so a
/// passing probe can never override a known-corrupt combination. `usable` is
/// `encode_slots_total > 0`: a render-node pin zeroes every other GPU, and a zeroed
/// GPU advertises nothing, dropping it out of the host union.
pub(crate) fn gpu_codec_set(
    plan: &BTreeSet<Codec>,
    excluded: &BTreeSet<Codec>,
    verdict: impl Fn(Codec) -> Option<bool>,
    usable: bool,
) -> BTreeSet<Codec> {
    if !usable {
        return BTreeSet::new();
    }
    plan.iter()
        .filter(|codec| !excluded.contains(codec))
        .filter(|&&codec| codec == Codec::H264 || verdict(codec) == Some(true))
        .copied()
        .collect()
}

/// The host-level `capacity.codecs` union over usable GPUs (agent-api.md amendment 12).
pub(crate) fn host_codec_union<'a>(
    gpu_sets: impl IntoIterator<Item = &'a BTreeSet<Codec>>,
) -> BTreeSet<Codec> {
    gpu_sets.into_iter().fold(BTreeSet::new(), |mut acc, set| {
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
    fn h264_admitted_whenever_the_plan_builds_it() {
        let plan = set(&[Codec::H264]);
        assert_eq!(
            gpu_codec_set(&plan, &BTreeSet::new(), |_| None, true),
            set(&[Codec::H264])
        );
    }

    #[test]
    fn every_other_codec_needs_an_explicit_pass() {
        let plan = set(&[Codec::H264, Codec::H265]);
        assert_eq!(
            gpu_codec_set(&plan, &BTreeSet::new(), |_| None, true),
            set(&[Codec::H264]),
            "no verdict yet ⇒ h264-only"
        );
        assert_eq!(
            gpu_codec_set(
                &plan,
                &BTreeSet::new(),
                |c| (c == Codec::H265).then_some(false),
                true
            ),
            set(&[Codec::H264]),
            "a failed probe is not advertised"
        );
        assert_eq!(
            gpu_codec_set(
                &plan,
                &BTreeSet::new(),
                |c| (c == Codec::H265).then_some(true),
                true
            ),
            set(&[Codec::H264, Codec::H265])
        );
    }

    #[test]
    fn compatibility_exclusion_precedes_the_probe() {
        let plan = set(&[Codec::H264, Codec::Av1]);
        let excluded = set(&[Codec::Av1]);
        // Even a passing probe cannot override a layer-2 exclusion — the rule never
        // calls `verdict` for an excluded codec.
        assert_eq!(
            gpu_codec_set(&plan, &excluded, |_| Some(true), true),
            set(&[Codec::H264])
        );
    }

    #[test]
    fn an_unusable_gpu_advertises_nothing() {
        let plan = set(&[Codec::H264, Codec::H265, Codec::Av1]);
        assert!(gpu_codec_set(&plan, &BTreeSet::new(), |_| Some(true), false).is_empty());
    }

    #[test]
    fn host_union_drops_a_pinned_out_gpu() {
        let gpu0 = set(&[Codec::H264, Codec::H265]);
        let gpu1 = BTreeSet::new(); // zeroed by a render-node pin
        assert_eq!(
            host_codec_union([&gpu0, &gpu1]),
            set(&[Codec::H264, Codec::H265])
        );
    }

    #[test]
    fn host_union_is_the_union_over_usable_gpus() {
        let gpu0 = set(&[Codec::H264, Codec::H265]);
        let gpu1 = set(&[Codec::H264, Codec::Av1]);
        assert_eq!(
            host_codec_union([&gpu0, &gpu1]),
            set(&[Codec::H264, Codec::H265, Codec::Av1])
        );
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
