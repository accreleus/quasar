//! The adapter against a local TLS server with a throwaway CA.

use std::io::Write as _;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio_rustls::TlsAcceptor;

use super::test_pki::{self, Issued};
use super::{HttpsFetcher, MAX_ASSET_BYTES};
use crate::trust::golang::proxy::ProxyEnv;
use crate::trust::source::parse_manifest_base_url;
use crate::trust::SignatureEvidence;

const SIG: &str = "platform-release-manifest.v2.json.sig";
const MANIFEST: &str = "platform-release-manifest.v2.json";
const LIMIT: Duration = Duration::from_secs(15);

#[derive(Clone)]
enum Reply {
    Ok(Vec<u8>),
    Gzip(Vec<u8>),
    Status(u16),
    Redirect(u16, String),
    /// Closes the connection once the request has been read.
    Drop,
    /// Reads the request and never answers.
    Stall,
    Raw(Vec<u8>),
    /// A chunked 200 body that never ends.
    Endless,
}

#[derive(Debug, Clone)]
struct Seen {
    target: String,
    headers: Vec<(String, String)>,
}

impl Seen {
    fn header(&self, name: &str) -> Option<&str> {
        self.headers
            .iter()
            .find(|(n, _)| n == name)
            .map(|(_, v)| v.as_str())
    }
}

type Router = Arc<dyn Fn(&str, u16) -> Reply + Send + Sync>;

struct Server {
    port: u16,
    seen: Arc<Mutex<Vec<Seen>>>,
}

impl Server {
    fn origin(&self) -> String {
        format!("https://localhost:{}", self.port)
    }

    fn base(&self) -> crate::trust::ManifestBaseUrl {
        parse_manifest_base_url(&format!("{}/rel/v{{version}}/", self.origin())).unwrap()
    }

    fn targets(&self) -> Vec<String> {
        self.seen
            .lock()
            .unwrap()
            .iter()
            .map(|s| s.target.clone())
            .collect()
    }

    fn seen(&self) -> Vec<Seen> {
        self.seen.lock().unwrap().clone()
    }
}

fn server_config(cert: &Issued) -> Arc<rustls::ServerConfig> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    Arc::new(
        rustls::ServerConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_no_client_auth()
            .with_single_cert(vec![cert.cert.clone()], cert.key.clone_key())
            .unwrap(),
    )
}

async fn serve(
    cert: &Issued,
    router: impl Fn(&str, u16) -> Reply + Send + Sync + 'static,
) -> Server {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let acceptor = TlsAcceptor::from(server_config(cert));
    let seen = Arc::new(Mutex::new(Vec::new()));
    let router: Router = Arc::new(router);
    let log = seen.clone();
    tokio::spawn(async move {
        loop {
            let Ok((tcp, _)) = listener.accept().await else {
                return;
            };
            let (acceptor, router, log) = (acceptor.clone(), router.clone(), log.clone());
            tokio::spawn(async move {
                let Ok(mut tls) = acceptor.accept(tcp).await else {
                    return;
                };
                let mut head = Vec::new();
                let mut byte = [0u8; 1];
                while !head.ends_with(b"\r\n\r\n") {
                    if tls.read(&mut byte).await.unwrap_or(0) == 0 {
                        return;
                    }
                    head.push(byte[0]);
                }
                let text = String::from_utf8_lossy(&head).into_owned();
                let mut lines = text.split("\r\n");
                let target = lines.next().unwrap().split(' ').nth(1).unwrap().to_owned();
                let headers = lines
                    .filter_map(|l| l.split_once(':'))
                    .map(|(n, v)| (n.trim().to_ascii_lowercase(), v.trim().to_owned()))
                    .collect();
                log.lock().unwrap().push(Seen {
                    target: target.clone(),
                    headers,
                });
                let respond = |code: u16, extra: &str, body: &[u8]| {
                    let mut r = format!("HTTP/1.1 {code} X\r\nContent-Length: {}\r\nConnection: close\r\n{extra}\r\n", body.len())
                        .into_bytes();
                    r.extend_from_slice(body);
                    r
                };
                let bytes = match router(&target, port) {
                    Reply::Ok(body) => respond(200, "", &body),
                    Reply::Gzip(body) => {
                        let mut enc = flate2::write::GzEncoder::new(
                            Vec::new(),
                            flate2::Compression::default(),
                        );
                        enc.write_all(&body).unwrap();
                        respond(200, "Content-Encoding: gzip\r\n", &enc.finish().unwrap())
                    }
                    Reply::Status(code) => respond(code, "", b"no"),
                    Reply::Redirect(code, location) => {
                        respond(code, &format!("Location: {location}\r\n"), b"")
                    }
                    Reply::Drop => return,
                    Reply::Stall => {
                        tokio::time::sleep(Duration::from_secs(3600)).await;
                        return;
                    }
                    Reply::Raw(bytes) => bytes,
                    Reply::Endless => {
                        let head = b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n";
                        if tls.write_all(head).await.is_err() {
                            return;
                        }
                        let mut chunk = b"1000\r\n".to_vec();
                        chunk.extend(vec![b'a'; 0x1000]);
                        chunk.extend(b"\r\n");
                        while tls.write_all(&chunk).await.is_ok() {}
                        return;
                    }
                };
                let _ = tls.write_all(&bytes).await;
                let _ = tls.shutdown().await;
            });
        }
    });
    Server { port, seen }
}

