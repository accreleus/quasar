//! Carries out the scheduler's decisions. One task owns the [`Scheduler`]; probes run
//! in their own tasks, so the loop is never blocked and a launch is never delayed.

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::time::SystemTime;

use tokio::sync::{mpsc, watch};
use tracing::{error, info};

use super::decision::{Action, Event, ProbeInputs, Scheduler};
use super::outcome::ProbeOutcome;
use super::{ProbeKind, ProbeTarget};

pub type BoxFuture<T> = Pin<Box<dyn Future<Output = T> + Send>>;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RunEnd {
    /// `reconciled` is false when a container's stop or cleanup outcome is unknown.
    Concluded {
        outcome: ProbeOutcome,
        reconciled: bool,
    },
    /// Never ran: a template warm-up holds the encode gate.
    Deferred,
}

pub trait ProbeRunner: Send + Sync + 'static {
    /// Must return promptly once `preempt` is true, with an indeterminate outcome.
    fn run(&self, target: ProbeTarget, preempt: watch::Receiver<bool>) -> BoxFuture<RunEnd>;
    /// Finish earlier container work of this kind under its original identity.
    fn reconcile(&self, kind: ProbeKind) -> BoxFuture<bool>;
}

/// What the agent loop applies to its `ReadinessReport`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ReportUpdate {
    Record {
        target: ProbeTarget,
        outcome: ProbeOutcome,
        observed_at: SystemTime,
    },
    NotApplicable {
        kind: ProbeKind,
        observed_at: SystemTime,
    },
    Forget(ProbeTarget),
}

/// The loop's internal channel. Carries external events plus the results of the
/// tasks the loop itself spawned, so the scheduler is only ever driven from one
/// place and a probe is never awaited inline.
#[derive(Debug)]
enum Msg {
    External(Event),
    RunEnded {
        target: ProbeTarget,
        /// `Err` is the panic text: the probe task itself panicked or was cancelled.
        end: Result<RunEnd, String>,
    },
    ReconcileEnded {
        kind: ProbeKind,
        ok: bool,
    },
}

/// Every method returns at once: the launch path calls these.
#[derive(Debug, Clone)]
pub struct ProbeHandle {
    tx: mpsc::UnboundedSender<Msg>,
}

impl ProbeHandle {
    pub fn registered(&self, inputs: ProbeInputs) {
        self.send(Event::Registered(inputs));
    }

    pub fn disconnected(&self) {
        self.send(Event::Disconnected);
    }

    pub fn inputs_observed(&self, inputs: ProbeInputs) {
        self.send(Event::InputsObserved(inputs));
    }

    pub fn launch_arrived(&self, gpu: i32) {
        self.send(Event::LaunchArrived { gpu });
    }

    pub fn sessions_changed(&self, live_gpus: std::collections::BTreeSet<i32>) {
        self.send(Event::SessionsChanged { live_gpus });
    }

    pub fn launch_failed(&self, gpu: i32, explains: std::collections::BTreeSet<ProbeKind>) {
        self.send(Event::LaunchFailed { gpu, explains });
    }

    pub fn encode_gate_freed(&self) {
        self.send(Event::EncodeGateFreed);
    }

    fn send(&self, event: Event) {
        // The orchestrator lives as long as the process; a closed channel is shutdown.
        let _ = self.tx.send(Msg::External(event));
    }

    /// A handle with no scheduler behind it, for a caller (`agent.rs`'s tests) that
    /// only wants to assert what it SENT — a tiny forwarding task un-wraps `Msg` back
    /// to the `Event` a real scheduler would have consumed.
    #[cfg(test)]
    pub(crate) fn detached() -> (ProbeHandle, mpsc::UnboundedReceiver<Event>) {
        let (tx, mut rx) = mpsc::unbounded_channel::<Msg>();
        let (etx, erx) = mpsc::unbounded_channel::<Event>();
        tokio::spawn(async move {
            while let Some(Msg::External(event)) = rx.recv().await {
                let _ = etx.send(event);
            }
        });
        (ProbeHandle { tx }, erx)
    }
}

/// Awaits `fut` on its own task so a panic surfaces as a `JoinError` here rather than
/// unwinding into the loop task.
async fn await_run(fut: BoxFuture<RunEnd>) -> Result<RunEnd, String> {
    match tokio::spawn(fut).await {
        Ok(end) => Ok(end),
        Err(e) => Err(e.to_string()),
    }
}

