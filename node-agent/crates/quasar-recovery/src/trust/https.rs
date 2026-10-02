//! The release-asset fetch: the verifier fetching the manifest and its signature itself
//! (ADR 0003), with the behaviour of the Go client it was ported from (on Go's
//! `http.DefaultTransport`):
//!
//! - one deadline over both fetches (`QUASAR_UPDATER_MANIFEST_TIMEOUT_S`), a 30 s dial
//!   and a 10 s TLS handshake bound;
//! - redirects 301/302/303/307/308 followed only as `redirect_allowed` permits, a 3xx
//!   with no `Location` returned as a response;
//! - `Accept: application/octet-stream`, `User-Agent: quasar-recovery`, gzip requested and
//!   decoded, basic auth from URL userinfo, `Referer` on a redirect;
//! - at most `MAX_ASSET_BYTES + 1` bytes of a 200 body read.
//!
//! Every failure to complete is an `Err`, which `probe` turns into a fetch error; only a
//! completed 404 on the signature asset reads as unsigned.
//!
//! Where Go's transport does something this one does not, it refuses instead: a request
//! routed through `HTTPS_PROXY` fails (no proxy client here), as does a non-ASCII host (no
//! IDNA). Roots are webpki-roots (Mozilla's set), not the image's `ca-certificates`, and
//! `SSL_CERT_FILE`/`SSL_CERT_DIR` are ignored; HTTP/2 is not offered. None of these reads a
//! failure as unsigned: `verify`/`require` fail closed behind a proxy or a private CA
//! (documented limits, `docs/configuration.md` "Release trust").

use std::sync::Arc;
use std::time::Duration;

use base64::Engine as _;
use bytes::Bytes;
use http_body_util::{BodyExt, Empty};
use hyper_util::rt::TokioIo;
use rustls::pki_types::ServerName;
use tokio::net::TcpStream;
use tokio::time::{timeout, timeout_at, Instant};
use tokio_rustls::TlsConnector;

use super::golang::proxy::ProxyEnv;
use super::golang::text::quote_bytes;
use super::golang::url::{self, Url};
use super::signature::SignatureEvidence;
use super::source::{
    probe, redirect_allowed, AssetResponse, ManifestBaseUrl, Probe, MAX_ASSET_BYTES,
};

const DIAL_TIMEOUT: Duration = Duration::from_secs(30);
const TLS_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
/// Go's `MaxResponseHeaderBytes` default.
const MAX_RESPONSE_HEADER_BYTES: usize = 10 << 20;

/// The HTTPS client the recovery actor fetches release assets with.
pub struct HttpsFetcher {
    tls: TlsConnector,
    proxy: ProxyEnv,
    handshake_timeout: Duration,
}