fn fetcher(ca: &Issued) -> HttpsFetcher {
    fetcher_with(ca, ProxyEnv::default(), Duration::from_secs(10))
}

fn fetcher_with(ca: &Issued, proxy: ProxyEnv, handshake: Duration) -> HttpsFetcher {
    let mut roots = rustls::RootCertStore::empty();
    roots.add(ca.cert.clone()).unwrap();
    HttpsFetcher::with_roots(roots, proxy, handshake)
}

fn pki() -> (Issued, Issued) {
    let ca = test_pki::ca("quasar-recovery test CA");
    let leaf = test_pki::server(&ca);
    (ca, leaf)
}

fn path(asset: &str) -> String {
    format!("/rel/v0.3.0/{asset}")
}

/// Serves a signature and a manifest at the probe's paths, anything else 404.
fn release(sig: Reply, manifest: Reply) -> impl Fn(&str, u16) -> Reply + Send + Sync + 'static {
    move |target, _| match target {
        t if t == path(SIG) => sig.clone(),
        t if t == path(MANIFEST) => manifest.clone(),
        _ => Reply::Status(404),
    }
}

fn fetch_error(ev: SignatureEvidence) -> String {
    match ev {
        SignatureEvidence::FetchError { error } => error,
        other => panic!("want a fetch error, got {other:?}"),
    }
}

#[tokio::test]
async fn both_assets_are_signed_evidence_at_the_urls_the_probe_computes() {
    let (ca, leaf) = pki();
    let srv = serve(
        &leaf,
        release(Reply::Ok(b"SIG".to_vec()), Reply::Ok(b"MANIFEST".to_vec())),
    )
    .await;
    let ev = fetcher(&ca)
        .evidence(&srv.base(), Some("0.3.0"), LIMIT)
        .await;
    assert_eq!(
        ev,
        SignatureEvidence::Signed {
            manifest: b"MANIFEST".to_vec(),
            signature: b"SIG".to_vec()
        }
    );
    assert_eq!(srv.targets(), vec![path(SIG), path(MANIFEST)]);
    let first = &srv.seen()[0];
    assert_eq!(first.header("user-agent"), Some("quasar-recovery"));
    assert_eq!(first.header("accept"), Some("application/octet-stream"));
    assert_eq!(first.header("accept-encoding"), Some("gzip"));
    assert_eq!(
        first.header("host"),
        Some(format!("localhost:{}", srv.port).as_str())
    );
    assert_eq!(first.header("authorization"), None);
    assert_eq!(first.header("referer"), None);
}