/// What the agent loop's connection code applies a [`ReportUpdate`] as, folding a host
/// probe's result into the report every capacity message reads.
pub fn apply(report: &mut crate::readiness::report::ReadinessReport, update: ReportUpdate) {
    match update {
        ReportUpdate::Record {
            target,
            outcome,
            observed_at,
        } => super::outcome::record(report, target, outcome, observed_at),
        ReportUpdate::NotApplicable { kind, observed_at } => {
            super::outcome::record_not_applicable(report, kind, observed_at)
        }
        ReportUpdate::Forget(target) => super::outcome::forget(report, target),
    }
}

pub fn spawn(runner: Arc<dyn ProbeRunner>) -> (ProbeHandle, mpsc::UnboundedReceiver<ReportUpdate>) {
    spawn_with_kinds(runner, &ProbeKind::ALL)
}

pub fn spawn_with_kinds(
    runner: Arc<dyn ProbeRunner>,
    kinds: &[ProbeKind],
) -> (ProbeHandle, mpsc::UnboundedReceiver<ReportUpdate>) {
    let kinds = kinds.to_vec();
    let (tx, mut rx) = mpsc::unbounded_channel::<Msg>();
    let (update_tx, update_rx) = mpsc::unbounded_channel::<ReportUpdate>();
    let handle = ProbeHandle { tx: tx.clone() };

    tokio::spawn(async move {
        let mut scheduler = Scheduler::with_kinds(&kinds);
        // The current run's pre-empt flag. Single-flight, so at most one at a time.
        let mut preempt: Option<watch::Sender<bool>> = None;

        while let Some(msg) = rx.recv().await {
            let actions = match msg {
                Msg::External(event) => scheduler.step(event),
                Msg::RunEnded { target, end } => {
                    preempt = None;
                    match end {
                        Ok(RunEnd::Concluded {
                            outcome,
                            reconciled,
                        }) => {
                            info!(
                                token = "host-probe-finished",
                                target = %target.check_id(),
                                "host probe finished"
                            );
                            // Checked against the scheduler's current inputs: a GPU that
                            // vanished mid-run must not have its late result recorded.
                            if scheduler.accepts_result(target) {
                                let _ = update_tx.send(ReportUpdate::Record {
                                    target,
                                    outcome,
                                    observed_at: SystemTime::now(),
                                });
                            }
                            scheduler.step(Event::ProbeFinished { target, reconciled })
                        }
                        Ok(RunEnd::Deferred) => scheduler.step(Event::ProbeDeferred(target)),
                        Err(panic_text) => {
                            error!(
                                token = "host-probe-task-panicked",
                                target = %target.check_id(),
                                error = %panic_text,
                                "the host probe task ended unexpectedly"
                            );
                            if scheduler.accepts_result(target) {
                                let _ = update_tx.send(ReportUpdate::Record {
                                    target,
                                    outcome: ProbeOutcome::Indeterminate {
                                        reason: "The host probe task ended unexpectedly; \
                                            see the agent log"
                                            .into(),
                                    },
                                    observed_at: SystemTime::now(),
                                });
                            }
                            // A container probe that died mid-way may have left a
                            // container behind; it must be reconciled before its kind
                            // runs again.
                            scheduler.step(Event::ProbeFinished {
                                target,
                                reconciled: !target.kind.is_container(),
                            })
                        }
                    }
                }
                Msg::ReconcileEnded { kind, ok } => scheduler.step(Event::Reconciled { kind, ok }),
            };

            for action in actions {
                match action {
                    Action::Start(target) => {
                        info!(
                            token = "host-probe-started",
                            target = %target.check_id(),
                            "starting host probe"
                        );
                        let (ptx, prx) = watch::channel(false);
                        preempt = Some(ptx);
                        let runner = runner.clone();
                        let tx = tx.clone();
                        tokio::spawn(async move {
                            let end = await_run(runner.run(target, prx)).await;
                            let _ = tx.send(Msg::RunEnded { target, end });
                        });
                    }
                    Action::Preempt(target) => {
                        info!(
                            token = "host-probe-preempted",
                            target = %target.check_id(),
                            "pre-empting the running host probe"
                        );
                        if let Some(ptx) = &preempt {
                            let _ = ptx.send(true);
                        }
                    }
                    Action::Reconcile(kind) => {
                        let runner = runner.clone();
                        let tx = tx.clone();
                        tokio::spawn(async move {
                            let ok = match tokio::spawn(runner.reconcile(kind)).await {
                                Ok(ok) => ok,
                                Err(e) => {
                                    error!(
                                        token = "host-probe-reconcile-panicked",
                                        kind = ?kind,
                                        error = %e,
                                        "the reconcile task ended unexpectedly"
                                    );
                                    false
                                }
                            };
                            let _ = tx.send(Msg::ReconcileEnded { kind, ok });
                        });
                    }
                    Action::NotApplicable(kind) => {
                        let _ = update_tx.send(ReportUpdate::NotApplicable {
                            kind,
                            observed_at: SystemTime::now(),
                        });
                    }
                    Action::Forget(target) => {
                        info!(
                            token = "host-probe-forgotten",
                            target = %target.check_id(),
                            "forgetting a host probe check"
                        );
                        let _ = update_tx.send(ReportUpdate::Forget(target));
                    }
                }
            }
        }
    });

    (handle, update_rx)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;
    use std::sync::Mutex;
    use std::time::Duration;
    use tokio::sync::oneshot;
    use ProbeKind::{ApplicationGpu, Audio, Input, Media};

    const BOUND: Duration = Duration::from_secs(5);

    struct Started {
        target: ProbeTarget,
        preempt: watch::Receiver<bool>,
        finish: oneshot::Sender<RunEnd>,
    }

    /// Each run parks until the test finishes it, or ends indeterminate on pre-emption.
    struct FakeRunner {
        started: mpsc::UnboundedSender<Started>,
        reconciles: Mutex<Vec<ProbeKind>>,
    }

    impl ProbeRunner for FakeRunner {
        fn run(&self, target: ProbeTarget, preempt: watch::Receiver<bool>) -> BoxFuture<RunEnd> {
            let (finish, finished) = oneshot::channel();
            let mut watching = preempt.clone();
            self.started
                .send(Started {
                    target,
                    preempt,
                    finish,
                })
                .unwrap();
            Box::pin(async move {
                tokio::select! {
                    end = finished => end.expect("the test dropped a run without finishing it"),
                    _ = watching.wait_for(|p| *p) => RunEnd::Concluded {
                        outcome: ProbeOutcome::Indeterminate { reason: "pre-empted".into() },
                        reconciled: true,
                    },
                }
            })
        }

        fn reconcile(&self, kind: ProbeKind) -> BoxFuture<bool> {
            self.reconciles.lock().unwrap().push(kind);
            Box::pin(async { true })
        }
    }

    struct Rig {
        handle: ProbeHandle,
        updates: mpsc::UnboundedReceiver<ReportUpdate>,
        started: mpsc::UnboundedReceiver<Started>,
        runner: Arc<FakeRunner>,
    }

    fn rig() -> Rig {
        let (started_tx, started) = mpsc::unbounded_channel();
        let runner = Arc::new(FakeRunner {
            started: started_tx,
            reconciles: Mutex::new(Vec::new()),
        });
        let (handle, updates) = spawn(runner.clone());
        Rig {
            handle,
            updates,
            started,
            runner,
        }
    }

    impl Rig {
        async fn next_start(&mut self) -> Started {
            tokio::time::timeout(BOUND, self.started.recv())
                .await
                .expect("no probe started")
                .unwrap()
        }

        async fn next_update(&mut self) -> ReportUpdate {
            tokio::time::timeout(BOUND, self.updates.recv())
                .await
                .expect("no report update")
                .unwrap()
        }

        async fn nothing_starts(&mut self) {
            assert!(
                tokio::time::timeout(Duration::from_millis(150), self.started.recv())
                    .await
                    .is_err(),
                "a probe started"
            );
        }
    }

    fn inputs(gpus: &[i32]) -> ProbeInputs {
        ProbeInputs {
            agent_image: "sha256:a".into(),
            driver: "d".into(),
            gpus: gpus
                .iter()
                .map(|g| (*g, format!("pci-{g}")))
                .collect::<BTreeMap<_, _>>(),
            settings: "s".into(),
        }
    }

    fn pass(text: &str) -> RunEnd {
        RunEnd::Concluded {
            outcome: ProbeOutcome::Pass {
                summary: text.into(),
            },
            reconciled: true,
        }
    }

    #[tokio::test]
    async fn results_are_reported_with_their_observation_time_and_probes_run_in_turn() {
        let mut rig = rig();
        let before = SystemTime::now();
        rig.handle.registered(inputs(&[0]));

        let mut order = Vec::new();
        for _ in 0..4 {
            let run = rig.next_start().await;
            order.push(run.target);
            rig.nothing_starts().await;
            run.finish.send(pass("ok")).unwrap();
            match rig.next_update().await {
                ReportUpdate::Record {
                    target,
                    outcome,
                    observed_at,
                } => {
                    assert_eq!(target, run.target);
                    assert!(matches!(outcome, ProbeOutcome::Pass { .. }));
                    assert!(observed_at >= before);
                }
                other => panic!("{other:?}"),
            }
        }
        assert_eq!(
            order,
            vec![
                ProbeTarget::host(Input),
                ProbeTarget::host(Audio),
                ProbeTarget::gpu(Media, 0),
                ProbeTarget::gpu(ApplicationGpu, 0),
            ]
        );
        rig.nothing_starts().await;
    }

    #[tokio::test]
    async fn a_host_with_no_gpu_reports_the_gpu_probes_not_applicable() {
        let mut rig = rig();
        rig.handle.registered(inputs(&[]));
        let mut kinds = Vec::new();
        for _ in 0..2 {
            match rig.next_update().await {
                ReportUpdate::NotApplicable { kind, .. } => kinds.push(kind),
                other => panic!("{other:?}"),
            }
        }
        assert_eq!(kinds, vec![Media, ApplicationGpu]);
    }

    #[tokio::test]
    async fn a_launch_preempts_the_running_probe_which_reports_indeterminate_and_runs_again() {
        let mut rig = rig();
        rig.handle.registered(inputs(&[0]));
        let run = rig.next_start().await;
        assert!(!*run.preempt.borrow());

        rig.handle.launch_arrived(0);
        let mut preempt = run.preempt.clone();
        tokio::time::timeout(BOUND, preempt.wait_for(|p| *p))
            .await
            .expect("the running probe was never told to stop")
            .unwrap();

        match rig.next_update().await {
            ReportUpdate::Record {
                target, outcome, ..
            } => {
                assert_eq!(target, ProbeTarget::host(Input));
                assert!(matches!(outcome, ProbeOutcome::Indeterminate { .. }));
            }
            other => panic!("{other:?}"),
        }
        let again = rig.next_start().await;
        assert_eq!(again.target, ProbeTarget::host(Input));
        assert!(!*again.preempt.borrow(), "a fresh run starts un-pre-empted");
        drop(run.finish);
        again.finish.send(pass("ok")).unwrap();
    }

    #[tokio::test]
    async fn a_result_for_a_gpu_that_vanished_mid_run_is_never_recorded() {
        let mut rig = rig();
        rig.handle.registered(inputs(&[0]));
        for _ in 0..2 {
            let run = rig.next_start().await;
            run.finish.send(pass("ok")).unwrap();
            rig.next_update().await;
        }
        let media = rig.next_start().await;
        assert_eq!(media.target, ProbeTarget::gpu(Media, 0));

        rig.handle.inputs_observed(inputs(&[]));
        let mut seen = Vec::new();
        for _ in 0..4 {
            seen.push(rig.next_update().await);
        }
        assert!(seen.contains(&ReportUpdate::Forget(ProbeTarget::gpu(Media, 0))));
        assert!(seen.contains(&ReportUpdate::Forget(ProbeTarget::gpu(ApplicationGpu, 0))));
        assert!(
            !seen
                .iter()
                .any(|u| matches!(u, ReportUpdate::Record { .. })),
            "{seen:?}"
        );
        assert!(
            tokio::time::timeout(Duration::from_millis(200), rig.updates.recv())
                .await
                .is_err(),
            "the vanished GPU's late result was recorded"
        );
    }

    #[tokio::test]
    async fn a_deferred_probe_records_nothing_and_runs_once_the_encode_gate_frees() {
        let mut rig = rig();
        rig.handle.registered(inputs(&[0]));
        for _ in 0..2 {
            let run = rig.next_start().await;
            run.finish.send(pass("ok")).unwrap();
            rig.next_update().await;
        }
        let media = rig.next_start().await;
        media.finish.send(RunEnd::Deferred).unwrap();

        let app = rig.next_start().await;
        assert_eq!(app.target, ProbeTarget::gpu(ApplicationGpu, 0));
        app.finish.send(pass("ok")).unwrap();
        assert!(matches!(
            rig.next_update().await,
            ReportUpdate::Record { target, .. } if target == ProbeTarget::gpu(ApplicationGpu, 0)
        ));
        rig.nothing_starts().await;

        rig.handle.encode_gate_freed();
        assert_eq!(rig.next_start().await.target, ProbeTarget::gpu(Media, 0));
    }

    #[tokio::test]
    async fn an_unreconciled_container_probe_is_reconciled_before_its_kind_runs_again() {
        let mut rig = rig();
        rig.handle.registered(inputs(&[]));
        rig.next_update().await;
        rig.next_update().await;
        let input = rig.next_start().await;
        input.finish.send(pass("ok")).unwrap();
        rig.next_update().await;
        let audio = rig.next_start().await;
        audio
            .finish
            .send(RunEnd::Concluded {
                outcome: ProbeOutcome::Indeterminate {
                    reason: "stop outcome unknown".into(),
                },
                reconciled: false,
            })
            .unwrap();
        rig.next_update().await;
        assert!(rig.runner.reconciles.lock().unwrap().is_empty());

        rig.handle.launch_failed(0, [Audio].into());
        let again = rig.next_start().await;
        assert_eq!(again.target, ProbeTarget::host(Audio));
        assert_eq!(*rig.runner.reconciles.lock().unwrap(), vec![Audio]);
    }

    #[test]
    fn apply_maps_each_update_kind_onto_the_report() {
        let mut report = crate::readiness::report::ReadinessReport::default();
        let now = SystemTime::now();
        apply(
            &mut report,
            ReportUpdate::Record {
                target: ProbeTarget::host(Input),
                outcome: ProbeOutcome::Pass {
                    summary: "ok".into(),
                },
                observed_at: now,
            },
        );
        assert_eq!(
            report.retained("input_probe").map(|c| c.status.as_str()),
            Some(crate::readiness::PASS)
        );

        apply(
            &mut report,
            ReportUpdate::NotApplicable {
                kind: Media,
                observed_at: now,
            },
        );
        assert_eq!(
            report.retained("media_probe").map(|c| c.status.as_str()),
            Some(crate::readiness::SKIP)
        );

        apply(&mut report, ReportUpdate::Forget(ProbeTarget::host(Input)));
        assert!(report.retained("input_probe").is_none());
    }

    struct PanickingRunner;
    impl ProbeRunner for PanickingRunner {
        fn run(&self, target: ProbeTarget, _: watch::Receiver<bool>) -> BoxFuture<RunEnd> {
            Box::pin(async move {
                if target.kind == Input {
                    panic!("probe task died");
                }
                RunEnd::Concluded {
                    outcome: ProbeOutcome::Pass {
                        summary: "ok".into(),
                    },
                    reconciled: true,
                }
            })
        }
        fn reconcile(&self, _: ProbeKind) -> BoxFuture<bool> {
            Box::pin(async { true })
        }
    }

    #[tokio::test]
    async fn a_probe_task_that_panics_is_indeterminate_and_the_next_probe_still_runs() {
        let (handle, mut updates) = spawn(Arc::new(PanickingRunner));
        handle.registered(inputs(&[]));
        let mut records = Vec::new();
        while records.len() < 2 {
            if let ReportUpdate::Record {
                target, outcome, ..
            } = tokio::time::timeout(BOUND, updates.recv())
                .await
                .unwrap()
                .unwrap()
            {
                records.push((target, outcome));
            }
        }
        assert_eq!(records[0].0, ProbeTarget::host(Input));
        assert!(matches!(records[0].1, ProbeOutcome::Indeterminate { .. }));
        assert_eq!(records[1].0, ProbeTarget::host(Audio));
        assert!(matches!(records[1].1, ProbeOutcome::Pass { .. }));
    }
}
