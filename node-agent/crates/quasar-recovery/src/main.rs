//! `quasar-recovery`: the recovery actor and the seed, one binary. Their inputs are
//! environment variables so a manager's stack or a single `docker run -e …` can supply
//! them; `docs/configuration.md` "Seed" and "Recovery actor" list them.

use std::path::PathBuf;
use std::process::ExitCode;
use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::actor::{Actor, ActorConfig, TrustConfig};
use quasar_recovery::bootstrap::Bootstrap;
use quasar_recovery::engine::DockerEngine;
use quasar_recovery::recipe::paths;
use quasar_recovery::seed::{self, profile, Seed, SeedConfig};
use quasar_recovery::socket::Request;
use quasar_recovery::trust::{self, SignatureEvidence};
use quasar_recovery::{identity, server, shutdown};
use tracing::{error, info};

const USAGE: &str = "usage: quasar-recovery <command>

commands:
  seed      keep this machine's recovery actor in existence: create it on a first install
            (inputs: docs/configuration.md \"Seed\"), re-create it if it is deleted
  actor     run the recovery actor: install or complete this machine's services, then
            serve its sockets (docs/configuration.md \"Recovery actor\")
  status    print this machine's inventory, as the running actor serves it (in the seed's
            container: what the seed last did)
  version   print this build's version and commit

restore, uninstall and reconfigure are not in this build.";

/// In the seed's own container only: what its last look came to, for the health check.
const SEED_STATUS_FILE: &str = "/tmp/quasar-seed.status";

/// A status answer must come back well inside the agent's own socket timeout.
const STATUS_ENGINE_DEADLINE: Duration = Duration::from_secs(3);

fn env(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.trim().is_empty())
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        // Dispatched before anything the actor initialises, so no actor start-up path can
        // reach the seed.
        Some("seed") => seed_mode(),
        Some("actor") => actor(),
        Some("status") => status(),
        Some("version") | Some("--version") => {
            println!(
                "quasar-recovery {} ({})",
                identity::version(),
                identity::source_commit()
            );
            ExitCode::SUCCESS
        }
        Some("restore") | Some("uninstall") | Some("reconfigure") => {
            eprintln!(
                "quasar-recovery: `{}` is not in this build\n\n{USAGE}",
                args[0]
            );
            ExitCode::from(2)
        }
        Some("help") | Some("-h") | Some("--help") => {
            println!("{USAGE}");
            ExitCode::SUCCESS
        }
        _ => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}

/// Every socket an actor may serve, in this container. Fixed, not configurable: the recipes
/// name the same paths, and an actor listening anywhere else would never be reached.
const SOCKETS: &[&str] = &[
    paths::AGENT_SOCKET,
    paths::SPLIT_AGENT_SOCKET,
    paths::CONTROL_SOCKET,
];

/// The inventory as the running actor serves it on the first of its sockets that answers,
/// then (on stderr) what is not as this machine's role needs it.
fn status() -> ExitCode {
    if let Ok(body) = std::fs::read_to_string(SEED_STATUS_FILE) {
        return seed_status(&body);
    }
    let mut last = None;
    for socket in SOCKETS {
        if !std::path::Path::new(socket).exists() {
            continue;
        }
        match server::fetch_status(std::path::Path::new(socket)) {
            Ok(body) => {
                println!("{body}");
                explain(&body);
                return ExitCode::SUCCESS;
            }
            Err(e) => last = Some(format!("{socket}: {e}")),
        }
    }
    eprintln!(
        "quasar-recovery status: {}",
        last.unwrap_or_else(|| "no recovery-actor socket in this container".into())
    );
    ExitCode::FAILURE
}

fn explain(body: &str) {
    let Ok(status) = serde_json::from_str::<quasar_recovery::socket::Status>(body) else {
        return;
    };
    let dir = env("QUASAR_MACHINE_DIR").unwrap_or_else(|| paths::MACHINE_DIR.into());
    let machine = quasar_recovery::machine::MachineDir::new(dir)
        .load_machine()
        .ok()
        .flatten();
    for line in quasar_recovery::explain::explain(&status, machine.as_ref()) {
        eprintln!("{line}");
    }
}

