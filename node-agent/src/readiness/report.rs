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
    }
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
    retained: Vec<Retained>,
    refresh_failed: bool,
}

impl ReadinessReport {
    /// A local refresh succeeded.
    pub fn refreshed(&mut self, checks: Vec<ReadinessCheck>) {
        self.checks = checks;
        self.refresh_failed = false;
    }

    /// A local refresh failed, panicked or timed out. Must never remove a failing check.
    pub fn refresh_failed(&mut self) {
        self.refresh_failed = true;
    }

    /// Record a retained check. An older `observed_at` than the held result for the
    /// same id is ignored; equal or newer replaces it in place.
    pub fn retain(&mut self, check: ReadinessCheck, observed_at: SystemTime) {
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
    /// retained check that isn't itself FAIL.
    pub fn merged(&self) -> Vec<ReadinessCheck> {
        let mut out = Vec::with_capacity(self.checks.len() + self.retained.len() + 1);
        for local in &self.checks {
            match self.retained.iter().find(|r| r.check.id == local.id) {
                Some(r) if !(local.status == super::FAIL && r.check.status != super::FAIL) => {
                    out.push(r.check.clone());
                }
                _ => out.push(local.clone()),
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
