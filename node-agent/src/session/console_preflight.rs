//! Console preflight (#407 RH07-15): before an agent created with console access
//! ([`crate::release::console::MARKER_ENV`]) ever reports healthy, check that it can
//! actually take the console display, and tell the recovery actor
//! (`POST /v1/console/preflight`, `testdata/recovery/agent-socket`, not frozen). A failed
//! preflight fails an in-flight ENABLE attempt at once (the actor answers `unhealthy` and
//! puts the previous agent back), and the text lands in `ConsoleLast.detail`, which
//! `release::console::derive` puts in front of the generic restored-summary reason.
//!
//! Kernel facts driving this (Linux ≥5.8, `drm_auth.c`; live-proven on nvidia-test
//! 2026-09-29, issue #407 comments): the first opener of a card node with no current
//! master becomes master with no capability check. Re-asserting master over an
//! already-held display needs `CAP_SYS_ADMIN`, checked against the host's initial user
//! namespace — a rootless container's own capability can never satisfy it. So a held
//! display fails `acquire_master_lock` with `EACCES`/`EPERM` on BOTH engine modes; only
//! the reason differs (rootful: lacks the grant too; rootless: no grant could ever help).
//! Owner decision 6: a held display is a named failure, never a crash — weston itself
//! would start and its atomic commits would just fail silently, which is why this module
//! checks for itself instead of trusting weston's exit.

use std::io;
use std::path::{Path, PathBuf};
use std::time::Duration;

use drm::Device as _;
use tracing::warn;

/// Bound the actor's `console_preflight.detail`: "one line, ≤1024 bytes"
/// (`testdata/recovery/agent-socket/README.md`).
const MAX_DETAIL_BYTES: usize = 1024;

/// What `POST /v1/console/preflight` carries.
// `detail` is never omitted: the fixture (`console-preflight-ok.json`) pins it present
// and explicitly `null` when `ok`, not absent.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct Preflight {
    pub ok: bool,
    pub detail: Option<String>,
}

impl Preflight {
    fn ok() -> Self {
        Preflight {
            ok: true,
            detail: None,
        }
    }

    fn fail(detail: String) -> Self {
        Preflight {
            ok: false,
            detail: Some(one_line(detail)),
        }
    }
}

/// Whichever primary DRM node this probe looked at is free, held by someone else, or
/// could not be opened at all. Kept apart from [`Preflight`] so the open error's kind
/// (permission vs. something else) can still drive the message.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DisplayState {
    Free,
    Held,
}

/// What the preflight needs from a real DRM card node, behind a trait so tests can
/// inject outcomes — no real DRM ioctl is available in the dev container (nor, for that
/// matter, a real display at all: only a hardware spike proves this against one).
trait DisplayProbe {
    fn probe(&self, path: &Path) -> io::Result<DisplayState>;
}

/// The real check: open read-write (an `EACCES`/`EPERM` here means the host was not
/// prepared with `--console`, not that the display is held), then test mastership by
/// re-issuing the DRM `SET_MASTER` ioctl — a no-op if the open already made this fd
/// master (the free case), `EACCES`/`EPERM` if another process holds it (see the module
/// doc). Frees the display again immediately so a subsequent `spawn_weston_console` can
/// take it; never leaves this probe holding master.
struct RealDisplayProbe;

impl DisplayProbe for RealDisplayProbe {
    fn probe(&self, path: &Path) -> io::Result<DisplayState> {
        struct Card(std::fs::File);
        impl std::os::fd::AsFd for Card {
            fn as_fd(&self) -> std::os::fd::BorrowedFd<'_> {
                self.0.as_fd()
            }
        }
        impl drm::Device for Card {}

        let file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(path)?;
        let card = Card(file);
        match card.acquire_master_lock() {
            Ok(()) => {
                let _ = card.release_master_lock();
                Ok(DisplayState::Free)
            }
            Err(e) if matches!(e.kind(), io::ErrorKind::PermissionDenied) => Ok(DisplayState::Held),
            Err(e) => Err(e),
        }
    }
}