#[tokio::test]
async fn only_a_signature_404_is_an_absence() {
    let (ca, leaf) = pki();
    let srv = serve(&leaf, release(Reply::Status(404), Reply::Ok(b"M".to_vec()))).await;
    let ev = fetcher(&ca)
        .evidence(&srv.base(), Some("0.3.0"), LIMIT)
        .await;
    assert_eq!(
        ev,
        SignatureEvidence::Absent {
            why: format!("release 0.3.0 publishes no {SIG} asset")
        }
    );
    assert_eq!(
        srv.targets(),
        vec![path(SIG)],
        "the manifest is not fetched once absence is decided"
    );
}

#[tokio::test]
async fn any_other_signature_status_is_a_fetch_error() {
    for code in [410, 500, 503, 401] {
        let (ca, leaf) = pki();
        let srv = serve(
            &leaf,
            release(Reply::Status(code), Reply::Ok(b"M".to_vec())),
        )
        .await;
        let err = fetch_error(
            fetcher(&ca)
                .evidence(&srv.base(), Some("0.3.0"), LIMIT)
                .await,
        );
        assert_eq!(
            err,
            format!("{}{} answered HTTP {code}", srv.origin(), path(SIG))
        );
    }
}

#[tokio::test]
async fn a_missing_manifest_beside_a_signature_is_a_fetch_error() {
    let (ca, leaf) = pki();
    let srv = serve(&leaf, release(Reply::Ok(b"S".to_vec()), Reply::Status(404))).await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(
        err.starts_with("release 0.3.0 publishes a signature but"),
        "{err}"
    );
}

#[tokio::test]
async fn a_refused_connection_is_a_fetch_error() {
    let (ca, _) = pki();
    let port = {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        l.local_addr().unwrap().port()
    };
    let base =
        parse_manifest_base_url(&format!("https://127.0.0.1:{port}/rel/v{{version}}/")).unwrap();
    let err = fetch_error(fetcher(&ca).evidence(&base, Some("0.3.0"), LIMIT).await);
    let url = format!("https://127.0.0.1:{port}{}", path(SIG));
    assert!(
        err.starts_with(&format!("fetching {url}: Get \"{url}\": dial tcp:")),
        "{err}"
    );
}

#[tokio::test]
async fn a_dropped_connection_is_a_fetch_error() {
    let (ca, leaf) = pki();
    let srv = serve(&leaf, release(Reply::Drop, Reply::Ok(b"M".to_vec()))).await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(
        err.starts_with(&format!("fetching {}{}: Get ", srv.origin(), path(SIG))),
        "{err}"
    );
}

#[tokio::test]
async fn a_truncated_body_is_a_fetch_error() {
    let (ca, leaf) = pki();
    let raw =
        b"HTTP/1.1 200 OK\r\nContent-Length: 100\r\nConnection: close\r\n\r\nonly ten b".to_vec();
    let srv = serve(&leaf, release(Reply::Raw(raw), Reply::Ok(b"M".to_vec()))).await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(
        err.starts_with(&format!("fetching {}{}: ", srv.origin(), path(SIG))),
        "{err}"
    );
}

#[tokio::test]
async fn an_untrusted_certificate_is_a_fetch_error() {
    let (ca, _) = pki();
    let (_, stranger) = pki();
    let srv = serve(
        &stranger,
        release(Reply::Ok(b"S".to_vec()), Reply::Ok(b"M".to_vec())),
    )
    .await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(err.contains("tls:") && err.contains("certificate"), "{err}");
    assert!(
        srv.targets().is_empty(),
        "nothing is sent over an unverified connection"
    );
}

#[tokio::test]
async fn a_stalled_tls_handshake_is_a_fetch_error() {
    let (ca, _) = pki();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move {
        let mut held = Vec::new();
        while let Ok((tcp, _)) = listener.accept().await {
            held.push(tcp); // accepted, never spoken to
        }
    });
    let base =
        parse_manifest_base_url(&format!("https://localhost:{port}/rel/v{{version}}/")).unwrap();
    let f = fetcher_with(&ca, ProxyEnv::default(), Duration::from_millis(200));
    let err = fetch_error(f.evidence(&base, Some("0.3.0"), LIMIT).await);
    assert!(err.ends_with("net/http: TLS handshake timeout"), "{err}");
}

