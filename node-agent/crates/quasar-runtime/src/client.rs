//! The bounded engine client: one executor, admission, deadlines and cancellation.

use crate::{
    docker, ContainerInspection, DaemonImage, EngineFacts, EngineInfo, EngineStorage, ErrorKind,
    ImageMetadata, RuntimeConfig, RuntimeError,
};
use std::{
    sync::{mpsc, Arc},
    time::Duration,
};
use tokio::sync::{watch, Semaphore};

/// How long one read-only engine inspection may take before the agent calls the engine
/// unreachable (#274). It is deliberately far below the client deadline: the agent's
/// readiness refresh runs every `READINESS_REFRESH_INTERVAL` and the control plane
/// abstains from a readiness verdict once a report is older than
/// `QUASAR_READINESS_STALE_SECS` (default 60 s), so a hung daemon — one whose socket
/// accepts the connection and then never answers — has to be visible in a report inside
/// that window, not merely "eventually".
pub const ENGINE_INSPECTION_BUDGET: Duration = Duration::from_secs(5);

// Own the executor independently of any control-plane connection. Background
// shutdown is safe even when the last caller lives on another Tokio executor.
struct Executor {
    runtime: Option<tokio::runtime::Runtime>,
    slots: Arc<Semaphore>,
}
impl Drop for Executor {
    fn drop(&mut self) {
        if let Some(runtime) = self.runtime.take() {
            runtime.shutdown_background();
        }
    }
}

/// A bounded operation. Dropping or cancelling reads stops them; image mutations
/// continue under their own deadline and retain uncertainty when interrupted.
/// `wait` bridges existing blocking callers; it must not be used on a media path.
pub struct Operation<T> {
    result: mpsc::Receiver<Result<T, RuntimeError>>,
    cancel: watch::Sender<bool>,
    _executor: Arc<Executor>,
}
impl<T> Operation<T> {
    pub fn cancel(&self) {
        let _ = self.cancel.send(true);
    }
    pub fn wait(self) -> Result<T, RuntimeError> {
        self.result
            .recv()
            .unwrap_or_else(|_| Err(ErrorKind::Unavailable.into()))
    }

    /// Stop waiting when a host probe's deadline passes or a launch pre-empts it, then
    /// keep waiting for the executor's own answer: dropping an observation must never
    /// stop, remove or roll back anything, and the race the cancellation lost still
    /// carries the real result.
    pub fn wait_with_cancel(self, mut cancelled: impl FnMut() -> bool) -> Result<T, RuntimeError> {
        let mut sent = false;
        loop {
            if !sent && cancelled() {
                self.cancel();
                sent = true;
            }
            match self.result.recv_timeout(Duration::from_millis(50)) {
                Ok(result) => return result,
                Err(mpsc::RecvTimeoutError::Timeout) => {}
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    return Err(ErrorKind::Unavailable.into())
                }
            }
        }
    }

    /// Wait up to `timeout` for the result without consuming the operation, for a
    /// caller that interleaves its own observation (progress, cancellation) with the
    /// wait and gives a lost executor its own meaning. Neither outcome cancels.
    pub fn recv_timeout(
        &self,
        timeout: Duration,
    ) -> Result<Result<T, RuntimeError>, mpsc::RecvTimeoutError> {
        self.result.recv_timeout(timeout)
    }
}
impl<T> Drop for Operation<T> {
    fn drop(&mut self) {
        self.cancel();
    }
}

#[derive(Clone)]
pub struct RuntimeClient {
    config: RuntimeConfig,
    executor: Arc<Executor>,
}
impl RuntimeClient {
    pub fn new(config: RuntimeConfig) -> Result<Self, RuntimeError> {
        if !config.socket.is_absolute()
            || config.socket.to_str().is_none()
            || config.deadline.is_zero()
            || config.max_in_flight == 0
            || config.max_in_flight > Semaphore::MAX_PERMITS
            || std::time::Instant::now()
                .checked_add(config.deadline)
                .is_none()
        {
            return Err(ErrorKind::InvalidConfiguration.into());
        }
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(1)
            .thread_name("quasar-runtime")
            .enable_all()
            .build()
            .map_err(|_| RuntimeError::from(ErrorKind::Unavailable))?;
        let slots = Arc::new(Semaphore::new(config.max_in_flight));
        Ok(Self {
            config,
            executor: Arc::new(Executor {
                runtime: Some(runtime),
                slots,
            }),
        })
    }

    /// The configuration this client was built with, for adapters layered on it.
    pub fn config(&self) -> &RuntimeConfig {
        &self.config
    }