/// `/dev/dri/card*` nodes, sorted, excluding `renderD*`. What the recipe passed a
/// rootless agent, or what a rootful one owns outright.
fn card_nodes(dri_root: &Path) -> Vec<PathBuf> {
    let mut nodes: Vec<PathBuf> = std::fs::read_dir(dri_root)
        .into_iter()
        .flatten()
        .flatten()
        .filter_map(|entry| {
            let name = entry.file_name().to_string_lossy().into_owned();
            let digits = name.strip_prefix("card")?;
            (!digits.is_empty() && digits.bytes().all(|b| b.is_ascii_digit()))
                .then(|| dri_root.join(name))
        })
        .collect();
    nodes.sort();
    nodes
}

/// `key=value` lines (systemd's seat/session state files), the first match.
fn parse_kv(body: &str, key: &str) -> Option<String> {
    let prefix = format!("{key}=");
    body.lines()
        .find_map(|l| l.strip_prefix(prefix.as_str()))
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
}

/// Names who holds the display from logind's world-readable seat/session state, bound
/// read-only at `host_run/systemd/{seats,sessions}` by the recipe when present (#407).
/// `None` when they are absent, unreadable, or name nobody in particular — the caller
/// falls back to a generic "another program holds the display".
fn name_holder(host_run: &Path) -> Option<String> {
    let seat0 = std::fs::read_to_string(host_run.join("systemd/seats/seat0")).ok()?;
    let session_id = parse_kv(&seat0, "ACTIVE")?;
    let session =
        std::fs::read_to_string(host_run.join("systemd/sessions").join(&session_id)).ok()?;
    let class = parse_kv(&session, "CLASS");
    match class.as_deref() {
        Some("greeter") => {
            let service = parse_kv(&session, "SERVICE");
            let who = service
                .as_deref()
                .and_then(|s| s.split('.').next())
                .filter(|s| !s.is_empty())
                .unwrap_or("the login screen");
            Some(format!("{who}, the login screen"))
        }
        Some("user") => {
            let who = parse_kv(&session, "USER").or_else(|| parse_kv(&session, "NAME"))?;
            match parse_kv(&session, "DESKTOP") {
                Some(desktop) => Some(format!("{who}'s {desktop} desktop")),
                None => Some(format!("{who}'s desktop session")),
            }
        }
        _ => None,
    }
}

/// One line, at most [`MAX_DETAIL_BYTES`] bytes, never splitting a UTF-8 character.
fn one_line(mut s: String) -> String {
    if s.contains(['\n', '\r']) {
        s = s.replace(['\n', '\r'], " ");
    }
    if s.len() > MAX_DETAIL_BYTES {
        s.truncate(MAX_DETAIL_BYTES);
        while !s.is_char_boundary(s.len()) {
            s.pop();
        }
    }
    s
}

const PREPARE_HOST_HINT: &str =
    "run host preparation (deploy/prepare-host.sh --console) as root, then try again";

fn run_with(probe: &dyn DisplayProbe, dri_root: &Path, host_run: &Path) -> Preflight {
    let cards = card_nodes(dri_root);
    if cards.is_empty() {
        return Preflight::fail(format!(
            "host not prepared for console mode: no display device is visible to this agent; \
             {PREPARE_HOST_HINT}"
        ));
    }
    let mut unprepared: Option<String> = None;
    for card in &cards {
        match probe.probe(card) {
            Ok(DisplayState::Free) => return Preflight::ok(),
            Ok(DisplayState::Held) => {
                let holder = name_holder(host_run)
                    .unwrap_or_else(|| "another program holds the display".to_string());
                return Preflight::fail(format!("{holder} holds the display"));
            }
            Err(e) => {
                unprepared.get_or_insert_with(|| {
                    format!(
                        "host not prepared for console mode: could not open {} ({e}); \
                         {PREPARE_HOST_HINT}",
                        card.display()
                    )
                });
            }
        }
    }
    Preflight::fail(unprepared.unwrap_or_else(|| {
        format!("host not prepared for console mode: no display device could be opened; {PREPARE_HOST_HINT}")
    }))
}

/// The startup preflight's own finding, for `readiness::console::check_display` — the
/// readiness check reads this cache rather than re-probing: re-opening a card node for
/// master outside of startup would race a live `spawn_weston_console` for exactly the
/// reason `session::console::drm_open_lock` exists to prevent.
static LAST: std::sync::RwLock<Option<Preflight>> = std::sync::RwLock::new(None);

/// A display that can be taken still fails when the console VT cannot
/// (`session::console_vt`): a console session would refuse to start.
fn with_console_vt(display: Preflight, vt: Result<(), String>) -> Preflight {
    match vt {
        Err(why) if display.ok => Preflight::fail(format!("console terminal: {why}")),
        _ => display,
    }
}

