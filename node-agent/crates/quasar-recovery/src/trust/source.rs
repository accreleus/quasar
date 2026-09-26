//! Fetching a release's manifest and signature: port of
//! `control-plane/internal/updater/signature_source.go`.
//!
//! The verifier fetches both assets itself, from a host-local base URL, by the request's
//! `release.version`; nothing the requester sends is trusted as evidence. [`probe`] is the
//! whole decision and does no I/O: it names each URL to fetch and classifies each outcome
//! into [`SignatureEvidence`]. `https::HttpsFetcher` drives it over the network; any
//! failure to complete a fetch is an `Err`, which is never read as unsigned.

use std::sync::LazyLock;
use std::time::Duration;

use regex::Regex;

use super::golang::text::{quote, trim_space};
use super::golang::url;
use super::signature::SignatureEvidence;

/// The org's own releases. `{version}` is the only substitution.
pub(crate) const DEFAULT_MANIFEST_BASE_URL: &str =
    "https://github.com/accreleus/quasar/releases/download/v{version}/";
/// The asset pair a release's signature is read from.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct ReleaseAssets {
    pub(crate) manifest: &'static str,
    pub(crate) signature: &'static str,
}

/// What an actor verifies (control-api.md amendment 14, "Release manifest format 2"):
/// RH06-era releases publish only the format-2 pair.
pub(crate) const FORMAT_2_ASSETS: ReleaseAssets = ReleaseAssets {
    manifest: "platform-release-manifest.v2.json",
    signature: "platform-release-manifest.v2.json.sig",
};

/// The Go updater's pair, which the shared trust vectors describe unless one names
/// `asset_format: 2`.
#[cfg(test)]
pub(crate) const FORMAT_1_ASSETS: ReleaseAssets = ReleaseAssets {
    manifest: "platform-release-manifest.json",
    signature: "platform-release-manifest.json.sig",
};

/// Larger than any real asset by three orders of magnitude.
pub(crate) const MAX_ASSET_BYTES: usize = 1 << 20;
/// Bounds both fetches together, under the agent's 30 s socket timeout.
pub(crate) const DEFAULT_ASSET_TIMEOUT: Duration = Duration::from_secs(15);

/// `QUASAR_UPDATER_MANIFEST_TIMEOUT_S` as the Go updater reads it: `envInt` (blank, not a
/// decimal integer, or not positive is the default), then `time.Duration(n) * time.Second`,
/// which wraps, then `Evidence`'s "not positive is the default".
pub fn parse_manifest_timeout(raw: &str) -> Duration {
    match raw.parse::<i64>() {
        Ok(n) if n > 0 => match n.wrapping_mul(1_000_000_000) {
            ns if ns > 0 => Duration::from_nanos(ns as u64),
            _ => DEFAULT_ASSET_TIMEOUT,
        },
        _ => DEFAULT_ASSET_TIMEOUT,
    }
}

/// Requests one asset fetch may make: the original and nine redirects.
const MAX_REDIRECTS: usize = 10;

/// The version is a wire value concatenated into a URL path, so it must be strict semver.
static VERSION_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$")
        .expect("version regex")
});

/// A validated `QUASAR_UPDATER_MANIFEST_BASE_URL`: https, a host, a `{version}`, and a
/// trailing `/`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ManifestBaseUrl(String);

impl ManifestBaseUrl {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

/// `QUASAR_UPDATER_MANIFEST_BASE_URL`; blank is the org's releases. HTTPS only: over
/// plaintext a network attacker could turn "signed" into "no signature published".
pub fn parse_manifest_base_url(raw: &str) -> Result<ManifestBaseUrl, String> {
    let mut raw = trim_space(raw).to_owned();
    if raw.is_empty() {
        raw = DEFAULT_MANIFEST_BASE_URL.to_owned();
    }
    if !raw.contains("{version}") {
        return Err(format!(
            "{} contains no {{version}} placeholder, so it cannot name a release's assets",
            quote(&raw)
        ));
    }
    if !raw.ends_with('/') {
        raw.push('/');
    }
    let probe = url::parse(&raw.replace("{version}", "0.0.0"))
        .map_err(|e| format!("{} is not a URL: {e}", quote(&raw)))?;
    if probe.scheme != "https" {
        return Err(format!(
            "{} is not https; the release manifest must be fetched over TLS",
            quote(&raw)
        ));
    }
    if probe.host.is_empty() {
        return Err(format!("{} names no host", quote(&raw)));
    }
    Ok(ManifestBaseUrl(raw))
}

/// A completed HTTP exchange. Anything that did not complete is an `Err` instead.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct AssetResponse {
    pub(crate) status: u16,
    pub(crate) body: Vec<u8>,
}

/// Either the evidence is decided, or one more asset must be fetched.
#[derive(Debug)]
pub(crate) enum Probe {
    Fetch(PendingFetch),
    Done(SignatureEvidence),
}

#[derive(Debug)]
pub(crate) struct PendingFetch {
    version: String,
    base: String,
    assets: ReleaseAssets,
    /// Set once the signature asset has been read; the manifest is fetched next.
    signature: Option<Vec<u8>>,
}

