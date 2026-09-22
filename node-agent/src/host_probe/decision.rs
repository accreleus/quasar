//! When host probes run (spec #252 "When probes run", "Probe lifecycle"). A state
//! machine with no I/O and no clock: the orchestrator feeds it events and carries out
//! the actions it returns.

use std::collections::{BTreeMap, BTreeSet};

use super::{ProbeCodec, ProbeKind, ProbeTarget};

/// What a probe result depends on. A change re-runs the probes it could affect.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ProbeInputs {
    pub agent_image: String,
    /// Driver version and driver-volume identity, as one comparable string.
    pub driver: String,
    /// GPU index to device identity. A different device under the same index is a
    /// different GPU.
    pub gpus: BTreeMap<i32, String>,
    /// The host settings that select the media path, as one comparable string.
    pub settings: String,
    /// Per GPU, the codecs above the floor its encoder plan can build: each gets a codec
    /// probe. A GPU absent here gets none.
    pub codecs: BTreeMap<i32, BTreeSet<ProbeCodec>>,
}

/// The slice of [`ProbeInputs`] a codec probe on one GPU ran under (#301): agent-side
/// state, never on the wire. A codec-probe pass counts only while the current stack's
/// stamp for that GPU index equals the one it was proven under.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EvidenceStamp {
    agent_image: String,
    driver: String,
    settings: String,
    gpu: String,
}

impl ProbeInputs {
    /// `None` when `gpu` is not in the inventory: nothing can be proven for it.
    pub fn evidence_stamp(&self, gpu: i32) -> Option<EvidenceStamp> {
        Some(EvidenceStamp {
            agent_image: self.agent_image.clone(),
            driver: self.driver.clone(),
            settings: self.settings.clone(),
            gpu: self.gpus.get(&gpu)?.clone(),
        })
    }
}