#[tokio::test]
async fn the_deadline_covers_both_fetches() {
    let (ca, leaf) = pki();
    let srv = serve(&leaf, release(Reply::Ok(b"S".to_vec()), Reply::Stall)).await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), Duration::from_millis(500))
            .await,
    );
    let url = format!("{}{}", srv.origin(), path(MANIFEST));
    assert_eq!(
        err,
        format!("fetching {url}: Get \"{url}\": context deadline exceeded")
    );
}

#[tokio::test]
async fn an_oversized_body_is_a_fetch_error_and_the_limit_itself_is_read() {
    let (ca, leaf) = pki();
    let srv = serve(
        &leaf,
        release(
            Reply::Ok(vec![b'a'; MAX_ASSET_BYTES + 1]),
            Reply::Ok(b"M".to_vec()),
        ),
    )
    .await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert_eq!(
        err,
        format!(
            "fetching {}{}: asset is larger than 1048576 bytes",
            srv.origin(),
            path(SIG)
        )
    );

    let srv = serve(
        &leaf,
        release(
            Reply::Ok(vec![b'a'; MAX_ASSET_BYTES]),
            Reply::Ok(b"M".to_vec()),
        ),
    )
    .await;
    let ev = fetcher(&ca)
        .evidence(&srv.base(), Some("0.3.0"), LIMIT)
        .await;
    assert!(
        matches!(ev, SignatureEvidence::Signed { signature, .. } if signature.len() == MAX_ASSET_BYTES)
    );
}

#[tokio::test]
async fn reading_stops_at_the_limit_rather_than_at_the_end_of_the_body() {
    let (ca, leaf) = pki();
    let srv = serve(&leaf, release(Reply::Endless, Reply::Ok(b"M".to_vec()))).await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), Duration::from_secs(20))
            .await,
    );
    assert_eq!(
        err,
        format!(
            "fetching {}{}: asset is larger than 1048576 bytes",
            srv.origin(),
            path(SIG)
        )
    );
}

#[tokio::test]
async fn gzip_is_decoded_and_the_limit_applies_to_the_decoded_bytes() {
    let (ca, leaf) = pki();
    let srv = serve(
        &leaf,
        release(Reply::Gzip(b"SIGNATURE".to_vec()), Reply::Ok(b"M".to_vec())),
    )
    .await;
    let ev = fetcher(&ca)
        .evidence(&srv.base(), Some("0.3.0"), LIMIT)
        .await;
    assert_eq!(
        ev,
        SignatureEvidence::Signed {
            manifest: b"M".to_vec(),
            signature: b"SIGNATURE".to_vec()
        }
    );

    let srv = serve(
        &leaf,
        release(
            Reply::Gzip(vec![0; MAX_ASSET_BYTES + 1]),
            Reply::Ok(b"M".to_vec()),
        ),
    )
    .await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(err.ends_with("asset is larger than 1048576 bytes"), "{err}");
}

#[tokio::test]
async fn a_corrupt_gzip_body_is_a_fetch_error() {
    let (ca, leaf) = pki();
    let raw = b"HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: 4\r\nConnection: close\r\n\r\nnope".to_vec();
    let srv = serve(&leaf, release(Reply::Raw(raw), Reply::Ok(b"M".to_vec()))).await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(err.contains("gzip"), "{err}");
}