/// Blocking; run on `spawn_blocking` from the caller (file I/O, DRM and VT ioctls).
pub(crate) fn run() -> Preflight {
    // First: it also puts back a console an earlier agent left on the console VT.
    let vt = crate::session::console_vt::reconcile_at_startup();
    let result = with_console_vt(
        run_with(
            &RealDisplayProbe,
            Path::new("/dev/dri"),
            Path::new("/host/run"),
        ),
        vt,
    );
    if let Ok(mut slot) = LAST.write() {
        *slot = Some(result.clone());
    }
    result
}

/// The most recent preflight's result; `None` before the first one has run (or on an
/// agent that was never created with console access, which never runs one at all).
pub(crate) fn last() -> Option<Preflight> {
    LAST.read().ok().and_then(|g| g.clone())
}

/// Tell the recovery actor. Best-effort: an unreachable actor is logged, not fatal — the
/// actor's own verification deadline is the backstop against a silently wedged attempt.
pub(crate) fn post(socket: Option<&Path>, result: &Preflight) {
    let Some(socket) = socket else { return };
    let body = serde_json::to_string(result).unwrap_or_else(|_| "{\"ok\":false}".to_string());
    match crate::release::unix_http::request(
        socket,
        "POST",
        "/v1/console/preflight",
        Some(&body),
        Duration::from_secs(10),
    ) {
        Ok(r) if r.status == 200 => {}
        Ok(r) => warn!(
            token = "console-preflight-post-refused",
            status = r.status,
            "the recovery actor refused this agent's console preflight report: {}",
            r.body
        ),
        Err(e) => warn!(
            token = "console-preflight-post-unreachable",
            "could not tell the recovery actor about this agent's console preflight: {e}"
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    struct FakeRoot {
        dir: PathBuf,
    }

    impl FakeRoot {
        fn new(name: &str) -> Self {
            let dir = std::env::temp_dir().join(format!(
                "quasar-console-preflight-{name}-{}-{}",
                std::process::id(),
                COUNTER.fetch_add(1, Ordering::Relaxed)
            ));
            std::fs::create_dir_all(&dir).unwrap();
            FakeRoot { dir }
        }

        fn card(&self, name: &str) -> &Self {
            std::fs::create_dir_all(&self.dir).unwrap();
            std::fs::write(self.dir.join(name), b"").unwrap();
            self
        }

        fn seat_active(&self, session_id: &str) -> &Self {
            let seats = self.dir.join("systemd/seats");
            std::fs::create_dir_all(&seats).unwrap();
            std::fs::write(seats.join("seat0"), format!("ACTIVE={session_id}\n")).unwrap();
            self
        }

        fn session(&self, id: &str, body: &str) -> &Self {
            let sessions = self.dir.join("systemd/sessions");
            std::fs::create_dir_all(&sessions).unwrap();
            std::fs::write(sessions.join(id), body).unwrap();
            self
        }
    }

    static COUNTER: AtomicUsize = AtomicUsize::new(0);

    impl Drop for FakeRoot {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    struct Scripted(std::collections::HashMap<PathBuf, io::Result<DisplayState>>);

    impl DisplayProbe for Scripted {
        fn probe(&self, path: &Path) -> io::Result<DisplayState> {
            match self.0.get(path) {
                Some(Ok(s)) => Ok(*s),
                Some(Err(e)) => Err(io::Error::new(e.kind(), e.to_string())),
                None => panic!("unscripted probe of {}", path.display()),
            }
        }
    }

    fn scripted(pairs: &[(&Path, io::Result<DisplayState>)]) -> Scripted {
        Scripted(
            pairs
                .iter()
                .map(|(p, r)| {
                    (
                        p.to_path_buf(),
                        match r {
                            Ok(s) => Ok(*s),
                            Err(e) => Err(io::Error::new(e.kind(), e.to_string())),
                        },
                    )
                })
                .collect(),
        )
    }

    #[test]
    fn an_untakeable_console_terminal_fails_an_otherwise_good_preflight() {
        let failed = with_console_vt(Preflight::ok(), Err("tty8 is busy".into()));
        assert!(!failed.ok);
        assert_eq!(
            failed.detail.as_deref(),
            Some("console terminal: tty8 is busy")
        );
        assert_eq!(with_console_vt(Preflight::ok(), Ok(())), Preflight::ok());
        let display = Preflight::fail("someone holds the display".into());
        assert_eq!(
            with_console_vt(display.clone(), Err("tty8 is busy".into())),
            display
        );
    }

    #[test]
    fn no_card_node_at_all_is_unprepared() {
        let root = FakeRoot::new("no-card");
        let probe = scripted(&[]);
        let result = run_with(&probe, &root.dir, &root.dir);
        assert!(!result.ok);
        assert!(
            result.detail.as_deref().unwrap().contains("not prepared"),
            "{result:?}"
        );
    }

    #[test]
    fn a_free_display_passes() {
        let root = FakeRoot::new("free");
        root.card("card0");
        let probe = scripted(&[(&root.dir.join("card0"), Ok(DisplayState::Free))]);
        let result = run_with(&probe, &root.dir, &root.dir);
        assert_eq!(
            result,
            Preflight {
                ok: true,
                detail: None
            }
        );
    }

    #[test]
    fn an_unopenable_card_fails_named_unprepared() {
        let root = FakeRoot::new("unprepared");
        root.card("card0");
        let probe = scripted(&[(
            &root.dir.join("card0"),
            Err(io::Error::from(io::ErrorKind::PermissionDenied)),
        )]);
        let result = run_with(&probe, &root.dir, &root.dir);
        assert!(!result.ok);
        let detail = result.detail.unwrap();
        assert!(detail.contains("not prepared"), "{detail}");
        assert!(detail.contains("prepare-host.sh --console"), "{detail}");
    }

    #[test]
    fn a_held_display_with_no_logind_state_names_nobody_in_particular() {
        let root = FakeRoot::new("held-unnamed");
        root.card("card0");
        let probe = scripted(&[(&root.dir.join("card0"), Ok(DisplayState::Held))]);
        let result = run_with(&probe, &root.dir, &root.dir);
        assert!(!result.ok);
        assert_eq!(
            result.detail.as_deref(),
            Some("another program holds the display holds the display")
        );
    }

    #[test]
    fn a_held_display_names_a_greeter_from_logind() {
        let root = FakeRoot::new("held-greeter");
        root.card("card0");
        root.seat_active("c1");
        root.session("c1", "CLASS=greeter\nSERVICE=gdm.service\n");
        let probe = scripted(&[(&root.dir.join("card0"), Ok(DisplayState::Held))]);
        let result = run_with(&probe, &root.dir, &root.dir);
        assert!(!result.ok);
        assert_eq!(
            result.detail.as_deref(),
            Some("gdm, the login screen holds the display")
        );
    }

    #[test]
    fn a_held_display_names_a_user_desktop_from_logind() {
        let root = FakeRoot::new("held-user");
        root.card("card0");
        root.seat_active("c2");
        root.session("c2", "CLASS=user\nUSER=alice\nDESKTOP=GNOME\n");
        let probe = scripted(&[(&root.dir.join("card0"), Ok(DisplayState::Held))]);
        let result = run_with(&probe, &root.dir, &root.dir);
        assert_eq!(
            result.detail.as_deref(),
            Some("alice's GNOME desktop holds the display")
        );
    }

    #[test]
    fn a_second_card_is_tried_after_the_first_is_unopenable() {
        let root = FakeRoot::new("second-card");
        root.card("card0");
        root.card("card1");
        let probe = scripted(&[
            (
                &root.dir.join("card0"),
                Err(io::Error::from(io::ErrorKind::PermissionDenied)),
            ),
            (&root.dir.join("card1"), Ok(DisplayState::Free)),
        ]);
        let result = run_with(&probe, &root.dir, &root.dir);
        assert!(result.ok, "{result:?}");
    }

    #[test]
    fn detail_is_truncated_to_one_line_within_the_byte_cap() {
        let long = "x".repeat(2000);
        let out = one_line(format!("a\nb\r\n{long}"));
        assert!(!out.contains('\n') && !out.contains('\r'));
        assert!(out.len() <= MAX_DETAIL_BYTES);
    }

    #[test]
    fn card_nodes_excludes_render_nodes_and_sorts() {
        let root = FakeRoot::new("nodes");
        root.card("card1");
        root.card("card0");
        root.card("renderD128");
        let found = card_nodes(&root.dir);
        assert_eq!(found, vec![root.dir.join("card0"), root.dir.join("card1")]);
    }
}
