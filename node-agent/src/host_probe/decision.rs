//! When host probes run (spec #252 "When probes run", "Probe lifecycle"). A state
//! machine with no I/O and no clock: the orchestrator feeds it events and carries out
//! the actions it returns.

use std::collections::{BTreeMap, BTreeSet};

use super::{ProbeKind, ProbeTarget};

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
    /// A launch failed in a way these probe kinds could explain.
    LaunchFailed {
        gpu: i32,
        explains: BTreeSet<ProbeKind>,
    },
    /// `reconciled` is false when a container probe's stop or cleanup outcome is not
    /// known. Always true for a child-process probe.
    ProbeFinished {
        target: ProbeTarget,
        reconciled: bool,
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
        }
    }

    pub fn running(&self) -> Option<ProbeTarget> {
        self.running.map(|r| r.target)
    }

    /// False once the target's GPU is gone: a late result must not be recorded.
    pub fn exists(&self, target: ProbeTarget) -> bool {
        match target.gpu {
            _ if !self.kinds.contains(&target.kind) => false,
            None => !target.kind.per_gpu(),
            Some(index) => self
                .inputs
                .as_ref()
                .is_some_and(|i| i.gpus.contains_key(&index)),
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
            Event::LaunchFailed { gpu, explains } => {
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
            }
            Event::ProbeFinished { target, reconciled } => {
                if let Some(running) = self.running.filter(|r| r.target == target) {
                    self.running = None;
                    if running.preempted && self.exists(target) {
                        self.pending.insert(target);
                    }
                    if !reconciled {
                        self.unreconciled.insert(target.kind);
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
                }
            }
        }
    }

    fn observe(&mut self, new: ProbeInputs, actions: &mut Vec<Action>) {
        let Some(old) = self.inputs.replace(new.clone()) else {
            return;
        };
        let per_gpu: Vec<ProbeKind> = self.kinds.iter().copied().filter(|k| k.per_gpu()).collect();

        for (index, identity) in &old.gpus {
            if new.gpus.get(index) == Some(identity) {
                continue;
            }
            for &kind in &per_gpu {
                let target = ProbeTarget::gpu(kind, *index);
                self.pending.remove(&target);
                self.gate_waiting.remove(&target);
                if let Some(running) = self.running.as_mut().filter(|r| r.target == target) {
                    running.discard = true;
                    self.preempt(actions);
                }
                actions.push(Action::Forget(target));
            }
        }
        if old.gpus.is_empty() && !new.gpus.is_empty() {
            for &kind in &per_gpu {
                actions.push(Action::Forget(ProbeTarget::host(kind)));
            }
        }

        if new.agent_image != old.agent_image {
            self.queue_kinds(&ProbeKind::ALL, &new, actions);
        } else if new.driver != old.driver
            || new.settings != old.settings
            || (new.gpus.is_empty() && !old.gpus.is_empty())
        {
            self.queue_kinds(&per_gpu, &new, actions);
        } else {
            for (index, identity) in &new.gpus {
                if old.gpus.get(index) != Some(identity) {
                    for &kind in &per_gpu {
                        self.pending.insert(ProbeTarget::gpu(kind, *index));
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
        let candidates: Vec<ProbeTarget> = self.pending.iter().copied().collect();
        for target in candidates {
            if target.gpu.is_some_and(|g| self.live_gpus.contains(&g)) {
                continue;
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
        });
        // Other kinds carry on.
        assert_eq!(after, vec![Action::Start(gpu(Media, 0))]);
        drain(&mut s, after);

        let retry = s.step(Event::LaunchFailed {
            gpu: 0,
            explains: [Audio].into(),
        });
        assert_eq!(retry, vec![Action::Reconcile(Audio)]);
        // Reconciliation is inside the single flight.
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 0,
                explains: [Input].into(),
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
        });
        s.step(Event::ProbeFinished {
            target: gpu(ApplicationGpu, 0),
            reconciled: false,
        });
        assert_eq!(
            s.step(Event::LaunchFailed {
                gpu: 0,
                explains: [ApplicationGpu, Media].into(),
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
}