#[tokio::test]
async fn https_redirects_are_followed_with_a_referer() {
    let (ca, leaf) = pki();
    let srv = serve(&leaf, |target, port| match target {
        t if t == path(SIG) => {
            Reply::Redirect(302, format!("https://localhost:{port}/storage/sig"))
        }
        "/storage/sig" => Reply::Ok(b"S".to_vec()),
        t if t == path(MANIFEST) => Reply::Redirect(307, "../../storage/manifest".into()),
        "/storage/manifest" => Reply::Ok(b"M".to_vec()),
        _ => Reply::Status(404),
    })
    .await;
    let ev = fetcher(&ca)
        .evidence(&srv.base(), Some("0.3.0"), LIMIT)
        .await;
    assert_eq!(
        ev,
        SignatureEvidence::Signed {
            manifest: b"M".to_vec(),
            signature: b"S".to_vec()
        }
    );
    assert_eq!(
        srv.targets(),
        vec![
            path(SIG),
            "/storage/sig".into(),
            path(MANIFEST),
            "/storage/manifest".into()
        ]
    );
    let seen = srv.seen();
    assert_eq!(
        seen[1].header("referer"),
        Some(format!("{}{}", srv.origin(), path(SIG)).as_str())
    );
    assert_eq!(seen[1].header("user-agent"), Some("quasar-recovery"));
}

#[tokio::test]
async fn a_redirect_off_tls_is_refused_before_it_is_followed() {
    let (ca, leaf) = pki();
    let srv = serve(&leaf, |target, port| match target {
        t if t == path(SIG) => Reply::Redirect(301, format!("http://localhost:{port}/plain")),
        _ => Reply::Status(404),
    })
    .await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(
        err.ends_with(": refusing a redirect from https to http"),
        "{err}"
    );
    assert_eq!(srv.targets(), vec![path(SIG)]);
}

#[tokio::test]
async fn nine_redirects_are_followed_and_the_tenth_is_refused() {
    for (hops, ok) in [(9, true), (10, false)] {
        let (ca, leaf) = pki();
        let srv = serve(&leaf, move |target, _| {
            let n: usize = target
                .strip_prefix("/hop/")
                .and_then(|n| n.parse().ok())
                .unwrap_or(0);
            match target {
                t if t == path(MANIFEST) => Reply::Ok(b"M".to_vec()),
                t if t == path(SIG) || n < hops => Reply::Redirect(302, format!("/hop/{}", n + 1)),
                _ => Reply::Ok(b"S".to_vec()),
            }
        })
        .await;
        let ev = fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await;
        if ok {
            assert!(
                matches!(ev, SignatureEvidence::Signed { .. }),
                "{hops}: {ev:?}"
            );
        } else {
            assert!(
                fetch_error(ev).ends_with(": stopped after 10 redirects"),
                "{hops}"
            );
        }
    }
}

#[tokio::test]
async fn a_redirect_without_a_location_is_a_response() {
    let (ca, leaf) = pki();
    let raw = b"HTTP/1.1 302 Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".to_vec();
    let srv = serve(&leaf, release(Reply::Raw(raw), Reply::Ok(b"M".to_vec()))).await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert_eq!(
        err,
        format!("{}{} answered HTTP 302", srv.origin(), path(SIG))
    );
}

#[tokio::test]
async fn an_unparsable_location_is_a_fetch_error() {
    let (ca, leaf) = pki();
    let srv = serve(
        &leaf,
        release(
            Reply::Redirect(302, "https://[::1".into()),
            Reply::Ok(b"M".to_vec()),
        ),
    )
    .await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(err.contains("failed to parse Location header"), "{err}");
}

#[tokio::test]
async fn url_userinfo_becomes_basic_auth() {
    let (ca, leaf) = pki();
    let srv = serve(
        &leaf,
        release(Reply::Ok(b"S".to_vec()), Reply::Ok(b"M".to_vec())),
    )
    .await;
    let base = parse_manifest_base_url(&format!(
        "https://mirror:pa%3Ass@localhost:{}/rel/v{{version}}/",
        srv.port
    ))
    .unwrap();
    let ev = fetcher(&ca).evidence(&base, Some("0.3.0"), LIMIT).await;
    assert!(matches!(ev, SignatureEvidence::Signed { .. }), "{ev:?}");
    // base64("mirror:pa:ss")
    assert_eq!(
        srv.seen()[0].header("authorization"),
        Some("Basic bWlycm9yOnBhOnNz")
    );
}