/// Starts gathering evidence for `version` (the request's `release.version`) from the
/// format-2 asset pair.
pub(crate) fn probe(base: &ManifestBaseUrl, version: Option<&str>) -> Probe {
    probe_assets(base, version, FORMAT_2_ASSETS)
}

pub(crate) fn probe_assets(
    base: &ManifestBaseUrl,
    version: Option<&str>,
    assets: ReleaseAssets,
) -> Probe {
    let v = trim_space(version.unwrap_or(""));
    if v.is_empty() {
        // An edge build or a revert to an unnamed build: nothing could have signed it.
        return Probe::Done(SignatureEvidence::Absent {
            why: "the request names no release version, so there is no published release manifest to verify against".into(),
        });
    }
    if !VERSION_RE.is_match(v) {
        return Probe::Done(SignatureEvidence::FetchError {
            error: format!(
                "release version {} is not a semver release version; refusing to compose an asset URL from it",
                quote(v)
            ),
        });
    }
    Probe::Fetch(PendingFetch {
        version: v.to_owned(),
        base: base.0.replace("{version}", v),
        assets,
        signature: None,
    })
}

#[cfg(test)]
impl Probe {
    /// Drives the probe with a blocking fetcher.
    pub(crate) fn run(
        self,
        mut fetch: impl FnMut(&str) -> Result<AssetResponse, String>,
    ) -> SignatureEvidence {
        let mut probe = self;
        loop {
            match probe {
                Probe::Done(evidence) => return evidence,
                Probe::Fetch(pending) => {
                    let outcome = fetch(&pending.url());
                    probe = pending.observe(outcome);
                }
            }
        }
    }
}

impl PendingFetch {
    /// The asset to fetch: the signature first, since it alone decides absence.
    pub(crate) fn url(&self) -> String {
        let asset = if self.signature.is_none() {
            self.assets.signature
        } else {
            self.assets.manifest
        };
        format!("{}{asset}", self.base)
    }

    pub(crate) fn observe(self, outcome: Result<AssetResponse, String>) -> Probe {
        let url = self.url();
        let outcome = outcome.and_then(|r| {
            if r.status == 200 && r.body.len() > MAX_ASSET_BYTES {
                Err(format!("asset is larger than {MAX_ASSET_BYTES} bytes"))
            } else {
                Ok(r)
            }
        });
        let fetch_error = |error: String| Probe::Done(SignatureEvidence::FetchError { error });
        match (self.signature, outcome) {
            (_, Err(e)) => fetch_error(format!("fetching {url}: {e}")),
            (None, Ok(r)) if r.status == 404 => Probe::Done(SignatureEvidence::Absent {
                why: format!(
                    "release {} publishes no {} asset",
                    self.version, self.assets.signature
                ),
            }),
            (None, Ok(r)) if r.status == 200 => Probe::Fetch(PendingFetch {
                version: self.version,
                base: self.base,
                assets: self.assets,
                signature: Some(r.body),
            }),
            (None, Ok(r)) => fetch_error(format!("{url} answered HTTP {}", r.status)),
            (Some(signature), Ok(r)) if r.status == 200 => Probe::Done(SignatureEvidence::Signed {
                manifest: r.body,
                signature,
            }),
            // A signature with no manifest beside it is a broken publish, never unsigned.
            (Some(_), Ok(r)) => fetch_error(format!(
                "release {} publishes a signature but {url} answered HTTP {}",
                self.version, r.status
            )),
        }
    }
}

/// The redirect policy (`CheckRedirect`): at most nine redirects, and never from an
/// https origin to anything but https, where a forged 404 would read as "unsigned".
/// `via` holds the scheme of every request made so far, the original first.
pub(crate) fn redirect_allowed(via: &[&str], next: &str) -> Result<(), String> {
    if via.len() >= MAX_REDIRECTS {
        return Err(format!("stopped after {MAX_REDIRECTS} redirects"));
    }
    match via.first() {
        Some(&"https") if next != "https" => {
            Err(format!("refusing a redirect from https to {next}"))
        }
        _ => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_manifest_timeout_wraps_like_go() {
        // Each row observed from Go: time.Duration(n) * time.Second, then envInt's and
        // Evidence's "not positive is the default".
        assert_eq!(parse_manifest_timeout("15"), Duration::from_secs(15));
        assert_eq!(parse_manifest_timeout(""), DEFAULT_ASSET_TIMEOUT);
        assert_eq!(parse_manifest_timeout("0"), DEFAULT_ASSET_TIMEOUT);
        assert_eq!(parse_manifest_timeout("-3"), DEFAULT_ASSET_TIMEOUT);
        assert_eq!(parse_manifest_timeout(" 5"), DEFAULT_ASSET_TIMEOUT);
        assert_eq!(
            parse_manifest_timeout("9223372036854775807"),
            DEFAULT_ASSET_TIMEOUT
        );
        assert_eq!(
            parse_manifest_timeout("18446744074"),
            Duration::from_nanos(290_448_384)
        );
        assert_eq!(parse_manifest_timeout("18446744073"), DEFAULT_ASSET_TIMEOUT);
        assert_eq!(
            parse_manifest_timeout("9223372036"),
            Duration::from_nanos(9_223_372_036_000_000_000)
        );
    }
}