impl HttpsFetcher {
    /// Production: webpki roots, and the proxy variables as this process sees them now.
    pub fn from_env() -> Self {
        let mut roots = rustls::RootCertStore::empty();
        roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned());
        Self::with_roots(roots, ProxyEnv::from_process(), TLS_HANDSHAKE_TIMEOUT)
    }

    fn with_roots(
        roots: rustls::RootCertStore,
        proxy: ProxyEnv,
        handshake_timeout: Duration,
    ) -> Self {
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let mut config = rustls::ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .expect("ring provider supports the default protocol versions")
            .with_root_certificates(roots)
            .with_no_client_auth();
        config.alpn_protocols = vec![b"http/1.1".to_vec()];
        HttpsFetcher {
            tls: TlsConnector::from(Arc::new(config)),
            proxy,
            handshake_timeout,
        }
    }

    /// Gathers the evidence for `version` under one deadline, as `Evidence(ctx, version)`.
    pub async fn evidence(
        &self,
        base: &ManifestBaseUrl,
        version: Option<&str>,
        limit: Duration,
    ) -> SignatureEvidence {
        // Past the clock's range is no deadline at all: refuse every fetch rather than wait.
        let deadline = Instant::now().checked_add(limit);
        let mut step = probe(base, version);
        loop {
            match step {
                Probe::Done(evidence) => return evidence,
                Probe::Fetch(pending) => {
                    let target = pending.url();
                    let outcome = match deadline {
                        None => Err(format!(
                            "Get {}: the manifest timeout {limit:?} is past the clock's range",
                            quote_bytes(target.as_bytes())
                        )),
                        Some(deadline) => match timeout_at(deadline, self.get(&target)).await {
                            Ok(outcome) => outcome,
                            Err(_) => Err(format!(
                                "Get {}: context deadline exceeded",
                                quote_bytes(target.as_bytes())
                            )),
                        },
                    };
                    step = pending.observe(outcome);
                }
            }
        }
    }

    /// One asset: `client.Do` plus `get`'s body handling. Only a completed exchange is `Ok`.
    async fn get(&self, target: &str) -> Result<AssetResponse, String> {
        let mut current = url::parse(target)?;
        let mut via: Vec<Url> = Vec::new();
        let mut referer: Option<Vec<u8>> = None;
        loop {
            let shown = display(&current);
            // `_connection` drives the exchange until the body is read.
            let (response, _connection) = self
                .round_trip(&current, referer.as_deref())
                .await
                .map_err(|e| format!("Get {}: {e}", quote_bytes(&shown)))?;
            let status = response.status().as_u16();
            let location = response
                .headers()
                .get(http::header::LOCATION)
                .map(|v| v.as_bytes().to_vec());
            if matches!(status, 301 | 302 | 303 | 307 | 308) {
                if let Some(location) = location.filter(|l| !l.is_empty()) {
                    let location_text = String::from_utf8_lossy(&location).into_owned();
                    let next = current.join(&location_text).map_err(|e| {
                        format!(
                            "Get {}: failed to parse Location header {}: {e}",
                            quote_bytes(&shown),
                            quote_bytes(&location)
                        )
                    })?;
                    via.push(current.clone());
                    let via_schemes: Vec<&str> = via.iter().map(|u| u.scheme.as_str()).collect();
                    redirect_allowed(&via_schemes, &next.scheme)
                        .map_err(|e| format!("Get {}: {e}", quote_bytes(&location)))?;
                    referer = Some(display(&current));
                    current = next;
                    continue;
                }
            }
            if status != 200 {
                return Ok(AssetResponse {
                    status,
                    body: Vec::new(),
                });
            }
            let gzip = response
                .headers()
                .get(http::header::CONTENT_ENCODING)
                .is_some_and(|v| v.as_bytes().eq_ignore_ascii_case(b"gzip"));
            let body = read_capped(response.into_body(), gzip).await?;
            return Ok(AssetResponse { status, body });
        }
    }

    /// One request over a fresh TLS connection.
    async fn round_trip(
        &self,
        target: &Url,
        referer: Option<&[u8]>,
    ) -> Result<(http::Response<hyper::body::Incoming>, AbortOnDrop), String> {
        if target.scheme != "https" {
            return Err(format!(
                "unsupported protocol scheme {}",
                quote_bytes(target.scheme.as_bytes())
            ));
        }
        let (host, port) = target.host_port();
        if host.is_empty() {
            return Err("http: no Host in request URL".into());
        }
        let host = String::from_utf8(host)
            .ok()
            .filter(|h| h.is_ascii())
            .ok_or_else(|| {
                "a non-ASCII host name needs IDNA, which this client does not do; refusing"
                    .to_string()
            })?;
        if let Some(proxy) = self.proxy.proxy_for(target) {
            return Err(format!(
                "proxyconnect: HTTPS_PROXY selects {} for this request and this client has no proxy support; refusing to connect around it",
                quote_bytes(format!("{}://{}", proxy.scheme, String::from_utf8_lossy(&proxy.host)).as_bytes())
            ));
        }
        let port: u16 = if port.is_empty() {
            443
        } else {
            std::str::from_utf8(&port)
                .ok()
                .and_then(|p| p.parse().ok())
                .ok_or_else(|| {
                    format!(
                        "dial tcp: address {}: invalid port",
                        String::from_utf8_lossy(&port)
                    )
                })?
        };
        let server_name = ServerName::try_from(host.clone()).map_err(|e| format!("tls: {e}"))?;
        let tcp = timeout(DIAL_TIMEOUT, TcpStream::connect((host.as_str(), port)))
            .await
            .map_err(|_| "dial tcp: i/o timeout".to_string())?
            .map_err(|e| format!("dial tcp: {e}"))?;
        let tls = timeout(self.handshake_timeout, self.tls.connect(server_name, tcp))
            .await
            .map_err(|_| "net/http: TLS handshake timeout".to_string())?
            .map_err(|e| format!("tls: {e}"))?;

        let (mut sender, connection) = hyper::client::conn::http1::Builder::new()
            .max_buf_size(MAX_RESPONSE_HEADER_BYTES)
            .max_headers(MAX_RESPONSE_HEADER_BYTES / 64)
            .handshake::<_, Empty<Bytes>>(TokioIo::new(tls))
            .await
            .map_err(|e| e.to_string())?;
        // Aborted with the guard, so a deadline never leaves the connection running.
        let driver = AbortOnDrop(tokio::spawn(async move {
            let _ = connection.await;
        }));

        let mut request = http::Request::builder()
            .method(http::Method::GET)
            .uri(String::from_utf8_lossy(&target.request_uri()).into_owned())
            .header(http::header::HOST, target.host.as_slice())
            .header(http::header::USER_AGENT, "quasar-recovery")
            .header(http::header::ACCEPT, "application/octet-stream")
            .header(http::header::ACCEPT_ENCODING, "gzip");
        if let Some(user) = &target.user {
            let mut credentials = user.username.clone();
            credentials.push(b':');
            credentials.extend(user.password.clone().unwrap_or_default());
            let value = format!(
                "Basic {}",
                base64::engine::general_purpose::STANDARD.encode(credentials)
            );
            request = request.header(http::header::AUTHORIZATION, value);
        }
        if let Some(referer) = referer {
            request = request.header(http::header::REFERER, referer);
        }
        let request = request
            .body(Empty::<Bytes>::new())
            .map_err(|e| format!("net/http: {e}"))?;
        let response = sender
            .send_request(request)
            .await
            .map_err(|e| e.to_string())?;
        Ok((response, driver))
    }
}

