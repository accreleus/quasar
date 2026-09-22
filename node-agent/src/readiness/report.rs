//! The readiness report the agent sends: the latest local refresh merged with the
//! checks that outlive it.

use std::time::SystemTime;

use crate::messages::ReadinessCheck;

pub const REFRESH_WARNING_ID: &str = "readiness_probe";

fn refresh_warning() -> ReadinessCheck {
    ReadinessCheck {
        id: REFRESH_WARNING_ID.into(),
        status: super::WARN.into(),
        summary: "Host readiness could not be refreshed; previous results are no longer current"
            .into(),
        remediation: "The agent will retry automatically. Check its logs if this persists.".into(),
        // No observation of its own: it reports the absence of a refresh, not a finding,
        // and it is a proxy — it must never carry `blocks`.
        observed_at: None,
        source: None,
        blocks: None,
    }
}

/// RFC3339 UTC, second precision, `Z` suffix. No time crate in this workspace; a
/// hand-rolled conversion is cheaper than a new dependency for one field.
/// Days-since-epoch <-> (year, month, day) is Howard Hinnant's `civil_from_days`.
fn format_observed_at(t: SystemTime) -> String {
    let secs = t
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    let days = (secs / 86_400) as i64;
    let rem = secs % 86_400;
    let (h, m, s) = (rem / 3600, (rem % 3600) / 60, rem % 60);
    let (y, mo, d) = civil_from_days(days);
    format!("{y:04}-{mo:02}-{d:02}T{h:02}:{m:02}:{s:02}Z")
}

fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = (z - era * 146_097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

#[derive(Debug)]
struct Retained {
    check: ReadinessCheck,
    observed_at: SystemTime,
}

/// Locally recomputed checks are replaced by every successful refresh. Retained checks
/// (host-probe results, the agent's safety states) change only through [`Self::retain`].
#[derive(Debug, Default)]
pub struct ReadinessReport {
    checks: Vec<ReadinessCheck>,
    /// When the last successful [`Self::refreshed`] ran — what `merged()` stamps every
    /// local check with. `None` before the first refresh.
    checks_observed_at: Option<SystemTime>,
    retained: Vec<Retained>,
    refresh_failed: bool,
}

impl ReadinessReport {
    /// A local refresh succeeded, observed at `at`.
    pub fn refreshed(&mut self, checks: Vec<ReadinessCheck>, at: SystemTime) {
        self.checks = checks;
        self.checks_observed_at = Some(at);
        self.refresh_failed = false;
    }

    /// A local refresh failed, panicked or timed out. Must never remove a failing check.
    pub fn refresh_failed(&mut self) {
        self.refresh_failed = true;
    }

    /// Record a retained check, stamping its `observed_at`. An older `observed_at` than
    /// the held result for the same id is ignored; equal or newer replaces it in place.
    pub fn retain(&mut self, mut check: ReadinessCheck, observed_at: SystemTime) {
        check.observed_at = Some(format_observed_at(observed_at));
        match self.retained.iter_mut().find(|r| r.check.id == check.id) {
            Some(existing) if observed_at >= existing.observed_at => {
                existing.check = check;
                existing.observed_at = observed_at;
            }
            Some(_) => {}
            None => self.retained.push(Retained { check, observed_at }),
        }
    }

    /// The held retained check for `id`, if any. Local (refreshed) checks are not
    /// visible here — only what [`Self::retain`] put in.
    pub fn retained(&self, id: &str) -> Option<&ReadinessCheck> {
        self.retained
            .iter()
            .find(|r| r.check.id == id)
            .map(|r| &r.check)
    }

    /// Drops the retained check for `id`, if any. Not a tombstone: a later
    /// [`Self::retain`] for the same id is accepted regardless of `observed_at`.
    pub fn forget(&mut self, id: &str) {
        self.retained.retain(|r| r.check.id != id);
    }

    /// What every capacity message carries: local checks, then retained checks not
    /// shadowed by a local id, then the refresh warning if the last refresh failed.
    /// A shared id shows the retained result, except a local FAIL is never hidden by a
    /// retained check that isn't itself FAIL. A local check that makes it through is
    /// stamped `observed_at` the time of the refresh that produced it — a failed refresh
    /// keeps the earlier local checks and they keep that earlier stamp.
    pub fn merged(&self) -> Vec<ReadinessCheck> {
        let mut out = Vec::with_capacity(self.checks.len() + self.retained.len() + 1);
        let local_observed_at = self.checks_observed_at.map(format_observed_at);
        for local in &self.checks {
            match self.retained.iter().find(|r| r.check.id == local.id) {
                Some(r) if !(local.status == super::FAIL && r.check.status != super::FAIL) => {
                    out.push(r.check.clone());
                }
                _ => {
                    let mut c = local.clone();
                    c.observed_at = local_observed_at.clone();
                    out.push(c);
                }
            }
        }
        for retained in &self.retained {
            if !self.checks.iter().any(|c| c.id == retained.check.id) {
                out.push(retained.check.clone());
            }
        }
        if self.refresh_failed {
            out.push(refresh_warning());
        }
        out
    }
}

#[cfg(test)]
mod format_tests {
    use super::*;
    use std::time::Duration;

    fn at(secs: u64) -> SystemTime {
        SystemTime::UNIX_EPOCH + Duration::from_secs(secs)
    }

    #[test]
    fn format_observed_at_matches_known_instants() {
        assert_eq!(format_observed_at(at(0)), "1970-01-01T00:00:00Z");
        assert_eq!(format_observed_at(at(1_000)), "1970-01-01T00:16:40Z");
        assert_eq!(format_observed_at(at(3_600)), "1970-01-01T01:00:00Z");
        assert_eq!(format_observed_at(at(86_400)), "1970-01-02T00:00:00Z");
        // A leap day and a date well outside 1970, as a sanity check on the century terms.
        assert_eq!(
            format_observed_at(at(1_709_164_800)),
            "2024-02-29T00:00:00Z"
        );
        assert_eq!(
            format_observed_at(at(1_789_812_000)),
            "2026-09-19T10:00:00Z"
        );
    }
}
