//! The listener's self-signed certificates and their rotation
//! (`protocol/transport-webtransport.md` §Certificates).

use std::time::{Duration, SystemTime};

use anyhow::Result;
use base64::Engine;
use serde::Serialize;
use sha2::{Digest, Sha256};
use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;

const HOUR: Duration = Duration::from_secs(3600);
/// `serverCertificateHashes` refuses a certificate valid longer than 14 days.
const VALIDITY: Duration = Duration::from_secs(13 * 24 * 3600);
const MAKE_NEXT: Duration = Duration::from_secs(72 * 3600);
const PRESENT_NEXT: Duration = Duration::from_secs(24 * 3600);

pub(crate) struct Cert {
    pub(crate) der: Vec<u8>,
    pub(crate) key_der: Vec<u8>,
    pub(crate) not_before: SystemTime,
    pub(crate) not_after: SystemTime,
    /// Base64 SHA-256 of `der`, the `serverCertificateHashes` value.
    pub(crate) sha256: String,
}

/// One entry of `capacity.webtransport.certificates` (`agent-api.md`).
#[derive(Serialize, Debug, Clone, PartialEq)]
pub struct CertReport {
    pub sha256: String,
    pub not_before: String,
    pub not_after: String,
}

impl Cert {
    fn make(now: SystemTime) -> Result<Self> {
        // X.509 keeps whole seconds; truncate so the report matches the certificate.
        let now = SystemTime::UNIX_EPOCH
            + Duration::from_secs(now.duration_since(SystemTime::UNIX_EPOCH)?.as_secs());
        let not_before = now - HOUR;
        let not_after = not_before + VALIDITY;
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256)?;
        let mut params = rcgen::CertificateParams::new(vec!["quasar-node-agent".to_string()])?;
        params.not_before = OffsetDateTime::from(not_before);
        params.not_after = OffsetDateTime::from(not_after);
        let der = params.self_signed(&key)?.der().to_vec();
        let sha256 = base64::engine::general_purpose::STANDARD.encode(Sha256::digest(&der));
        Ok(Self {
            der,
            key_der: key.serialize_der(),
            not_before,
            not_after,
            sha256,
        })
    }

    fn left(&self, now: SystemTime) -> Duration {
        self.not_after.duration_since(now).unwrap_or_default()
    }

    fn report(&self) -> CertReport {
        let rfc = |t: SystemTime| OffsetDateTime::from(t).format(&Rfc3339).unwrap_or_default();
        CertReport {
            sha256: self.sha256.clone(),
            not_before: rfc(self.not_before),
            not_after: rfc(self.not_after),
        }
    }
}

/// Every certificate the listener may present, oldest first, at most two.
pub(crate) struct CertSet {
    certs: Vec<Cert>,
}

impl CertSet {
    pub(crate) fn new(now: SystemTime) -> Result<Self> {
        let mut set = Self { certs: Vec::new() };
        set.tick(now)?;
        Ok(set)
    }

    /// Ages the set to `now`: drops what expired, makes the next certificate once the
    /// newest has 72 h or less. True when the reported list changed.
    pub(crate) fn tick(&mut self, now: SystemTime) -> Result<bool> {
        let before = self.certs.len();
        self.certs.retain(|c| c.not_after > now);
        let mut changed = self.certs.len() != before;
        if self.certs.last().is_none_or(|c| c.left(now) <= MAKE_NEXT) {
            self.certs.push(Cert::make(now)?);
            changed = true;
        }
        Ok(changed)
    }

    /// What a new connection gets: the next certificate once the old one has 24 h or less.
    pub(crate) fn presented(&self, now: SystemTime) -> &Cert {
        match &self.certs[..] {
            [old, next] if old.left(now) <= PRESENT_NEXT => next,
            certs => &certs[0],
        }
    }

    /// `capacity.webtransport.certificates`: the presented one first.
    pub(crate) fn report(&self, now: SystemTime) -> Vec<CertReport> {
        let first = self.presented(now).sha256.clone();
        let mut out: Vec<CertReport> = self.certs.iter().map(Cert::report).collect();
        out.sort_by_key(|c| c.sha256 != first);
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const DAY: Duration = Duration::from_secs(24 * 3600);

    #[test]
    fn a_certificate_is_p256_and_valid_at_most_fourteen_days() {
        let now = SystemTime::now();
        let set = CertSet::new(now).unwrap();
        let cert = set.presented(now);
        assert!(cert.not_after.duration_since(cert.not_before).unwrap() <= 14 * DAY);
        assert!(cert.not_before <= now && now < cert.not_after);
        // id-ecPublicKey with prime256v1, the only key `serverCertificateHashes` takes.
        let p256 = [0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07];
        assert!(cert.der.windows(p256.len()).any(|w| w == p256));
        let hash = base64::engine::general_purpose::STANDARD
            .decode(&cert.sha256)
            .unwrap();
        assert_eq!(hash, Sha256::digest(&cert.der).to_vec());
    }

    #[test]
    fn rotation_reports_the_next_at_72h_presents_it_at_24h_and_forgets_the_expired() {
        let t0 = SystemTime::now();
        let mut set = CertSet::new(t0).unwrap();
        let first = set.presented(t0).sha256.clone();
        let expires = set.certs[0].not_after;

        let quiet = expires - MAKE_NEXT - HOUR;
        assert!(!set.tick(quiet).unwrap());
        assert_eq!(set.report(quiet).len(), 1);

        let overlap = expires - MAKE_NEXT;
        assert!(
            set.tick(overlap).unwrap(),
            "the next certificate is reported"
        );
        let report = set.report(overlap);
        assert_eq!(report.len(), 2);
        assert_eq!(report[0].sha256, first, "the old one is still presented");
        let next = report[1].sha256.clone();

        let switch = expires - PRESENT_NEXT;
        assert!(!set.tick(switch).unwrap());
        assert_eq!(set.presented(switch).sha256, next);
        assert_eq!(set.report(switch)[0].sha256, next, "presented first");

        assert!(set.tick(expires).unwrap(), "the expired one is dropped");
        let report = set.report(expires);
        assert_eq!(report.len(), 1);
        assert_eq!(report[0].sha256, next);
    }
}