    /// Run `work` under this client's admission and default deadline; cancellable.
    /// The seam adapters layered on this crate use for their own read operations.
    pub fn submit<T: Send + 'static>(
        &self,
        work: impl std::future::Future<Output = Result<T, RuntimeError>> + Send + 'static,
    ) -> Operation<T> {
        self.submit_owned(work, self.config.deadline, false)
    }

    /// Run `work` under this client's admission and an explicit `budget`. A
    /// `detached` operation is a mutation: it ignores cancellation, keeps the executor
    /// alive until it finishes, and a spent budget is [`ErrorKind::UnknownOutcome`]
    /// rather than [`ErrorKind::Timeout`].
    pub fn submit_owned<T: Send + 'static>(
        &self,
        work: impl std::future::Future<Output = Result<T, RuntimeError>> + Send + 'static,
        budget: Duration,
        detached: bool,
    ) -> Operation<T> {
        let (send, result) = mpsc::sync_channel(1);
        let (cancel, mut cancelled) = watch::channel(false);
        let operation = Operation {
            result,
            cancel,
            _executor: self.executor.clone(),
        };
        if budget.is_zero() || std::time::Instant::now().checked_add(budget).is_none() {
            let _ = send.send(Err(ErrorKind::InvalidConfiguration.into()));
            return operation;
        }
        match self.executor.slots.clone().try_acquire_owned() {
            Err(_) => {
                let _ = send.send(Err(ErrorKind::Busy.into()));
            }
            Ok(permit) => {
                let deadline = tokio::time::Instant::now() + budget;
                let owner = detached.then(|| self.executor.clone());
                self.executor
                    .runtime
                    .as_ref()
                    .expect("live executor")
                    .spawn(async move {
                        let _permit = permit;
                        let _owner = owner;
                        let result = tokio::select! {
                            biased;
                            _ = cancelled.changed(), if !detached => Err(ErrorKind::Cancelled.into()),
                            result = tokio::time::timeout_at(deadline, work) =>
                                result.unwrap_or_else(|_| Err(if detached { ErrorKind::UnknownOutcome } else { ErrorKind::Timeout }.into())),
                        };
                        let _ = send.send(result);
                    });
            }
        }
        operation
    }

    pub fn discover(&self) -> Operation<EngineInfo> {
        let config = self.config.clone();
        self.submit(async move { docker::discover(&config).await.map(|(_, info)| info) })
    }

    /// Read one container's daemon-authoritative configuration. `Ok(None)` is
    /// only a conclusively missing container; inaccessible engines are errors.
    pub fn inspect_container(
        &self,
        id: impl Into<String>,
    ) -> Operation<Option<ContainerInspection>> {
        let config = self.config.clone();
        let id = id.into();
        self.submit(async move { docker::inspect_container(&config, &id).await })
    }

    /// [`Self::inspect_container`] under a caller-chosen budget. The readiness refresh uses
    /// it with [`ENGINE_INSPECTION_BUDGET`]: a refresh that straddles a daemon freeze has
    /// already proved the engine answers, so it does not skip its remaining collectors —
    /// and one full client deadline inside a refresh is a whole staleness window (#274).
    pub fn inspect_container_within(
        &self,
        id: impl Into<String>,
        budget: Duration,
    ) -> Operation<Option<ContainerInspection>> {
        let config = self.config.clone();
        let id = id.into();
        self.submit_owned(
            async move { docker::inspect_container(&config, &id).await },
            std::cmp::min(self.config.deadline, budget),
            false,
        )
    }

    /// Snapshot every live container, including containers Quasar does not own.
    /// Every listed ID is re-inspected so an incomplete liveness fact fails closed.
    pub fn live_containers(&self) -> Operation<Vec<ContainerInspection>> {
        let config = self.config.clone();
        self.submit(async move { docker::live_containers(&config).await })
    }

    /// Filesystem location in the container engine daemon's host namespace.
    pub fn engine_storage(&self) -> Operation<EngineStorage> {
        let config = self.config.clone();
        self.submit(async move { docker::engine_storage(&config).await })
    }

    /// [`Self::engine_storage`] under a caller-chosen budget; see
    /// [`Self::inspect_container_within`] for why the readiness path needs one.
    pub fn engine_storage_within(&self, budget: Duration) -> Operation<EngineStorage> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::engine_storage(&config).await },
            std::cmp::min(self.config.deadline, budget),
            false,
        )
    }

    /// One bounded, read-only inspection of the engine: discovery plus `/info`, folded
    /// into [`EngineFacts`]. Budgeted at [`ENGINE_INSPECTION_BUDGET`] so a readiness
    /// refresh on a wedged daemon reports it inside the control plane's staleness window
    /// instead of spending the whole refresh window here (#274).
    pub fn inspect_engine(&self) -> Operation<EngineFacts> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::inspect_engine(&config).await },
            std::cmp::min(self.config.deadline, ENGINE_INSPECTION_BUDGET),
            false,
        )
    }

    /// This client's default per-operation deadline, for callers choosing between it and
    /// an explicit budget.
    pub fn deadline(&self) -> Duration {
        self.config.deadline
    }

    /// The endpoint this client speaks to, for readiness wording.
    pub fn endpoint(&self) -> String {
        format!("unix://{}", self.config.socket.display())
    }

    /// Image identity and its baked environment. `Ok(None)` is a missing image.
    pub fn inspect_image_metadata(
        &self,
        image: impl Into<String>,
    ) -> Operation<Option<ImageMetadata>> {
        let config = self.config.clone();
        let image = image.into();
        self.submit(async move { docker::inspect_image_metadata(&config, &image).await })
    }

    pub fn daemon_images(&self) -> Operation<Vec<DaemonImage>> {
        let config = self.config.clone();
        self.submit(async move { docker::daemon_images(&config).await })
    }

    pub fn all_container_image_ids(&self) -> Operation<Vec<String>> {
        let config = self.config.clone();
        self.submit(async move { docker::all_container_image_ids(&config).await })
    }
}