/// What a finished probe concluded, as far as scheduling cares: a codec probe runs only
/// on a GPU whose media probe last concluded `Passed`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Verdict {
    Passed,
    /// Failed, or not applicable to this GPU.
    NotPassed,
    /// Leaves the last definitive verdict standing.
    Indeterminate,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Event {
    /// The registration handshake finished and the first capacity message was sent.
    Registered(ProbeInputs),
    /// The control-plane connection ended; the next one has its own handshake window.
    Disconnected,
    InputsObserved(ProbeInputs),
    /// A `session_assign` landed. Must be delivered before the launch needs the GPU.
    LaunchArrived {
        gpu: i32,
    },
    /// GPUs with an assigned, starting, running or tearing-down session, and whether
    /// any session is still on its way to running.
    SessionsChanged {
        live_gpus: BTreeSet<i32>,
        launching: bool,
    },
    /// A launch failed in a way these probe kinds could explain. `codec` is the failed
    /// session's codec when above the floor: with `Media` it selects that codec probe.
    LaunchFailed {
        gpu: i32,
        explains: BTreeSet<ProbeKind>,
        codec: Option<ProbeCodec>,
    },
    /// `reconciled` is false when a container probe's stop or cleanup outcome is not
    /// known. Always true for a child-process probe.
    ProbeFinished {
        target: ProbeTarget,
        reconciled: bool,
        verdict: Verdict,
    },
    Reconciled {
        kind: ProbeKind,
        ok: bool,
    },
    /// The probe never ran: a template warm-up holds the encode gate.
    ProbeDeferred(ProbeTarget),
    EncodeGateFreed,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Action {
    Start(ProbeTarget),
    /// Stop the running probe now; its result is indeterminate. The probe still
    /// reports `ProbeFinished`, and nothing else starts until it does.
    Preempt(ProbeTarget),
    /// Finish an earlier container probe under its original operation identity.
    Reconcile(ProbeKind),
    /// The host has no GPU: report the kind's check as `skip`.
    NotApplicable(ProbeKind),
    /// The target no longer exists: remove its check.
    Forget(ProbeTarget),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Running {
    target: ProbeTarget,
    preempted: bool,
    /// Its GPU vanished or was replaced mid-run: whatever it reports describes a
    /// device that is no longer there.
    discard: bool,
    /// An identity trigger landed mid-run: its result is still recorded, but a pass from
    /// the old stack does not admit codec probes.
    stale_evidence: bool,
}

#[derive(Debug)]
pub struct Scheduler {
    /// The kinds this agent can run; the others are never queued.
    kinds: Vec<ProbeKind>,
    registered: bool,
    inputs: Option<ProbeInputs>,
    pending: BTreeSet<ProbeTarget>,
    running: Option<Running>,
    live_gpus: BTreeSet<i32>,
    /// A launch is in flight. Nothing starts: even a host-wide probe would compete with
    /// it for the container runtime.
    launching: bool,
    /// Kinds with a container whose stop or cleanup is unaccounted for. No probe of
    /// that kind starts, so no new operation identity is issued over the old one.
    unreconciled: BTreeSet<ProbeKind>,
    reconciling: Option<ProbeKind>,
    /// Deferred at the encode gate; pending again once it is free.
    gate_waiting: BTreeSet<ProbeTarget>,
    /// GPUs whose media probe last concluded with a pass. A codec target waits while its
    /// GPU's media probe is outstanding and is dropped if that GPU is not in here.
    media_passed: BTreeSet<i32>,
}

impl Default for Scheduler {
    fn default() -> Self {
        Self::new()
    }
}

impl Scheduler {
    pub fn new() -> Self {
        Self::with_kinds(&ProbeKind::ALL)
    }

    pub fn with_kinds(kinds: &[ProbeKind]) -> Self {
        Scheduler {
            kinds: kinds.to_vec(),
            registered: false,
            inputs: None,
            pending: BTreeSet::new(),
            running: None,
            live_gpus: BTreeSet::new(),
            launching: false,
            unreconciled: BTreeSet::new(),
            reconciling: None,
            gate_waiting: BTreeSet::new(),
            media_passed: BTreeSet::new(),
        }
    }

    pub fn running(&self) -> Option<ProbeTarget> {
        self.running.map(|r| r.target)
    }

    /// What a codec probe starting now on `gpu` runs under: stamped on its result.
    pub fn evidence_stamp(&self, gpu: i32) -> Option<EvidenceStamp> {
        self.inputs.as_ref()?.evidence_stamp(gpu)
    }

    /// False once the target's GPU is gone: a late result must not be recorded.
    pub fn exists(&self, target: ProbeTarget) -> bool {
        match target.gpu {
            _ if !self.kinds.contains(&target.kind) => false,
            None => !target.kind.per_gpu(),
            Some(index) => self.inputs.as_ref().is_some_and(|i| {
                i.gpus.contains_key(&index)
                    && target.codec.is_none_or(|codec| {
                        i.codecs.get(&index).is_some_and(|c| c.contains(&codec))
                    })
            }),
        }
    }

    /// Ask before `ProbeFinished` is stepped.
    pub fn accepts_result(&self, target: ProbeTarget) -> bool {
        self.exists(target)
            && !self
                .running
                .is_some_and(|r| r.target == target && r.discard)
    }

    pub fn step(&mut self, event: Event) -> Vec<Action> {
        let mut actions = Vec::new();
        // A reconcile that just failed is retried on a later event, never in a loop
        // driven by its own completion.
        let mut reconcile_failed = None;
        match event {
            Event::Registered(inputs) => {
                self.registered = true;
                // Each connection has its own encode gate; the one waited on is gone.
                self.pending.append(&mut self.gate_waiting);
                if self.inputs.is_none() {
                    self.queue_kinds(&ProbeKind::ALL, &inputs, &mut actions);
                    self.inputs = Some(inputs);
                } else {
                    self.observe(inputs, &mut actions);
                }
            }
            Event::Disconnected => {
                self.registered = false;
                self.preempt(&mut actions);
            }
            Event::InputsObserved(inputs) => {
                if self.inputs.is_some() {
                    self.observe(inputs, &mut actions);
                }
            }
            Event::LaunchArrived { gpu } => {
                self.launching = true;
                self.live_gpus.insert(gpu);
                self.preempt(&mut actions);
            }
            Event::SessionsChanged {
                live_gpus,
                launching,
            } => {
                self.live_gpus = live_gpus;
                self.launching = launching;
            }
            Event::LaunchFailed {
                gpu,
                explains,
                codec,
            } => {
                let codec_target = codec
                    .filter(|_| explains.contains(&ProbeKind::Media))
                    .map(|codec| ProbeTarget::codec(gpu, codec));
                for kind in explains {
                    let target = if kind.per_gpu() {
                        ProbeTarget::gpu(kind, gpu)
                    } else {
                        ProbeTarget::host(kind)
                    };
                    if self.exists(target) {
                        self.pending.insert(target);
                    }
                }
                // Queued behind the media probe just queued, so it runs only if that passes.
                if let Some(target) = codec_target.filter(|t| self.exists(*t)) {
                    self.pending.insert(target);
                }
            }
            Event::ProbeFinished {
                target,
                reconciled,
                verdict,
            } => {
                if let Some(running) = self.running.filter(|r| r.target == target) {
                    self.running = None;
                    if running.preempted && self.exists(target) {
                        self.pending.insert(target);
                    }
                    if !reconciled {
                        self.unreconciled.insert(target.kind);
                    }
                    if !running.discard && self.exists(target) {
                        let verdict = match verdict {
                            Verdict::Passed if running.stale_evidence => Verdict::Indeterminate,
                            other => other,
                        };
                        self.media_verdict(target, verdict);
                    }
                }
            }
            Event::ProbeDeferred(target) => {
                if self.running.is_some_and(|r| r.target == target) {
                    self.running = None;
                    if self.exists(target) {
                        self.gate_waiting.insert(target);
                    }
                }
            }
            Event::EncodeGateFreed => {
                let waiting: Vec<ProbeTarget> = std::mem::take(&mut self.gate_waiting)
                    .into_iter()
                    .filter(|t| self.exists(*t))
                    .collect();
                self.pending.extend(waiting);
            }
            Event::Reconciled { kind, ok } => {
                if self.reconciling == Some(kind) {
                    self.reconciling = None;
                    if ok {
                        self.unreconciled.remove(&kind);
                    } else {
                        reconcile_failed = Some(kind);
                    }
                }
            }
        }
        self.dispatch(reconcile_failed, &mut actions);
        actions
    }

    fn preempt(&mut self, actions: &mut Vec<Action>) {
        if let Some(running) = self.running.as_mut().filter(|r| !r.preempted) {
            running.preempted = true;
            actions.push(Action::Preempt(running.target));
        }
    }

    /// A media probe's verdict gates its GPU's codec probes. A pass after anything else
    /// queues them all, so a GPU that recovers is not left without codec verdicts until
    /// the next input change.
    fn media_verdict(&mut self, target: ProbeTarget, verdict: Verdict) {
        let (ProbeKind::Media, Some(gpu), None) = (target.kind, target.gpu, target.codec) else {
            return;
        };
        match verdict {
            Verdict::Passed => {
                if self.media_passed.insert(gpu) {
                    self.queue_codecs(gpu);
                }
            }
            Verdict::NotPassed => {
                self.media_passed.remove(&gpu);
                let drop = |t: &ProbeTarget| t.gpu == Some(gpu) && t.codec.is_some();
                self.pending.retain(|t| !drop(t));
                self.gate_waiting.retain(|t| !drop(t));
            }
            Verdict::Indeterminate => {}
        }
    }

    fn queue_codecs(&mut self, gpu: i32) {
        if !self.kinds.contains(&ProbeKind::Media) {
            return;
        }
        let codecs = self
            .inputs
            .as_ref()
            .and_then(|i| i.codecs.get(&gpu))
            .cloned()
            .unwrap_or_default();
        for codec in codecs {
            self.pending.insert(ProbeTarget::codec(gpu, codec));
        }
    }

    fn queue_kinds(&mut self, kinds: &[ProbeKind], inputs: &ProbeInputs, out: &mut Vec<Action>) {
        for &kind in kinds {
            if !self.kinds.contains(&kind) {
                continue;
            }
            if !kind.per_gpu() {
                self.pending.insert(ProbeTarget::host(kind));
            } else if inputs.gpus.is_empty() {
                out.push(Action::NotApplicable(kind));
            } else {
                for &index in inputs.gpus.keys() {
                    self.pending.insert(ProbeTarget::gpu(kind, index));
                    if kind == ProbeKind::Media {
                        for &codec in inputs.codecs.get(&index).into_iter().flatten() {
                            self.pending.insert(ProbeTarget::codec(index, codec));
                        }
                    }
                }
            }
        }
    }

    /// The agent image, driver or media settings changed: a codec pass from the old stack
    /// is not evidence for the new one, so every GPU's codec checks are forgotten and its
    /// codec probes need a fresh floor pass. The floor check keeps its retained verdict.
    fn forget_codec_evidence(&mut self, new: &ProbeInputs, actions: &mut Vec<Action>) {
        self.media_passed.clear();
        if let Some(running) = self.running.as_mut() {
            running.stale_evidence = true;
        }
        if !self.kinds.contains(&ProbeKind::Media) {
            return;
        }
        for (&index, codecs) in &new.codecs {
            for &codec in codecs {
                self.retire(ProbeTarget::codec(index, codec), actions);
            }
        }
    }

    /// Stops tracking a target that no longer exists and removes its check.
    fn retire(&mut self, target: ProbeTarget, actions: &mut Vec<Action>) {
        self.pending.remove(&target);
        self.gate_waiting.remove(&target);
        if let Some(running) = self.running.as_mut().filter(|r| r.target == target) {
            running.discard = true;
            self.preempt(actions);
        }
        actions.push(Action::Forget(target));
    }

    fn observe(&mut self, new: ProbeInputs, actions: &mut Vec<Action>) {
        let Some(old) = self.inputs.replace(new.clone()) else {
            return;
        };
        let per_gpu: Vec<ProbeKind> = self.kinds.iter().copied().filter(|k| k.per_gpu()).collect();

        let media = self.kinds.contains(&ProbeKind::Media);
        let no_codecs = BTreeSet::new();
        for (index, identity) in &old.gpus {
            let old_codecs = old.codecs.get(index).unwrap_or(&no_codecs);
            if new.gpus.get(index) == Some(identity) {
                // Same GPU: a codec its plan no longer builds loses its check; a new one
                // is probed (behind the media probe, as always).
                let new_codecs = new.codecs.get(index).unwrap_or(&no_codecs);
                if media {
                    for &codec in old_codecs.difference(new_codecs) {
                        self.retire(ProbeTarget::codec(*index, codec), actions);
                    }
                    for &codec in new_codecs.difference(old_codecs) {
                        self.pending.insert(ProbeTarget::codec(*index, codec));
                    }
                }
                continue;
            }
            self.media_passed.remove(index);
            for &kind in &per_gpu {
                self.retire(ProbeTarget::gpu(kind, *index), actions);
            }
            if media {
                for &codec in old_codecs {
                    self.retire(ProbeTarget::codec(*index, codec), actions);
                }
            }
        }
        if old.gpus.is_empty() && !new.gpus.is_empty() {
            for &kind in &per_gpu {
                actions.push(Action::Forget(ProbeTarget::host(kind)));
            }
        }

        let identity_changed = new.agent_image != old.agent_image
            || new.driver != old.driver
            || new.settings != old.settings;
        if identity_changed {
            self.forget_codec_evidence(&new, actions);
        }
        if new.agent_image != old.agent_image {
            self.queue_kinds(&ProbeKind::ALL, &new, actions);
        } else if identity_changed || (new.gpus.is_empty() && !old.gpus.is_empty()) {
            self.queue_kinds(&per_gpu, &new, actions);
        } else {
            for (index, identity) in &new.gpus {
                if old.gpus.get(index) != Some(identity) {
                    for &kind in &per_gpu {
                        self.pending.insert(ProbeTarget::gpu(kind, *index));
                    }
                    if media {
                        for &codec in new.codecs.get(index).into_iter().flatten() {
                            self.pending.insert(ProbeTarget::codec(*index, codec));
                        }
                    }
                }
            }
        }
    }

    fn dispatch(&mut self, reconcile_failed: Option<ProbeKind>, actions: &mut Vec<Action>) {
        if !self.registered
            || self.launching
            || self.running.is_some()
            || self.reconciling.is_some()
        {
            return;
        }
        // Codec probes last: they only add codecs, so every probe that can block a launch
        // concludes first.
        let (floors, codecs): (Vec<ProbeTarget>, Vec<ProbeTarget>) = self
            .pending
            .iter()
            .copied()
            .partition(|t| t.codec.is_none());
        for target in floors.into_iter().chain(codecs) {
            if target.gpu.is_some_and(|g| self.live_gpus.contains(&g)) {
                continue;
            }
            if let (Some(gpu), Some(_)) = (target.gpu, target.codec) {
                let floor = target.media_floor();
                if self.pending.contains(&floor) || self.gate_waiting.contains(&floor) {
                    continue;
                }
                if !self.media_passed.contains(&gpu) {
                    self.pending.remove(&target);
                    continue;
                }
            }
            if self.unreconciled.contains(&target.kind) {
                if reconcile_failed == Some(target.kind) {
                    continue;
                }
                self.reconciling = Some(target.kind);
                actions.push(Action::Reconcile(target.kind));
                return;
            }
            self.pending.remove(&target);
            self.running = Some(Running {
                target,
                preempted: false,
                discard: false,
                stale_evidence: false,
            });
            actions.push(Action::Start(target));
            return;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use ProbeKind::{ApplicationGpu, Audio, Input, Media};

    fn inputs(gpus: &[(i32, &str)]) -> ProbeInputs {
        ProbeInputs {
            agent_image: "sha256:agent-a".into(),
            driver: "nvidia:595.99.02 volume:abc".into(),
            gpus: gpus.iter().map(|(i, id)| (*i, id.to_string())).collect(),
            settings: "encoder=vulkan".into(),
            codecs: BTreeMap::new(),
        }
    }

    fn one_gpu() -> ProbeInputs {
        inputs(&[(0, "pci-0000:01:00.0")])
    }

    fn host(kind: ProbeKind) -> ProbeTarget {
        ProbeTarget::host(kind)
    }

    fn gpu(kind: ProbeKind, index: i32) -> ProbeTarget {
        ProbeTarget::gpu(kind, index)
    }

    fn finished(target: ProbeTarget) -> Event {
        Event::ProbeFinished {
            target,
            reconciled: true,
            verdict: Verdict::Passed,
        }
    }

    fn live(gpus: &[i32]) -> Event {
        Event::SessionsChanged {
            live_gpus: gpus.iter().copied().collect(),
            launching: false,
        }
    }

    fn launching(gpus: &[i32]) -> Event {
        Event::SessionsChanged {
            live_gpus: gpus.iter().copied().collect(),
            launching: true,
        }
    }

    /// Runs every started probe to completion and returns the order they started in.
    fn drain(s: &mut Scheduler, first: Vec<Action>) -> Vec<ProbeTarget> {
        let mut started = Vec::new();
        let mut actions = first;
        loop {
            let next: Vec<ProbeTarget> = actions
                .iter()
                .filter_map(|a| match a {
                    Action::Start(t) => Some(*t),
                    _ => None,
                })
                .collect();
            let Some(&target) = next.first() else {
                return started;
            };
            assert_eq!(next.len(), 1, "two probes started at once: {actions:?}");
            started.push(target);
            actions = s.step(finished(target));
        }
    }

    /// A scheduler that has run its start-up probes on `inputs` and is idle.
    fn settled(inputs: ProbeInputs) -> Scheduler {
        let mut s = Scheduler::new();
        let first = s.step(Event::Registered(inputs));
        drain(&mut s, first);
        assert_eq!(s.running(), None);
        s
    }

    #[test]
    fn nothing_runs_before_registration_completes() {
        let mut s = Scheduler::new();
        assert_eq!(s.step(Event::InputsObserved(one_gpu())), vec![]);
        assert_eq!(s.step(live(&[])), vec![]);
        assert_eq!(s.running(), None);
    }

    #[test]
    fn at_start_every_probe_runs_once_one_at_a_time() {
        let mut s = Scheduler::new();
        let first = s.step(Event::Registered(inputs(&[(0, "a"), (1, "b")])));
        assert_eq!(first, vec![Action::Start(host(Input))]);
        let order = drain(&mut s, first);
        assert_eq!(
            order,
            vec![
                host(Input),
                host(Audio),
                gpu(Media, 0),
                gpu(Media, 1),
                gpu(ApplicationGpu, 0),
                gpu(ApplicationGpu, 1),
            ]
        );
        assert_eq!(
            s.step(live(&[])),
            vec![],
            "nothing re-runs without a trigger"
        );
    }

    #[test]
    fn a_host_with_no_gpu_reports_gpu_probes_not_applicable() {
        let mut s = Scheduler::new();
        let first = s.step(Event::Registered(inputs(&[])));
        assert_eq!(
            first,
            vec![
                Action::NotApplicable(Media),
                Action::NotApplicable(ApplicationGpu),
                Action::Start(host(Input)),
            ]
        );
        assert_eq!(drain(&mut s, first), vec![host(Input), host(Audio)]);
    }

    #[test]
    fn single_flight_holds_while_a_probe_is_running() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(one_gpu()));
        assert_eq!(s.running(), Some(host(Input)));
        let mut changed = one_gpu();
        changed.agent_image = "sha256:agent-b".into();
        assert_eq!(s.step(Event::InputsObserved(changed)), vec![]);
        assert_eq!(s.step(live(&[])), vec![]);
        assert_eq!(s.running(), Some(host(Input)));
    }

    #[test]
    fn a_new_agent_image_reruns_every_probe() {
        let mut s = settled(one_gpu());
        let mut changed = one_gpu();
        changed.agent_image = "sha256:agent-b".into();
        let first = s.step(Event::InputsObserved(changed));
        assert_eq!(
            drain(&mut s, first),
            vec![
                host(Input),
                host(Audio),
                gpu(Media, 0),
                gpu(ApplicationGpu, 0)
            ]
        );
    }

    #[test]
    fn a_driver_change_reruns_the_gpu_probes_only() {
        let mut s = settled(one_gpu());
        let mut changed = one_gpu();
        changed.driver = "nvidia:610.10 volume:def".into();
        let first = s.step(Event::InputsObserved(changed));
        assert_eq!(
            drain(&mut s, first),
            vec![gpu(Media, 0), gpu(ApplicationGpu, 0)]
        );
    }

    #[test]
    fn a_relevant_settings_change_reruns_the_gpu_probes_only() {
        let mut s = settled(one_gpu());
        let mut changed = one_gpu();
        changed.settings = "encoder=nvenc".into();
        let first = s.step(Event::InputsObserved(changed));
        assert_eq!(
            drain(&mut s, first),
            vec![gpu(Media, 0), gpu(ApplicationGpu, 0)]
        );
    }

    #[test]
    fn unchanged_inputs_run_nothing() {
        let mut s = settled(one_gpu());
        assert_eq!(s.step(Event::InputsObserved(one_gpu())), vec![]);
    }

    #[test]
    fn a_new_gpu_is_probed_and_the_others_are_left_alone() {
        let mut s = settled(one_gpu());
        let first = s.step(Event::InputsObserved(inputs(&[
            (0, "pci-0000:01:00.0"),
            (1, "pci-0000:02:00.0"),
        ])));
        assert_eq!(
            drain(&mut s, first),
            vec![gpu(Media, 1), gpu(ApplicationGpu, 1)]
        );
    }

    #[test]
    fn a_vanished_gpu_has_its_checks_forgotten_and_is_not_probed() {
        let mut s = settled(inputs(&[(0, "a"), (1, "b")]));
        let actions = s.step(Event::InputsObserved(inputs(&[(0, "a")])));
        assert_eq!(
            actions,
            vec![
                Action::Forget(gpu(Media, 1)),
                Action::Forget(gpu(ApplicationGpu, 1)),
            ]
        );
    }

    #[test]
    fn a_different_device_under_the_same_index_is_forgotten_then_probed() {
        let mut s = settled(one_gpu());
        let first = s.step(Event::InputsObserved(inputs(&[(0, "pci-0000:09:00.0")])));
        assert_eq!(
            first,
            vec![
                Action::Forget(gpu(Media, 0)),
                Action::Forget(gpu(ApplicationGpu, 0)),
                Action::Start(gpu(Media, 0)),
            ]
        );
    }

    #[test]
    fn losing_the_last_gpu_forgets_its_checks_and_reports_not_applicable() {
        let mut s = settled(one_gpu());
        assert_eq!(
            s.step(Event::InputsObserved(inputs(&[]))),
            vec![
                Action::Forget(gpu(Media, 0)),
                Action::Forget(gpu(ApplicationGpu, 0)),
                Action::NotApplicable(Media),
                Action::NotApplicable(ApplicationGpu),
            ]
        );
    }

    #[test]
    fn gaining_a_first_gpu_forgets_the_not_applicable_checks() {
        let mut s = settled(inputs(&[]));
        let first = s.step(Event::InputsObserved(one_gpu()));
        assert_eq!(
            first,
            vec![
                Action::Forget(host(Media)),
                Action::Forget(host(ApplicationGpu)),
                Action::Start(gpu(Media, 0)),
            ]
        );
    }

    #[test]
    fn a_vanished_gpu_preempts_the_probe_running_on_it() {
        let mut s = settled(inputs(&[(0, "a"), (1, "b")]));
        let mut changed = inputs(&[(0, "a"), (1, "b")]);
        changed.driver = "other".into();
        s.step(Event::InputsObserved(changed.clone()));
        s.step(finished(gpu(Media, 0)));
        assert_eq!(s.running(), Some(gpu(Media, 1)));

        changed.gpus.remove(&1);
        let actions = s.step(Event::InputsObserved(changed));
        assert!(
            actions.contains(&Action::Preempt(gpu(Media, 1))),
            "{actions:?}"
        );
        // The pre-empted probe of a GPU that is gone is not queued again.
        let after = s.step(finished(gpu(Media, 1)));
        assert_eq!(drain(&mut s, after), vec![gpu(ApplicationGpu, 0)]);
    }

    #[test]
    fn a_result_for_a_gpu_replaced_mid_run_is_not_accepted() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(one_gpu()));
        s.step(finished(host(Input)));
        s.step(finished(host(Audio)));
        assert!(s.accepts_result(gpu(Media, 0)));

        s.step(Event::InputsObserved(inputs(&[(0, "pci-0000:09:00.0")])));
        assert!(!s.accepts_result(gpu(Media, 0)), "it probed the old device");
        assert_eq!(
            s.step(finished(gpu(Media, 0))),
            vec![Action::Start(gpu(Media, 0))]
        );
        assert!(s.accepts_result(gpu(Media, 0)));
    }

    #[test]
    fn an_explicable_launch_failure_reruns_the_probes_that_could_explain_it() {
        let mut s = settled(inputs(&[(0, "a"), (1, "b")]));
        let first = s.step(Event::LaunchFailed {
            gpu: 1,
            explains: [Media, Audio].into(),
            codec: None,
        });
        assert_eq!(drain(&mut s, first), vec![host(Audio), gpu(Media, 1)]);
    }

    #[test]
    fn an_inexplicable_launch_failure_runs_nothing() {
        let mut s = settled(one_gpu());
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 0,
                explains: BTreeSet::new(),
                codec: None,
            }),
            vec![]
        );
    }

    #[test]
    fn a_launch_failure_on_an_unknown_gpu_runs_nothing() {
        let mut s = settled(one_gpu());
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 7,
                explains: [Media].into(),
                codec: None,
            }),
            vec![]
        );
    }

    #[test]
    fn a_gpu_with_a_live_session_is_never_probed_until_the_session_ends() {
        let mut s = settled(inputs(&[(0, "a"), (1, "b")]));
        s.step(live(&[0]));
        let mut changed = inputs(&[(0, "a"), (1, "b")]);
        changed.driver = "other".into();
        let first = s.step(Event::InputsObserved(changed));
        assert_eq!(
            drain(&mut s, first),
            vec![gpu(Media, 1), gpu(ApplicationGpu, 1)],
            "only the idle GPU is probed"
        );
        let after = s.step(live(&[]));
        assert_eq!(
            drain(&mut s, after),
            vec![gpu(Media, 0), gpu(ApplicationGpu, 0)]
        );
    }

    #[test]
    fn a_failed_launch_is_probed_only_once_its_gpu_is_free() {
        let mut s = settled(one_gpu());
        s.step(Event::LaunchArrived { gpu: 0 });
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 0,
                explains: [Media].into(),
                codec: None,
            }),
            vec![],
            "the failed session still holds the GPU"
        );
        assert_eq!(s.step(live(&[])), vec![Action::Start(gpu(Media, 0))]);
    }

    #[test]
    fn host_probes_may_run_while_a_session_is_live() {
        let mut s = settled(one_gpu());
        s.step(live(&[0]));
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 0,
                explains: [Input].into(),
                codec: None,
            }),
            vec![Action::Start(host(Input))]
        );
    }

    #[test]
    fn a_launch_preempts_the_running_probe_whichever_gpu_it_lands_on() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(inputs(&[(0, "a"), (1, "b")])));
        s.step(finished(host(Input)));
        s.step(finished(host(Audio)));
        assert_eq!(s.running(), Some(gpu(Media, 0)));

        assert_eq!(
            s.step(Event::LaunchArrived { gpu: 1 }),
            vec![Action::Preempt(gpu(Media, 0))]
        );
        // Asked once; a second launch does not repeat it.
        assert_eq!(s.step(Event::LaunchArrived { gpu: 1 }), vec![]);
        // Nothing starts until the pre-empted probe has reported.
        assert_eq!(s.step(live(&[1])), vec![]);
        assert_eq!(s.running(), Some(gpu(Media, 0)));

        // It is indeterminate, so it runs again; GPU 1 waits for its session.
        let after = s.step(finished(gpu(Media, 0)));
        assert_eq!(after, vec![Action::Start(gpu(Media, 0))]);
        assert_eq!(
            drain(&mut s, after),
            vec![gpu(Media, 0), gpu(ApplicationGpu, 0)]
        );
    }

    #[test]
    fn nothing_starts_while_a_launch_is_in_flight() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(one_gpu()));
        assert_eq!(
            s.step(Event::LaunchArrived { gpu: 0 }),
            vec![Action::Preempt(host(Input))]
        );
        // Pending in the agent, then started but not yet running.
        assert_eq!(s.step(launching(&[0])), vec![]);
        assert_eq!(
            s.step(finished(host(Input))),
            vec![],
            "not even a host probe"
        );
        assert_eq!(s.step(launching(&[0])), vec![]);
        // Running: host probes resume, the GPU's probes wait for the session to end.
        let resumed = s.step(live(&[0]));
        assert_eq!(drain(&mut s, resumed), vec![host(Input), host(Audio)]);
    }

    #[test]
    fn a_rejected_launch_lets_probes_resume_at_once() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(one_gpu()));
        s.step(Event::LaunchArrived { gpu: 0 });
        s.step(finished(host(Input)));
        assert_eq!(s.step(live(&[])), vec![Action::Start(host(Input))]);
    }

    #[test]
    fn a_launch_with_no_probe_running_preempts_nothing() {
        let mut s = settled(one_gpu());
        assert_eq!(s.step(Event::LaunchArrived { gpu: 0 }), vec![]);
    }

    #[test]
    fn a_lost_connection_preempts_and_nothing_starts_until_registered_again() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(one_gpu()));
        assert_eq!(
            s.step(Event::Disconnected),
            vec![Action::Preempt(host(Input))]
        );
        assert_eq!(s.step(finished(host(Input))), vec![]);
        // Reconnecting with unchanged inputs resumes what was pending, nothing more.
        let first = s.step(Event::Registered(one_gpu()));
        assert_eq!(
            drain(&mut s, first),
            vec![
                host(Input),
                host(Audio),
                gpu(Media, 0),
                gpu(ApplicationGpu, 0)
            ]
        );
        let again = s.step(Event::Disconnected);
        assert_eq!(again, vec![]);
        assert_eq!(s.step(Event::Registered(one_gpu())), vec![]);
    }

    #[test]
    fn an_unreconciled_container_probe_is_reconciled_before_its_kind_runs_again() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(one_gpu()));
        s.step(finished(host(Input)));
        assert_eq!(s.running(), Some(host(Audio)));
        let after = s.step(Event::ProbeFinished {
            target: host(Audio),
            reconciled: false,
            verdict: Verdict::Passed,
        });
        // Other kinds carry on.
        assert_eq!(after, vec![Action::Start(gpu(Media, 0))]);
        drain(&mut s, after);

        let retry = s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [Audio].into(),
            codec: None,
        });
        assert_eq!(retry, vec![Action::Reconcile(Audio)]);
        // Reconciliation is inside the single flight.
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 0,
                explains: [Input].into(),
                codec: None,
            }),
            vec![]
        );
        assert_eq!(
            s.step(Event::Reconciled {
                kind: Audio,
                ok: true,
            }),
            vec![Action::Start(host(Input))]
        );
        assert_eq!(
            s.step(finished(host(Input))),
            vec![Action::Start(host(Audio))]
        );
    }

    #[test]
    fn a_failed_reconcile_never_starts_its_kind_and_does_not_spin() {
        let mut s = settled(one_gpu());
        s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [ApplicationGpu].into(),
            codec: None,
        });
        s.step(Event::ProbeFinished {
            target: gpu(ApplicationGpu, 0),
            reconciled: false,
            verdict: Verdict::Passed,
        });
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 0,
                explains: [ApplicationGpu, Media].into(),
                codec: None,
            }),
            vec![Action::Start(gpu(Media, 0))]
        );
        assert_eq!(
            s.step(finished(gpu(Media, 0))),
            vec![Action::Reconcile(ApplicationGpu)]
        );
        assert_eq!(
            s.step(Event::Reconciled {
                kind: ApplicationGpu,
                ok: false,
            }),
            vec![],
            "no immediate retry, and no start under a new identity"
        );
        // The next event tries again.
        assert_eq!(s.step(live(&[])), vec![Action::Reconcile(ApplicationGpu)]);
    }

    #[test]
    fn a_probe_deferred_at_the_encode_gate_waits_for_it_and_others_carry_on() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(one_gpu()));
        s.step(finished(host(Input)));
        s.step(finished(host(Audio)));
        assert_eq!(s.running(), Some(gpu(Media, 0)));

        let after = s.step(Event::ProbeDeferred(gpu(Media, 0)));
        assert_eq!(after, vec![Action::Start(gpu(ApplicationGpu, 0))]);
        assert_eq!(s.step(finished(gpu(ApplicationGpu, 0))), vec![]);
        assert_eq!(s.step(live(&[])), vec![], "no retry until the gate frees");

        assert_eq!(
            s.step(Event::EncodeGateFreed),
            vec![Action::Start(gpu(Media, 0))]
        );
        assert_eq!(s.step(Event::EncodeGateFreed), vec![]);
    }

    #[test]
    fn a_kind_this_agent_cannot_run_is_never_queued_reported_or_forgotten() {
        let mut s = Scheduler::with_kinds(&[Input, Media]);
        let first = s.step(Event::Registered(one_gpu()));
        assert_eq!(drain(&mut s, first), vec![host(Input), gpu(Media, 0)]);
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 0,
                explains: [Audio, ApplicationGpu].into(),
                codec: None,
            }),
            vec![]
        );
        assert_eq!(
            s.step(Event::InputsObserved(inputs(&[]))),
            vec![Action::Forget(gpu(Media, 0)), Action::NotApplicable(Media)]
        );
    }

    #[test]
    fn a_probe_deferred_on_one_connection_runs_on_the_next() {
        let mut s = Scheduler::with_kinds(&[Media]);
        s.step(Event::Registered(one_gpu()));
        s.step(Event::ProbeDeferred(gpu(Media, 0)));
        assert_eq!(s.step(Event::Disconnected), vec![]);
        assert_eq!(
            s.step(Event::Registered(one_gpu())),
            vec![Action::Start(gpu(Media, 0))]
        );
    }

    #[test]
    fn a_stale_finish_report_is_ignored() {
        let mut s = Scheduler::new();
        s.step(Event::Registered(one_gpu()));
        assert_eq!(s.step(finished(gpu(Media, 0))), vec![]);
        assert_eq!(s.running(), Some(host(Input)));
    }

    // ── #300: codec probes ───────────────────────────────────────────────────────────

    use ProbeCodec::{Av1, H265};

    fn codec(index: i32, c: ProbeCodec) -> ProbeTarget {
        ProbeTarget::codec(index, c)
    }

    fn with_codecs(mut inputs: ProbeInputs, codecs: &[(i32, &[ProbeCodec])]) -> ProbeInputs {
        inputs.codecs = codecs
            .iter()
            .map(|(i, c)| (*i, c.iter().copied().collect()))
            .collect();
        inputs
    }

    /// One GPU whose plan builds HEVC and AV1 above the floor.
    fn codec_gpu() -> ProbeInputs {
        with_codecs(one_gpu(), &[(0, &[H265, Av1])])
    }

    /// Like [`drain`], but each probe concludes with `verdict(target)`.
    fn drain_with(
        s: &mut Scheduler,
        first: Vec<Action>,
        verdict: impl Fn(ProbeTarget) -> Verdict,
    ) -> Vec<ProbeTarget> {
        let mut started = Vec::new();
        let mut actions = first;
        while let Some(target) = actions.iter().find_map(|a| match a {
            Action::Start(t) => Some(*t),
            _ => None,
        }) {
            started.push(target);
            actions = s.step(Event::ProbeFinished {
                target,
                reconciled: true,
                verdict: verdict(target),
            });
        }
        started
    }

    fn settled_with(inputs: ProbeInputs) -> Scheduler {
        let mut s = Scheduler::new();
        let first = s.step(Event::Registered(inputs));
        drain(&mut s, first);
        assert_eq!(s.running(), None);
        s
    }

    fn media_failed(s: &mut Scheduler, index: i32) {
        let first = s.step(Event::LaunchFailed {
            gpu: index,
            explains: [Media].into(),
            codec: None,
        });
        drain_with(s, first, |t| {
            if t == gpu(Media, index) {
                Verdict::NotPassed
            } else {
                Verdict::Passed
            }
        });
    }

    #[test]
    fn codec_probes_queue_behind_a_passing_media_probe_and_after_every_blocking_probe() {
        let mut s = Scheduler::new();
        let first = s.step(Event::Registered(with_codecs(
            inputs(&[(0, "a"), (1, "b")]),
            &[(0, &[H265, Av1]), (1, &[H265])],
        )));
        assert_eq!(
            drain(&mut s, first),
            vec![
                host(Input),
                host(Audio),
                gpu(Media, 0),
                gpu(Media, 1),
                gpu(ApplicationGpu, 0),
                gpu(ApplicationGpu, 1),
                codec(0, H265),
                codec(0, Av1),
                codec(1, H265),
            ]
        );
        assert_eq!(
            s.step(live(&[])),
            vec![],
            "nothing re-runs without a trigger"
        );
    }

    #[test]
    fn codec_probes_are_dropped_after_a_failing_media_probe() {
        let mut s = Scheduler::new();
        let first = s.step(Event::Registered(with_codecs(
            inputs(&[(0, "a"), (1, "b")]),
            &[(0, &[H265, Av1]), (1, &[H265, Av1])],
        )));
        let order = drain_with(&mut s, first, |t| {
            if t == gpu(Media, 0) {
                Verdict::NotPassed
            } else {
                Verdict::Passed
            }
        });
        assert!(!order.contains(&codec(0, H265)), "{order:?}");
        assert!(!order.contains(&codec(0, Av1)), "{order:?}");
        assert!(order.contains(&codec(1, H265)) && order.contains(&codec(1, Av1)));
    }

    #[test]
    fn a_media_probe_that_never_concluded_runs_no_codec_probe() {
        let mut s = Scheduler::with_kinds(&[Media]);
        let first = s.step(Event::Registered(codec_gpu()));
        assert_eq!(
            drain_with(&mut s, first, |_| Verdict::Indeterminate),
            vec![gpu(Media, 0)]
        );
    }

    #[test]
    fn a_codec_probe_waits_while_its_media_probe_is_deferred() {
        let mut s = Scheduler::with_kinds(&[Media, ApplicationGpu]);
        s.step(Event::Registered(codec_gpu()));
        assert_eq!(s.running(), Some(gpu(Media, 0)));
        let after = s.step(Event::ProbeDeferred(gpu(Media, 0)));
        assert_eq!(after, vec![Action::Start(gpu(ApplicationGpu, 0))]);
        assert_eq!(
            s.step(finished(gpu(ApplicationGpu, 0))),
            vec![],
            "the codec probes wait for the media probe"
        );
        let freed = s.step(Event::EncodeGateFreed);
        assert_eq!(
            drain(&mut s, freed),
            vec![gpu(Media, 0), codec(0, H265), codec(0, Av1)]
        );
    }

    #[test]
    fn codec_probes_rerun_on_every_media_trigger() {
        let changes: [fn(&mut ProbeInputs); 3] = [
            |i| i.agent_image = "sha256:agent-b".into(),
            |i| i.driver = "nvidia:610.10 volume:def".into(),
            |i| i.settings = "encoder=nvenc".into(),
        ];
        for change in changes {
            let mut s = settled_with(codec_gpu());
            let mut changed = codec_gpu();
            change(&mut changed);
            let first = s.step(Event::InputsObserved(changed));
            let order = drain(&mut s, first);
            let at = order.iter().position(|t| *t == gpu(Media, 0)).unwrap();
            assert_eq!(
                &order[order.len() - 2..],
                &[codec(0, H265), codec(0, Av1)],
                "{order:?}"
            );
            assert!(at < order.len() - 2);
        }
    }

    #[test]
    fn a_replaced_gpu_forgets_its_codec_checks_and_is_probed_again() {
        let mut s = settled_with(codec_gpu());
        let first = s.step(Event::InputsObserved(with_codecs(
            inputs(&[(0, "pci-0000:09:00.0")]),
            &[(0, &[H265, Av1])],
        )));
        for forgotten in [
            gpu(Media, 0),
            gpu(ApplicationGpu, 0),
            codec(0, H265),
            codec(0, Av1),
        ] {
            assert!(first.contains(&Action::Forget(forgotten)), "{first:?}");
        }
        assert_eq!(
            drain(&mut s, first),
            vec![
                gpu(Media, 0),
                gpu(ApplicationGpu, 0),
                codec(0, H265),
                codec(0, Av1)
            ]
        );
    }

    #[test]
    fn a_vanished_gpu_forgets_its_codec_checks() {
        let mut s = settled_with(with_codecs(
            inputs(&[(0, "a"), (1, "b")]),
            &[(0, &[Av1]), (1, &[Av1])],
        ));
        let actions = s.step(Event::InputsObserved(with_codecs(
            inputs(&[(0, "a")]),
            &[(0, &[Av1])],
        )));
        assert_eq!(
            actions,
            vec![
                Action::Forget(gpu(Media, 1)),
                Action::Forget(gpu(ApplicationGpu, 1)),
                Action::Forget(codec(1, Av1)),
            ]
        );
    }

    #[test]
    fn a_launch_failure_naming_a_gpu_and_codec_requeues_that_codec_probe_only() {
        let mut s = settled_with(with_codecs(
            inputs(&[(0, "a"), (1, "b")]),
            &[(0, &[H265, Av1]), (1, &[H265, Av1])],
        ));
        let first = s.step(Event::LaunchFailed {
            gpu: 1,
            explains: [Media].into(),
            codec: Some(Av1),
        });
        assert_eq!(drain(&mut s, first), vec![gpu(Media, 1), codec(1, Av1)]);
    }

    #[test]
    fn a_launch_failure_selects_no_codec_probe_unless_media_explains_it() {
        let mut s = settled_with(codec_gpu());
        let first = s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [ApplicationGpu].into(),
            codec: Some(Av1),
        });
        assert_eq!(drain(&mut s, first), vec![gpu(ApplicationGpu, 0)]);

        let first = s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [Media].into(),
            codec: None,
        });
        assert_eq!(drain(&mut s, first), vec![gpu(Media, 0)]);
    }

    #[test]
    fn a_launch_failure_for_a_codec_the_gpu_does_not_plan_queues_no_codec_probe() {
        let mut s = settled_with(with_codecs(one_gpu(), &[(0, &[H265])]));
        let first = s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [Media].into(),
            codec: Some(Av1),
        });
        assert_eq!(drain(&mut s, first), vec![gpu(Media, 0)]);
    }

    /// Codec evidence must come from the current stack: an identity trigger forgets the
    /// held codec checks and the codec probes wait for a fresh floor pass.
    #[test]
    fn an_identity_trigger_forgets_codec_checks_and_requeues_them_behind_the_floor() {
        let changes: [fn(&mut ProbeInputs); 3] = [
            |i| i.agent_image = "sha256:agent-b".into(),
            |i| i.driver = "nvidia:610.10 volume:def".into(),
            |i| i.settings = "encoder=nvenc".into(),
        ];
        for change in changes {
            let mut s = settled_with(codec_gpu());
            let mut changed = codec_gpu();
            change(&mut changed);
            let first = s.step(Event::InputsObserved(changed));
            for forgotten in [codec(0, H265), codec(0, Av1)] {
                assert!(first.contains(&Action::Forget(forgotten)), "{first:?}");
            }
            assert!(
                !first.contains(&Action::Forget(gpu(Media, 0))),
                "the floor keeps its retained verdict"
            );
            let order = drain(&mut s, first);
            let floor = order.iter().position(|t| *t == gpu(Media, 0)).unwrap();
            assert!(
                order.ends_with(&[codec(0, H265), codec(0, Av1)]),
                "{order:?}"
            );
            assert!(floor < order.len() - 2);
        }
    }

    #[test]
    fn after_an_identity_trigger_an_indeterminate_floor_runs_no_codec_probe() {
        let mut s = settled_with(codec_gpu());
        let mut changed = codec_gpu();
        changed.driver = "nvidia:610.10 volume:def".into();
        let first = s.step(Event::InputsObserved(changed));
        let order = drain_with(&mut s, first, |t| {
            if t == gpu(Media, 0) {
                Verdict::Indeterminate
            } else {
                Verdict::Passed
            }
        });
        assert!(!order.contains(&codec(0, H265)), "{order:?}");
        assert!(!order.contains(&codec(0, Av1)), "{order:?}");
        // A fresh pass later queues them all.
        let first = s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [Media].into(),
            codec: None,
        });
        assert_eq!(
            drain(&mut s, first),
            vec![gpu(Media, 0), codec(0, H265), codec(0, Av1)]
        );
    }

    /// A floor probe that was running on the old stack when the trigger arrived does not
    /// count as the fresh pass.
    #[test]
    fn a_floor_pass_from_a_run_that_straddled_an_identity_trigger_is_not_fresh() {
        let mut s = Scheduler::with_kinds(&[Media]);
        s.step(Event::Registered(codec_gpu()));
        assert_eq!(s.running(), Some(gpu(Media, 0)));
        let mut changed = codec_gpu();
        changed.driver = "nvidia:610.10 volume:def".into();
        s.step(Event::InputsObserved(changed));
        let after = s.step(finished(gpu(Media, 0)));
        assert_eq!(
            after,
            vec![Action::Start(gpu(Media, 0))],
            "re-run on the new stack"
        );
        assert_eq!(
            s.step(Event::ProbeFinished {
                target: gpu(Media, 0),
                reconciled: true,
                verdict: Verdict::Indeterminate,
            }),
            vec![],
            "no fresh pass, no codec probe"
        );
    }

    #[test]
    fn a_pinned_out_gpu_not_applicable_floor_drops_its_codec_probes() {
        let mut s = Scheduler::with_kinds(&[Media]);
        let first = s.step(Event::Registered(codec_gpu()));
        assert_eq!(
            drain_with(&mut s, first, |_| super::super::orchestrator::verdict_of(
                &super::super::outcome::ProbeOutcome::NotApplicable {
                    summary: "pinned elsewhere".into(),
                }
            )),
            vec![gpu(Media, 0)]
        );
    }

    /// The retained media verdict gates: an indeterminate re-run not caused by an identity
    /// trigger (here a launch failure) leaves the last pass standing for the codec probe.
    #[test]
    fn an_indeterminate_media_rerun_leaves_the_last_pass_standing_for_the_codec_probe() {
        let mut s = settled_with(codec_gpu());
        let first = s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [Media].into(),
            codec: Some(Av1),
        });
        let order = drain_with(&mut s, first, |t| {
            if t == gpu(Media, 0) {
                Verdict::Indeterminate
            } else {
                Verdict::Passed
            }
        });
        assert_eq!(order, vec![gpu(Media, 0), codec(0, Av1)]);
    }

    #[test]
    fn a_gpu_whose_media_probe_recovers_has_all_its_codec_probes_queued() {
        let mut s = settled_with(codec_gpu());
        media_failed(&mut s, 0);
        let first = s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [Media].into(),
            codec: None,
        });
        assert_eq!(
            drain(&mut s, first),
            vec![gpu(Media, 0), codec(0, H265), codec(0, Av1)]
        );
    }

    #[test]
    fn a_codec_the_plan_newly_builds_is_probed_and_one_it_dropped_is_forgotten() {
        let mut s = settled_with(with_codecs(one_gpu(), &[(0, &[H265])]));
        let first = s.step(Event::InputsObserved(with_codecs(
            one_gpu(),
            &[(0, &[Av1])],
        )));
        assert!(first.contains(&Action::Forget(codec(0, H265))), "{first:?}");
        assert_eq!(drain(&mut s, first), vec![codec(0, Av1)]);
    }

    #[test]
    fn a_codec_probe_never_runs_on_a_gpu_with_a_live_session_and_a_launch_preempts_it() {
        let mut s = Scheduler::with_kinds(&[Media]);
        s.step(Event::Registered(codec_gpu()));
        assert_eq!(
            s.step(finished(gpu(Media, 0))),
            vec![Action::Start(codec(0, H265))]
        );
        assert_eq!(
            s.step(Event::LaunchArrived { gpu: 0 }),
            vec![Action::Preempt(codec(0, H265))]
        );
        assert_eq!(
            s.step(Event::ProbeFinished {
                target: codec(0, H265),
                reconciled: true,
                verdict: Verdict::Indeterminate,
            }),
            vec![]
        );
        assert_eq!(s.step(live(&[0])), vec![], "the GPU has a live session");
        let after = s.step(live(&[]));
        assert_eq!(
            drain(&mut s, after),
            vec![codec(0, H265), codec(0, Av1)],
            "the pre-empted codec probe runs again"
        );
    }

    /// #301: every identity a codec pass depends on is in the stamp; the codec plan is
    /// not (a codec dropped from the plan loses its check through `Forget` instead).
    #[test]
    fn evidence_stamp_changes_with_every_identity_it_depends_on() {
        let base = one_gpu();
        let stamp = base.evidence_stamp(0).unwrap();
        assert_eq!(base.evidence_stamp(1), None, "no GPU, nothing proven");

        let mut planned = base.clone();
        planned.codecs.insert(0, [ProbeCodec::H265].into());
        assert_eq!(planned.evidence_stamp(0), Some(stamp.clone()));

        let changes: [fn(&mut ProbeInputs); 4] = [
            |i| i.agent_image = "sha256:agent-b".into(),
            |i| i.driver = "nvidia:610.57.04 volume:def".into(),
            |i| i.settings = "encoder=nvenc".into(),
            |i| {
                i.gpus.insert(0, "pci-0000:02:00.0".into());
            },
        ];
        for change in changes {
            let mut other = base.clone();
            change(&mut other);
            assert_ne!(other.evidence_stamp(0), Some(stamp.clone()), "{other:?}");
        }
    }
}