struct AbortOnDrop(tokio::task::JoinHandle<()>);

impl Drop for AbortOnDrop {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// `scheme://host` + the request URI, without userinfo: Go's error and `Referer` text.
fn display(u: &Url) -> Vec<u8> {
    let mut out = format!("{}://", u.scheme).into_bytes();
    out.extend_from_slice(&u.host);
    out.extend(u.request_uri());
    out
}

/// Reads a 200 body, decoding gzip, and stops past `MAX_ASSET_BYTES` (Go's
/// `io.LimitReader(resp.Body, MaxAssetBytes+1)`).
async fn read_capped(mut body: hyper::body::Incoming, gzip: bool) -> Result<Vec<u8>, String> {
    let mut capped = CappedBody::new(gzip, MAX_ASSET_BYTES + 1);
    while let Some(frame) = body.frame().await {
        let frame = frame.map_err(|e| e.to_string())?;
        let Ok(chunk) = frame.into_data() else {
            continue;
        };
        if capped.push(&chunk)? {
            return Ok(capped.into_bytes());
        }
    }
    capped.finish()
}

/// A body's bytes, decoded, up to `cap` and never beyond: gzip inflates into a sink that
/// refuses the byte after `cap`, so a bomb costs at most `cap` plus the decoder's own
/// 32 KiB window, not whatever one received chunk inflates to.
struct CappedBody {
    decoder: Option<flate2::write::MultiGzDecoder<Bounded>>,
    plain: Bounded,
    /// The most decoded bytes ever held at once.
    #[cfg(test)]
    peak: usize,
}

/// A byte sink that takes at most `cap` bytes, then refuses.
struct Bounded {
    bytes: Vec<u8>,
    cap: usize,
}

impl Bounded {
    fn full(&self) -> bool {
        self.bytes.len() >= self.cap
    }

    fn take(&mut self, data: &[u8]) -> usize {
        let n = data.len().min(self.cap - self.bytes.len());
        self.bytes.extend_from_slice(&data[..n]);
        n
    }
}

impl std::io::Write for Bounded {
    fn write(&mut self, data: &[u8]) -> std::io::Result<usize> {
        if self.full() && !data.is_empty() {
            return Err(std::io::Error::other("decoded body reached its limit"));
        }
        Ok(self.take(data))
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

impl CappedBody {
    fn new(gzip: bool, cap: usize) -> Self {
        let sink = || Bounded {
            bytes: Vec::new(),
            cap,
        };
        CappedBody {
            decoder: gzip.then(|| flate2::write::MultiGzDecoder::new(sink())),
            plain: sink(),
            #[cfg(test)]
            peak: 0,
        }
    }

    /// Takes one received chunk; `true` once `cap` bytes are held.
    fn push(&mut self, chunk: &[u8]) -> Result<bool, String> {
        use std::io::Write as _;
        let full = match &mut self.decoder {
            Some(d) => {
                let written = d.write_all(chunk);
                if d.get_ref().full() {
                    true
                } else {
                    written.map_err(|e| format!("gzip: {e}"))?;
                    false
                }
            }
            None => {
                self.plain.take(chunk);
                self.plain.full()
            }
        };
        #[cfg(test)]
        {
            let held = self
                .decoder
                .as_ref()
                .map_or(&self.plain, |d| d.get_ref())
                .bytes
                .len();
            self.peak = self.peak.max(held);
        }
        Ok(full)
    }

    fn into_bytes(self) -> Vec<u8> {
        match self.decoder {
            Some(d) => d.get_ref().bytes.clone(),
            None => self.plain.bytes,
        }
    }

    fn finish(self) -> Result<Vec<u8>, String> {
        match self.decoder {
            Some(d) => d
                .finish()
                .map(|b| b.bytes)
                .map_err(|e| format!("gzip: {e}")),
            None => Ok(self.plain.bytes),
        }
    }
}

#[cfg(test)]
mod test_pki;
#[cfg(test)]
mod tests;
