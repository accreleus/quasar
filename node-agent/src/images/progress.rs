//! Image-state progress throttling.
use std::time::{Duration, Instant};

const THROTTLE_INTERVAL: Duration = Duration::from_secs(2);
const THROTTLE_DELTA_PCT: u8 = 5;

/// Decides whether a progress sample is worth emitting as `image_state`.
///
/// "Whichever is coarser" means a later sample emits only when BOTH thresholds are
/// crossed (>=2s elapsed AND >=5 points moved); either alone is the finer bound.
/// The first sample always emits, so `pulling` is reported promptly at pull start.
#[derive(Debug, Default)]
pub struct ProgressThrottle {
    last: Option<(Instant, u8)>,
}

impl ProgressThrottle {
    pub fn new() -> Self {
        ProgressThrottle::default()
    }

    /// `now` is threaded in, not read internally, so tests drive time without sleeping.
    pub fn should_emit(&mut self, now: Instant, pct: u8) -> bool {
        let emit = match self.last {
            None => true,
            Some((last_at, last_pct)) => {
                now.duration_since(last_at) >= THROTTLE_INTERVAL
                    && pct.abs_diff(last_pct) >= THROTTLE_DELTA_PCT
            }
        };
        if emit {
            self.last = Some((now, pct));
        }
        emit
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn throttle_always_emits_first_sample() {
        let mut t = ProgressThrottle::new();
        assert!(t.should_emit(Instant::now(), 0));
    }

    #[test]
    fn throttle_suppresses_small_fast_deltas() {
        let mut t = ProgressThrottle::new();
        let t0 = Instant::now();
        assert!(t.should_emit(t0, 10));
        // 1s later, +3pct: neither threshold crossed.
        assert!(!t.should_emit(t0 + Duration::from_secs(1), 13));
    }

    #[test]
    fn throttle_suppresses_the_time_threshold_alone() {
        // A stalled pull must not re-emit the same percent every 2s.
        let mut t = ProgressThrottle::new();
        let t0 = Instant::now();
        assert!(t.should_emit(t0, 10));
        assert!(!t.should_emit(t0 + Duration::from_secs(2), 11));
        assert!(!t.should_emit(t0 + Duration::from_secs(60), 10));
    }

    #[test]
    fn throttle_suppresses_the_delta_threshold_alone() {
        // A fast pull crosses 5% many times a second and must not emit that often.
        let mut t = ProgressThrottle::new();
        let t0 = Instant::now();
        assert!(t.should_emit(t0, 10));
        assert!(!t.should_emit(t0 + Duration::from_millis(100), 15));
        assert!(!t.should_emit(t0 + Duration::from_millis(1999), 90));
    }

    #[test]
    fn throttle_emits_only_when_both_thresholds_are_crossed() {
        let mut t = ProgressThrottle::new();
        let t0 = Instant::now();
        assert!(t.should_emit(t0, 0));
        // +1pct, +500ms: below both -> suppressed.
        assert!(!t.should_emit(t0 + Duration::from_millis(500), 1));
        // +2.4s AND +6pct since the last EMITTED sample -> emits.
        assert!(t.should_emit(t0 + Duration::from_millis(2400), 6));
        // Both baselines moved to that emission.
        assert!(!t.should_emit(t0 + Duration::from_millis(4900), 10));
        assert!(t.should_emit(t0 + Duration::from_millis(5000), 11));
    }

    #[test]
    fn throttle_counts_a_backwards_delta_too() {
        // A percent moving down (docker re-estimating totals) is still a >=5-point move.
        let mut t = ProgressThrottle::new();
        let t0 = Instant::now();
        assert!(t.should_emit(t0, 40));
        assert!(t.should_emit(t0 + Duration::from_secs(3), 30));
    }
}