#[tokio::test]
async fn a_request_go_would_proxy_is_refused_without_connecting() {
    let (ca, leaf) = pki();
    let srv = serve(
        &leaf,
        release(Reply::Ok(b"S".to_vec()), Reply::Ok(b"M".to_vec())),
    )
    .await;
    let proxy = ProxyEnv {
        https_proxy: "proxy.example.invalid:3128".into(),
        no_proxy: String::new(),
    };
    let base = parse_manifest_base_url("https://mirror.example.invalid/rel/v{version}/").unwrap();
    let err = fetch_error(
        fetcher_with(&ca, proxy.clone(), Duration::from_secs(10))
            .evidence(&base, Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(
        err.contains("proxyconnect") && err.contains("refusing"),
        "{err}"
    );

    // Go never proxies localhost, so neither does this client.
    let ev = fetcher_with(&ca, proxy, Duration::from_secs(10))
        .evidence(&srv.base(), Some("0.3.0"), LIMIT)
        .await;
    assert!(matches!(ev, SignatureEvidence::Signed { .. }), "{ev:?}");
}

#[tokio::test]
async fn a_non_ascii_host_is_refused() {
    let (ca, _) = pki();
    let base =
        parse_manifest_base_url("https://b\u{fc}cher.example.invalid/rel/v{version}/").unwrap();
    let err = fetch_error(fetcher(&ca).evidence(&base, Some("0.3.0"), LIMIT).await);
    assert!(err.contains("IDNA"), "{err}");
}

#[tokio::test]
async fn production_roots_are_webpki_and_refuse_the_test_ca() {
    let (_, leaf) = pki();
    let srv = serve(
        &leaf,
        release(Reply::Ok(b"S".to_vec()), Reply::Ok(b"M".to_vec())),
    )
    .await;
    let err = fetch_error(
        HttpsFetcher::from_env()
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(err.contains("tls:"), "{err}");
}

/// One incompressible MiB, then 32 MiB of zeros compressed about a thousandfold.
fn gzip_bomb() -> Vec<u8> {
    let mut random = vec![0u8; MAX_ASSET_BYTES];
    ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut random).unwrap();
    let mut enc = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
    enc.write_all(&random).unwrap();
    let zeros = vec![0u8; 1 << 20];
    for _ in 0..32 {
        enc.write_all(&zeros).unwrap();
    }
    enc.finish().unwrap()
}

#[test]
fn a_gzip_bomb_is_decoded_no_further_than_the_limit() {
    let bomb = gzip_bomb();
    let cap = MAX_ASSET_BYTES + 1;
    let mut body = super::CappedBody::new(true, cap);
    let mut full = false;
    for chunk in bomb.chunks(64 << 10) {
        if body.push(chunk).unwrap() {
            full = true;
            break;
        }
    }
    assert!(full, "the limit is reached");
    assert!(
        body.peak <= cap,
        "decoded {} bytes for a cap of {cap}",
        body.peak
    );
    assert_eq!(body.into_bytes().len(), cap);
}

#[tokio::test]
async fn a_gzip_bomb_is_refused() {
    let (ca, leaf) = pki();
    let raw = {
        let bomb = gzip_bomb();
        let mut r = format!(
            "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            bomb.len()
        )
        .into_bytes();
        r.extend(bomb);
        r
    };
    let srv = serve(&leaf, release(Reply::Raw(raw), Reply::Ok(b"M".to_vec()))).await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), LIMIT)
            .await,
    );
    assert!(err.ends_with("asset is larger than 1048576 bytes"), "{err}");
}

#[tokio::test]
async fn a_limit_past_the_clock_is_a_fetch_error_not_a_panic() {
    let (ca, leaf) = pki();
    let srv = serve(
        &leaf,
        release(Reply::Ok(b"S".to_vec()), Reply::Ok(b"M".to_vec())),
    )
    .await;
    let err = fetch_error(
        fetcher(&ca)
            .evidence(&srv.base(), Some("0.3.0"), Duration::MAX)
            .await,
    );
    assert!(err.contains("timeout"), "{err}");
    assert!(
        srv.targets().is_empty(),
        "nothing is fetched without a deadline"
    );
}
