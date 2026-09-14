//! Image operations exposed through Quasar types and bounded progress snapshots.
use super::*;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ImageInfo {
    pub id: String,
    pub bytes: u64,
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct ImageProgress {
    pub percent: u8,
    pub bytes: u64,
}

pub struct ImageOperation<T> {
    operation: Operation<T>,
    progress: watch::Receiver<ImageProgress>,
}
impl<T> ImageOperation<T> {
    pub fn wait(self, progress: impl FnMut(ImageProgress)) -> Result<T, RuntimeError> {
        self.wait_with_cancel(progress, || false)
    }

    /// Stop observing when a session is cancelled; the mutation keeps its own
    /// deadline and durable intent. Never hold the progress read lock in callbacks.
    pub fn wait_with_cancel(
        mut self,
        mut progress: impl FnMut(ImageProgress),
        mut cancelled: impl FnMut() -> bool,
    ) -> Result<T, RuntimeError> {
        loop {
            if cancelled() {
                return Err(ErrorKind::Cancelled.into());
            }
            let sample = *self.progress.borrow_and_update();
            progress(sample);
            match self
                .operation
                .result
                .recv_timeout(Duration::from_millis(100))
            {
                Ok(result) => {
                    let sample = *self.progress.borrow_and_update();
                    progress(sample);
                    if cancelled() {
                        return Err(ErrorKind::Cancelled.into());
                    }
                    return result;
                }
                Err(mpsc::RecvTimeoutError::Timeout) => {}
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    return Err(ErrorKind::UnknownOutcome.into())
                }
            }
        }
    }
}

impl RuntimeClient {
    pub fn build_image(
        &self,
        request: BuildRequest,
        budget: Duration,
    ) -> ImageOperation<ImageInfo> {
        let config = self.config.clone();
        let (send, progress) = watch::channel(ImageProgress::default());
        ImageOperation {
            operation: self.submit_owned(
                async move { docker::build_image(&config, request, send, budget).await },
                budget,
                true,
            ),
            progress,
        }
    }

    /// Remove a managed reference without forcing deletion of an in-use image.
    pub fn remove_image(&self, image: impl Into<String>, deadline: Duration) -> Operation<()> {
        let image = image.into();
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::remove_image(&config, &image).await },
            deadline,
            true,
        )
    }

    pub fn ensure_image(
        &self,
        image: impl Into<String>,
        deadline: Duration,
    ) -> ImageOperation<ImageInfo> {
        let image = image.into();
        let config = self.config.clone();
        let (send, progress) = watch::channel(ImageProgress::default());
        ImageOperation {
            operation: self.submit_owned(
                async move { docker::ensure_image(&config, &image, send).await },
                deadline,
                true,
            ),
            progress,
        }
    }
}

/// A synced intent survives process/transport failure. Only conclusive observed
/// state or a terminal daemon response clears it; absence is not pull completion.
#[derive(serde::Serialize, serde::Deserialize)]
pub(super) struct Intent {
    pub image: String,
    pub socket: PathBuf,
    pub remove_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub build_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub build_fingerprint: Option<String>,
}

pub(super) struct Journal {
    path: PathBuf,
    _lease: std::fs::File,
}
impl Journal {
    pub async fn acquire(config: &RuntimeConfig, image: &str) -> Result<Self, RuntimeError> {
        use sha2::{Digest, Sha256};
        use std::os::{
            fd::AsRawFd,
            unix::fs::{DirBuilderExt, OpenOptionsExt},
        };
        let root = config
            .image_state_path
            .as_ref()
            .ok_or(ErrorKind::InvalidConfiguration)?;
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(root)
            .map_err(|_| ErrorKind::Unavailable)?;
        let key = Sha256::digest(image.as_bytes())
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let lease = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(root.join(format!("{key}.lock")))
            .map_err(|_| ErrorKind::Unavailable)?;
        loop {
            // SAFETY: lease owns a live file descriptor until this operation ends.
            if unsafe { libc::flock(lease.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } == 0 {
                break;
            }
            if std::io::Error::last_os_error().kind() != std::io::ErrorKind::WouldBlock {
                return Err(ErrorKind::Unavailable.into());
            }
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
        Ok(Self {
            path: root.join(key),
            _lease: lease,
        })
    }
    pub fn pending(&self) -> Result<Option<Intent>, RuntimeError> {
        use std::{io::Read, os::unix::fs::OpenOptionsExt};
        match std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&self.path)
        {
            Ok(file) => {
                let mut bytes = Vec::new();
                file.take(8193)
                    .read_to_end(&mut bytes)
                    .map_err(|_| ErrorKind::Unavailable)?;
                if bytes.len() > 8192 {
                    return Err(ErrorKind::Protocol.into());
                }
                serde_json::from_slice(&bytes)
                    .map(Some)
                    .map_err(|_| ErrorKind::Protocol.into())
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(_) => Err(ErrorKind::Unavailable.into()),
        }
    }
    pub fn begin(&self, intent: Intent) -> Result<(), RuntimeError> {
        use std::{io::Write, os::unix::fs::OpenOptionsExt};
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&self.path)
            .map_err(|e| {
                if e.kind() == std::io::ErrorKind::AlreadyExists {
                    ErrorKind::UnknownOutcome
                } else {
                    ErrorKind::Unavailable
                }
            })?;
        file.write_all(&serde_json::to_vec(&intent).map_err(|_| ErrorKind::Protocol)?)
            .and_then(|_| file.sync_all())
            .map_err(|_| ErrorKind::Unavailable)?;
        self.sync_parent()
    }
    pub fn clear(&self) -> Result<(), RuntimeError> {
        std::fs::remove_file(&self.path).map_err(|_| ErrorKind::Unavailable)?;
        self.sync_parent()
    }
    fn sync_parent(&self) -> Result<(), RuntimeError> {
        std::fs::File::open(self.path.parent().unwrap())
            .and_then(|f| f.sync_all())
            .map_err(|_| ErrorKind::Unavailable.into())
    }
}