/// Healthy while the seed keeps looking: its last look is at most three intervals old.
fn seed_status(body: &str) -> ExitCode {
    let mut lines = body.lines();
    let at: u64 = lines
        .next()
        .and_then(|l| l.trim().parse().ok())
        .unwrap_or(0);
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let age = now.saturating_sub(at);
    println!(
        "seed: {} ({age} s ago)",
        lines.next().unwrap_or("no look yet")
    );
    if age <= 3 * seed::INTERVAL.as_secs() {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

fn init_logging() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .with_writer(std::io::stderr)
        .init();
}

fn install_signals(what: &'static str) -> Result<(), ExitCode> {
    shutdown::install(what).map_err(|e| {
        error!(
            token = "signals-unavailable",
            "cannot install the SIGTERM handler: {e}"
        );
        ExitCode::FAILURE
    })
}

fn seed_mode() -> ExitCode {
    init_logging();
    if let Err(code) = install_signals("seed") {
        return code;
    }
    info!(
        version = identity::version(),
        commit = identity::source_commit(),
        "seed starting"
    );
    let engine = match DockerEngine::from_environment() {
        Ok(engine) => engine,
        Err(e) => {
            error!(
                token = "seed-engine-config",
                "the container engine endpoint is unusable: {e}"
            );
            return ExitCode::FAILURE;
        }
    };
    info!(engine = %engine.endpoint(), "engine");
    let mut config =
        SeedConfig::new(env("QUASAR_MACHINE_DIR").unwrap_or_else(|| profile::MACHINE_DIR.into()));
    config.self_container = quasar_runtime::self_inspection::self_container_id();
    config.status_file = Some(SEED_STATUS_FILE.into());
    Seed::new(Arc::new(engine), config).run()
}

fn actor() -> ExitCode {
    init_logging();
    if let Err(code) = install_signals("recovery actor") {
        return code;
    }
    info!(
        version = identity::version(),
        commit = identity::source_commit(),
        "recovery actor starting"
    );

    // A seed-created actor reads its install inputs from the seed's container instead.
    let Bootstrap { role, operator } = match Bootstrap::from_process_env() {
        Ok(boot) => boot,
        Err(why) => {
            error!(token = "actor-role-invalid", "{why}");
            return ExitCode::from(2);
        }
    };
    let machine_dir = env("QUASAR_MACHINE_DIR").unwrap_or_else(|| paths::MACHINE_DIR.into());
    let mut config = ActorConfig::new(machine_dir.clone(), role, operator);
    config.self_container = quasar_runtime::self_inspection::self_container_id();
    config.seed_container = env(profile::SEED_CONTAINER_ENV);
    if let Some(fallback) = env("QUASAR_DOCKER_SOCKET_HOST_PATH") {
        config.docker_socket_fallback = fallback;
    }

    let engines = DockerEngine::from_environment().and_then(|engine| {
        let mut short = quasar_runtime::RuntimeConfig::from_environment()?;
        short.deadline = STATUS_ENGINE_DEADLINE;
        Ok((engine, DockerEngine::new(short)?))
    });
    let (engine, status_engine) = match engines {
        Ok(engines) => engines,
        Err(e) => {
            error!(
                token = "actor-engine-config",
                "the container engine endpoint is unusable: {e}"
            );
            return ExitCode::FAILURE;
        }
    };
    info!(engine = %engine.endpoint(), "engine");
    match trust_from_env(machine_dir.clone()) {
        Ok((trust, evidence)) => {
            config.trust = trust;
            config.evidence = evidence;
        }
        Err(why) => {
            error!(token = "actor-trust-config-invalid", "{why}");
            return ExitCode::from(2);
        }
    }
    let actor =
        Arc::new(Actor::new(Arc::new(engine), config).with_status_engine(Arc::new(status_engine)));

    // The lease first, then the sockets, then `resume`: settling an interrupted attempt can
    // take a whole verification, and the agent must be able to read its status meanwhile.
    if let Err(e) = actor.acquire_lease() {
        error!(token = "actor-lease-unavailable", "{e}");
        return ExitCode::FAILURE;
    }
    let (stopped, servers) = std::sync::mpsc::channel::<std::io::Error>();
    let mut serving: Vec<PathBuf> = Vec::new();
    serve_planned(&actor, &mut serving, &stopped);

    // The lease is already held, so `resume` cannot answer `LeaseHeld`.
    match actor.resume() {
        Ok(()) => info!("this machine's services are installed and running"),
        Err(e) => error!(
            token = "actor-resume-failed",
            "{e}; the install is retried on the next start, and status keeps being served"
        ),
    }
    // A first install learns its role from the seed's inputs; bind what it needs.
    serve_planned(&actor, &mut serving, &stopped);
    match actor.trust() {
        Ok(t) => info!(
            namespaces = ?t.allowed_namespaces,
            signature_mode = t.signature.mode.as_str(),
            "release trust in force"
        ),
        Err(why) => error!(token = "actor-trust-recorded-invalid", "{why}"),
    }

    if serving.is_empty() {
        return ExitCode::FAILURE;
    }
    drop(stopped);
    let e = servers
        .recv()
        .unwrap_or_else(|_| std::io::Error::other("every socket thread ended"));
    error!(token = "actor-socket-failed", "a socket stopped: {e}");
    ExitCode::FAILURE
}

/// Binds and serves every socket of [`Actor::socket_plan`] not already served.
fn serve_planned(
    actor: &Arc<Actor>,
    serving: &mut Vec<PathBuf>,
    stopped: &std::sync::mpsc::Sender<std::io::Error>,
) {
    for plan in actor.socket_plan() {
        if serving.contains(&plan.path) {
            continue;
        }
        match server::bind_owned(&plan.path, plan.owner) {
            Ok(listener) => {
                info!(socket = %plan.path.display(), caller = ?plan.caller, "serving");
                let (actor, stopped, caller) = (actor.clone(), stopped.clone(), plan.caller);
                std::thread::spawn(move || {
                    let _ = stopped.send(server::serve(listener, actor, caller));
                });
                serving.push(plan.path);
            }
            // `resume` still runs: an interrupted attempt settles and an install completes
            // whether or not anyone can ask about it.
            Err(e) => error!(
                token = "actor-socket-bind-failed",
                "cannot create the socket {}: {e} (is the {} volume mounted at {}?)",
                plan.path.display(),
                quasar_recovery::recipe::names::AGENT_SOCKET_VOLUME,
                paths::AGENT_SOCKET_DIR
            ),
        }
    }
}

type Evidence = Box<dyn Fn(&Request) -> SignatureEvidence + Send + Sync>;

/// The updater's trust knobs, read the way the Go updater reads them
/// (`docs/configuration.md` "Recovery actor").
/// This start's own settings, which apply only until machine state records the seed's at
/// install (`Actor::trust`); the fetch's base URL and timeout follow the same rule.
fn trust_from_env(machine_dir: String) -> Result<(TrustConfig, Evidence), String> {
    let raw = |k: &str| std::env::var(k).unwrap_or_default();
    let allowed_namespaces =
        trust::parse_allowed_namespaces(&raw("QUASAR_UPDATER_ALLOWED_NAMESPACES"));
    let mode = trust::parse_signature_mode(&raw("QUASAR_UPDATER_SIGNATURE_MODE"))
        .map_err(|e| format!("QUASAR_UPDATER_SIGNATURE_MODE: {e}"))?;
    let keys = trust::parse_trusted_keys(&raw("QUASAR_UPDATER_TRUSTED_KEYS"))
        .map_err(|e| format!("QUASAR_UPDATER_TRUSTED_KEYS: {e}"))?;
    let base = trust::parse_manifest_base_url(&raw("QUASAR_UPDATER_MANIFEST_BASE_URL"))
        .map_err(|e| format!("QUASAR_UPDATER_MANIFEST_BASE_URL: {e}"))?;
    let timeout = trust::parse_manifest_timeout(&raw("QUASAR_UPDATER_MANIFEST_TIMEOUT_S"));
    info!(namespaces = ?allowed_namespaces, signature_mode = mode.as_str(), "release trust");
    let policy = trust::SignaturePolicy { mode, keys };
    let fetcher = Arc::new(trust::HttpsFetcher::from_env());
    let evidence: Evidence = Box::new(move |req: &Request| {
        let runtime = match tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
        {
            Ok(rt) => rt,
            Err(e) => {
                return SignatureEvidence::FetchError {
                    error: format!("no runtime to fetch the release assets on: {e}"),
                }
            }
        };
        let recorded = quasar_recovery::machine::MachineDir::new(&machine_dir)
            .load_machine()
            .ok()
            .flatten()
            .map(|m| m.inputs.trust)
            .filter(|t| !t.is_empty());
        let (base, timeout) = match recorded {
            Some(t) => (
                trust::parse_manifest_base_url(t.manifest_base_url.as_deref().unwrap_or(""))
                    .unwrap_or_else(|_| base.clone()),
                trust::parse_manifest_timeout(t.manifest_timeout_s.as_deref().unwrap_or("")),
            ),
            None => (base.clone(), timeout),
        };
        runtime.block_on(fetcher.evidence(&base, req.release.version.as_deref(), timeout))
    });
    Ok((
        TrustConfig {
            allowed_namespaces,
            signature: policy,
        },
        evidence,
    ))
}
