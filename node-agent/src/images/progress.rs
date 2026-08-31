//! Pull-progress throttling (agent-api.md `image_state`: at most every 2s or >=5%
//! delta, whichever coarser, never per docker layer event) plus best-effort parsing
//! of `docker pull`'s per-layer text into an aggregate percent.
//!
//! Scraped from CLI text because the agent's docker plumbing is CLI-based by design;
//! machine-readable progress would mean taking on the daemon HTTP API.

use std::collections::BTreeMap;
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

/// One `docker pull` layer's current/total bytes, parsed from a progress line.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LayerProgress {
    pub current: u64,
    pub total: u64,
}

/// Parse one non-TTY `docker pull` layer line, e.g.
/// `5eb5b503b376: Downloading [====>   ]  1.211MB/72.9MB`. Any other line is `None`.
pub fn parse_pull_line(line: &str) -> Option<(String, LayerProgress)> {
    let (id, rest) = line.split_once(':')?;
    let id = id.trim();
    if id.is_empty() || id.contains(' ') {
        return None; // not a "<layer-id>: ..." line
    }
    let rest = rest.trim();
    if !(rest.starts_with("Downloading") || rest.starts_with("Extracting")) {
        return None;
    }
    // The "current/total" figure trails the progress bar, e.g. "1.211MB/72.9MB".
    let sizes = rest.rsplit(']').next().unwrap_or(rest).trim();
    let (cur, total) = sizes.split_once('/')?;
    let current = parse_size(cur.trim())?;
    let total = parse_size(total.trim())?;
    Some((id.to_string(), LayerProgress { current, total }))
}

/// Parse a docker-formatted size (`1.211MB`, `512B`, `3.4kB`, `2.1GB`) into bytes.
fn parse_size(s: &str) -> Option<u64> {
    let s = s.trim();
    let idx = s.find(|c: char| c.is_alphabetic())?;
    let (num, unit) = s.split_at(idx);
    let num: f64 = num.parse().ok()?;
    let mul: f64 = match unit {
        "B" => 1.0,
        "kB" | "KB" => 1_000.0,
        "MB" => 1_000_000.0,
        "GB" => 1_000_000_000.0,
        _ => return None,
    };
    Some((num * mul) as u64)
}

/// Percent from a classic-builder (`DOCKER_BUILDKIT=0`) `Step 3/14 : ...` line.
/// Best-effort per agent-api.md `image_build`: omit `progress_pct` when absent.
pub fn parse_build_step(line: &str) -> Option<u8> {
    let rest = line.trim().strip_prefix("Step ")?;
    let frac = rest.split_whitespace().next()?;
    let (n, m) = frac.split_once('/')?;
    let n: u64 = n.parse().ok()?;
    let m: u64 = m.parse().ok()?;
    if m == 0 {
        return None;
    }
    Some(((n.min(m) as f64 / m as f64) * 100.0) as u8)
}

/// Aggregates per-layer byte progress into one percent + bytes figure. Layers appear
/// over time, so the percent is approximate early and converges as more are observed.
#[derive(Debug, Default)]
pub struct PullProgressTracker {
    layers: BTreeMap<String, LayerProgress>,
}

impl PullProgressTracker {
    pub fn new() -> Self {
        PullProgressTracker::default()
    }

    pub fn observe(&mut self, id: String, p: LayerProgress) {
        self.layers.insert(id, p);
    }

    /// `(progress_pct 0-100, bytes downloaded so far)`.
    pub fn snapshot(&self) -> (u8, u64) {
        let total: u64 = self.layers.values().map(|p| p.total).sum();
        let current: u64 = self.layers.values().map(|p| p.current).sum();
        if total == 0 {
            return (0, current);
        }
        let pct = ((current as f64 / total as f64) * 100.0).clamp(0.0, 100.0) as u8;
        (pct, current)
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

    #[test]
    fn parses_downloading_line() {
        let (id, p) =
            parse_pull_line("5eb5b503b376: Downloading [====>   ]  1.211MB/72.9MB").unwrap();
        assert_eq!(id, "5eb5b503b376");
        assert_eq!(p.current, 1_211_000);
        assert_eq!(p.total, 72_900_000);
    }

    #[test]
    fn parses_extracting_line() {
        let (id, p) = parse_pull_line(
            "5eb5b503b376: Extracting [==================================================>]  72.9MB/72.9MB",
        )
        .unwrap();
        assert_eq!(id, "5eb5b503b376");
        assert_eq!(p.current, 72_900_000);
        assert_eq!(p.total, 72_900_000);
    }

    #[test]
    fn parses_build_step_fraction() {
        assert_eq!(parse_build_step("Step 1/10 : FROM alpine:3"), Some(10));
        assert_eq!(parse_build_step("Step 5/10 : RUN echo hi"), Some(50));
        assert_eq!(parse_build_step("Step 10/10 : CMD [\"sh\"]"), Some(100));
        // Leading indentation is tolerated.
        assert_eq!(parse_build_step("  Step 2/4 : COPY . ."), Some(50));
    }

    #[test]
    fn ignores_non_step_build_lines() {
        assert!(parse_build_step("Successfully built abc123").is_none());
        assert!(parse_build_step("Successfully tagged quasar-local/x:1").is_none());
        assert!(parse_build_step(" ---> a1b2c3d4").is_none());
        assert!(parse_build_step("Step /10 : bad").is_none());
        assert!(parse_build_step("Step 3/0 : divzero").is_none());
        assert!(parse_build_step("Sending build context to Docker daemon").is_none());
    }

    #[test]
    fn ignores_non_progress_lines() {
        assert!(parse_pull_line("5eb5b503b376: Pull complete").is_none());
        assert!(parse_pull_line("Digest: sha256:abcd").is_none());
        assert!(parse_pull_line("Status: Downloaded newer image for x:latest").is_none());
        assert!(parse_pull_line("Using default tag: latest").is_none());
    }

    #[test]
    fn tracker_aggregates_across_layers() {
        let mut tr = PullProgressTracker::new();
        tr.observe(
            "a".into(),
            LayerProgress {
                current: 50,
                total: 100,
            },
        );
        tr.observe(
            "b".into(),
            LayerProgress {
                current: 25,
                total: 100,
            },
        );
        let (pct, bytes) = tr.snapshot();
        assert_eq!(pct, 37);
        assert_eq!(bytes, 75);
    }

    #[test]
    fn tracker_updates_a_layer_in_place() {
        let mut tr = PullProgressTracker::new();
        tr.observe(
            "a".into(),
            LayerProgress {
                current: 10,
                total: 100,
            },
        );
        tr.observe(
            "a".into(),
            LayerProgress {
                current: 60,
                total: 100,
            },
        );
        let (pct, bytes) = tr.snapshot();
        assert_eq!(pct, 60);
        assert_eq!(bytes, 60);
    }
}
