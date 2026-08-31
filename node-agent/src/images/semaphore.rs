//! Blocking counting semaphore capping concurrent pulls at 2, so an ensure storm
//! never starves live-session IO (agent-api.md image-management P2).
//!
//! Std-only, not `tokio::sync::Semaphore`: pulls run on plain `std::thread`s, so a
//! Tokio handle would have to be threaded into every pull thread for no benefit.

use std::sync::{Arc, Condvar, Mutex};

pub struct CountingSemaphore {
    state: Mutex<usize>,
    cvar: Condvar,
}

impl CountingSemaphore {
    pub fn new(permits: usize) -> Arc<Self> {
        Arc::new(CountingSemaphore {
            state: Mutex::new(permits),
            cvar: Condvar::new(),
        })
    }

    /// Block until a permit is available, then hold it until the returned
    /// guard drops.
    pub fn acquire(self: &Arc<Self>) -> SemaphoreGuard {
        let mut count = self.state.lock().unwrap();
        while *count == 0 {
            count = self.cvar.wait(count).unwrap();
        }
        *count -= 1;
        drop(count);
        SemaphoreGuard { sem: self.clone() }
    }
}

pub struct SemaphoreGuard {
    sem: Arc<CountingSemaphore>,
}

impl Drop for SemaphoreGuard {
    fn drop(&mut self) {
        let mut count = self.sem.state.lock().unwrap();
        *count += 1;
        self.sem.cvar.notify_one();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::Duration;

    #[test]
    fn caps_concurrency_at_the_configured_limit() {
        let sem = CountingSemaphore::new(2);
        let concurrent = Arc::new(AtomicUsize::new(0));
        let max_seen = Arc::new(AtomicUsize::new(0));
        let mut handles = Vec::new();
        for _ in 0..6 {
            let sem = sem.clone();
            let concurrent = concurrent.clone();
            let max_seen = max_seen.clone();
            handles.push(std::thread::spawn(move || {
                let _permit = sem.acquire();
                let now = concurrent.fetch_add(1, Ordering::SeqCst) + 1;
                max_seen.fetch_max(now, Ordering::SeqCst);
                std::thread::sleep(Duration::from_millis(30));
                concurrent.fetch_sub(1, Ordering::SeqCst);
            }));
        }
        for h in handles {
            h.join().unwrap();
        }
        assert!(max_seen.load(Ordering::SeqCst) <= 2);
        assert!(max_seen.load(Ordering::SeqCst) >= 1);
    }

    #[test]
    fn permits_are_released_on_drop() {
        let sem = CountingSemaphore::new(1);
        {
            let _permit = sem.acquire();
            assert_eq!(*sem.state.lock().unwrap(), 0);
        }
        assert_eq!(*sem.state.lock().unwrap(), 1);
    }
}
