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
use quasar_recovery::{identity, operator, server, shutdown, uninstall};
use tracing::{error, info};

const USAGE: &str = "usage: quasar-recovery <command>

commands:
  seed      keep this machine's recovery actor in existence: create it on a first install
            (inputs: docs/configuration.md \"Seed\"), re-create it if it is deleted
  actor     run the recovery actor: install or complete this machine's services, then
            serve its sockets (docs/configuration.md \"Recovery actor\")
  status    print this machine's inventory, as the running actor serves it (in the seed's
            container: what the seed last did)
  uninstall [--purge [--confirm <node name>] [--dump-to <host dir>]]
            remove this machine's Quasar services, in its own container (docs/configuration.md);
            keeps the database, machine state and homes unless --purge
  reconfigure [--dry-run] [--yes] VARIABLE=value...
            change a GPU host's inputs (home root, release trust, app defaults, ...) through
            a verified replacement; run it inside the recovery actor (docker exec). A change
            that moves the control plane's container is refused in this build
  version   print this build's version and commit

restore is not in this build.";

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
        Some("uninstall") => uninstall(&args[1..]),
        Some("reconfigure") => reconfigure(&args[1..]),
        Some("restore") => {
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

fn seed_status(body: &str) -> ExitCode {
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let (line, healthy) = seed::status_report(body, now);
    println!("{line}");
    if healthy {
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
    // A journal this process cannot write, or an attempt it can no longer drive, is
    // settled by the next start (D8): exit so the restart policy provides one.
    config.on_died = Box::new(|| {
        error!(
            token = "actor-exiting-to-settle",
            "this recovery actor can no longer drive its attempt; exiting so the next start settles it"
        );
        std::process::exit(1);
    });
    let actor =
        Arc::new(Actor::new(Arc::new(engine), config).with_status_engine(Arc::new(status_engine)));

    // The lease first, then the sockets, then `resume`: settling an interrupted attempt can
    // take a whole verification, and the agent must be able to read its status meanwhile.
    // During a hand-over the lease is held by the other actor, and this one waits.
    if let Err(e) = actor.acquire_lease_waiting() {
        error!(token = "actor-lease-unavailable", "{e}");
        return ExitCode::FAILURE;
    }
    unbound(actor.serve());
    serve_operator(&actor);

    match actor.resume() {
        Ok(()) if actor.retired() => {}
        Ok(()) => info!("this machine's services are installed and running"),
        Err(e) => error!(
            token = "actor-resume-failed",
            "{e}; the install is retried on the next start, and status keeps being served"
        ),
    }
    if !actor.retired() {
        // A first install learns its role from the seed's inputs; bind what it needs.
        unbound(actor.serve());
        match actor.trust() {
            Ok(t) => info!(
                namespaces = ?t.allowed_namespaces,
                signature_mode = t.signature.mode.as_str(),
                "release trust in force"
            ),
            Err(why) => error!(token = "actor-trust-recorded-invalid", "{why}"),
        }
        if !actor.serving() {
            return ExitCode::FAILURE;
        }
    }

    // A hand-over stops this process's sockets on purpose while it waits to be stopped or
    // to take the machine back; only a socket that stopped on its own ends the process.
    loop {
        if actor.retired() {
            info!(
                token = "actor-retired",
                "this recovery actor handed the machine over and exits"
            );
            return ExitCode::SUCCESS;
        }
        if let Some((socket, e)) = actor.serving_failed() {
            error!(
                token = "actor-socket-failed",
                "the socket {} stopped: {e}",
                socket.display()
            );
            return ExitCode::FAILURE;
        }
        std::thread::sleep(Duration::from_millis(500));
    }
}

/// `resume` still runs whatever this says: an interrupted attempt settles and an install
/// completes whether or not anyone can ask about it.
fn unbound(failed: Vec<(PathBuf, std::io::Error)>) {
    for (socket, e) in failed {
        error!(
            token = "actor-socket-bind-failed",
            "cannot create the socket {}: {e} (is the {} volume mounted at {}?)",
            socket.display(),
            quasar_recovery::recipe::names::AGENT_SOCKET_VOLUME,
            paths::AGENT_SOCKET_DIR
        );
    }
}

/// The operator socket (`quasar_recovery::operator`), inside this container only. Not
/// fatal: without it only `reconfigure` is unavailable.
fn serve_operator(actor: &Arc<Actor>) {
    let path = std::path::Path::new(operator::SOCKET);
    match server::bind(path) {
        Ok(listener) => {
            let actor = actor.clone();
            std::thread::spawn(move || {
                let e = operator::serve(listener, actor);
                error!(token = "actor-operator-socket-failed", "the operator socket stopped: {e}");
            });
        }
        Err(e) => error!(
            token = "actor-operator-socket-unbound",
            "cannot create the operator socket {}: {e}; reconfigure is unavailable until the actor restarts",
            path.display()
        ),
    }
}

fn flag_value(args: &[String], name: &str) -> Result<Option<String>, String> {
    match args.iter().position(|a| a == name) {
        None => Ok(args
            .iter()
            .find_map(|a| a.strip_prefix(&format!("{name}=")).map(str::to_owned))),
        Some(i) => args
            .get(i + 1)
            .filter(|v| !v.starts_with("--"))
            .cloned()
            .map(Some)
            .ok_or_else(|| format!("{name} needs a value")),
    }
}

fn uninstall(args: &[String]) -> ExitCode {
    init_logging();
    let known = ["--purge", "--confirm", "--dump-to"];
    let mut i = 0;
    while i < args.len() {
        let a = args[i].split('=').next().unwrap_or("");
        if !known.contains(&a) {
            eprintln!(
                "quasar-recovery uninstall: unknown argument {:?}\n\n{USAGE}",
                args[i]
            );
            return ExitCode::from(2);
        }
        i += if (a == "--confirm" || a == "--dump-to") && !args[i].contains('=') {
            2
        } else {
            1
        };
    }
    let (confirm, dump_to) = match (flag_value(args, "--confirm"), flag_value(args, "--dump-to")) {
        (Ok(c), Ok(d)) => (c, d),
        (Err(e), _) | (_, Err(e)) => {
            eprintln!("quasar-recovery uninstall: {e}");
            return ExitCode::from(2);
        }
    };
    let purge = args.iter().any(|a| a == "--purge");
    if !purge && (confirm.is_some() || dump_to.is_some()) {
        eprintln!("quasar-recovery uninstall: --confirm and --dump-to belong to --purge; without it nothing is deleted or dumped\n\n{USAGE}");
        return ExitCode::from(2);
    }
    let opts = uninstall::Options {
        purge,
        confirm,
        dump_to,
    };
    let engine = match DockerEngine::from_environment() {
        Ok(engine) => engine,
        Err(e) => {
            eprintln!("quasar-recovery uninstall: the container engine endpoint is unusable: {e}");
            return ExitCode::FAILURE;
        }
    };
    let dir = env("QUASAR_MACHINE_DIR").unwrap_or_else(|| paths::MACHINE_DIR.into());
    let mut run = uninstall::Uninstall::new(Arc::new(engine), dir);
    run.self_container = quasar_runtime::self_inspection::self_container_id();
    if std::io::IsTerminal::is_terminal(&std::io::stdin()) {
        run.prompt = Some(Box::new(|question: &str| {
            eprint!("{question}");
            let mut line = String::new();
            std::io::stdin().read_line(&mut line).ok()?;
            Some(line.trim().to_owned())
        }));
    }
    match run.run(&opts) {
        Ok(report) => {
            for line in report.lines {
                println!("{line}");
            }
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("quasar-recovery uninstall: {e}");
            ExitCode::FAILURE
        }
    }
}

fn reconfigure(args: &[String]) -> ExitCode {
    use quasar_recovery::reconfigure::ReconfigureRequest;
    let mut changes = std::collections::BTreeMap::new();
    let (mut yes, mut dry_run) = (false, false);
    for a in args {
        match a.as_str() {
            "--yes" => yes = true,
            "--dry-run" => dry_run = true,
            kv => match kv.split_once('=') {
                Some((k, v)) if k.starts_with("QUASAR_") => {
                    changes.insert(k.to_owned(), v.to_owned());
                }
                _ => {
                    eprintln!("quasar-recovery reconfigure: expected VARIABLE=value, got {kv:?}\n\n{USAGE}");
                    return ExitCode::from(2);
                }
            },
        }
    }
    let socket = std::path::Path::new(operator::SOCKET);
    if !socket.exists() {
        eprintln!(
            "quasar-recovery reconfigure: no operator socket at {}; run it inside the recovery actor: docker exec -it quasar-recovery quasar-recovery reconfigure VARIABLE=value",
            socket.display()
        );
        return ExitCode::FAILURE;
    }
    let ask = |dry_run: bool| {
        operator::reconfigure(
            socket,
            &ReconfigureRequest {
                changes: changes.clone(),
                dry_run,
            },
        )
    };
    let plan = match ask(true) {
        Ok(operator::Answer::Planned(p)) => p,
        Ok(operator::Answer::Refused(r)) => {
            eprintln!(
                "quasar-recovery reconfigure: refused ({}): {}",
                r.reason, r.message
            );
            return ExitCode::FAILURE;
        }
        Err(e) => {
            eprintln!("quasar-recovery reconfigure: the recovery actor did not answer: {e}");
            return ExitCode::FAILURE;
        }
    };
    if plan.changed.is_empty() {
        println!("Nothing to change: every value is already in force.");
        return ExitCode::SUCCESS;
    }
    println!("Changes: {}", plan.changed.join(", "));
    if plan.replaced.is_empty() {
        println!("No service needs re-creating.");
    } else {
        println!(
            "Re-creates, on the image it already runs: {}{}",
            plan.replaced.join(", "),
            if plan.replaced.iter().any(|r| r == "node-agent") {
                " (re-creating the node agent ends this host's sessions)"
            } else {
                ""
            }
        );
    }
    if dry_run {
        return ExitCode::SUCCESS;
    }
    if !plan.replaced.is_empty() && !yes {
        println!(
            "Nothing was changed. Drain the host if it has sessions, then run again with --yes."
        );
        return ExitCode::from(3);
    }
    let done = match ask(false) {
        Ok(operator::Answer::Planned(p)) => p,
        Ok(operator::Answer::Refused(r)) => {
            eprintln!(
                "quasar-recovery reconfigure: refused ({}): {}",
                r.reason, r.message
            );
            return ExitCode::FAILURE;
        }
        Err(e) => {
            eprintln!("quasar-recovery reconfigure: the recovery actor did not answer: {e}");
            return ExitCode::FAILURE;
        }
    };
    let Some(id) = done.request_id else {
        println!("Reconfigured: the new inputs are in force.");
        return ExitCode::SUCCESS;
    };
    println!("Replacing (attempt {id})…");
    let mut last = String::new();
    let deadline = std::time::Instant::now() + Duration::from_secs(30 * 60);
    loop {
        std::thread::sleep(Duration::from_secs(2));
        let status = operator::call(socket, "GET", &format!("/v1/status?request_id={id}"), None)
            .ok()
            .and_then(|(code, body)| (code == 200).then_some(body))
            .and_then(|body| serde_json::from_str::<quasar_recovery::socket::Status>(&body).ok());
        let Some(result) = status.and_then(|s| s.result) else {
            if std::time::Instant::now() > deadline {
                eprintln!("quasar-recovery reconfigure: lost sight of attempt {id}; `quasar-recovery status` shows its outcome");
                return ExitCode::FAILURE;
            }
            continue;
        };
        let state = format!("{:?}", result.state).to_lowercase();
        if state != last {
            println!("  {state}");
            last = state;
        }
        match result.state {
            quasar_recovery::socket::State::Succeeded => {
                println!("Reconfigured: the new inputs are in force.");
                return ExitCode::SUCCESS;
            }
            quasar_recovery::socket::State::Failed => {
                eprintln!(
                    "The reconfigure failed ({}){}; the previous inputs are back in force.\n{}",
                    result.reason.map(|r| r.to_string()).unwrap_or_default(),
                    if result.restored {
                        " and the previous container was put back"
                    } else {
                        ""
                    },
                    result.output
                );
                return ExitCode::FAILURE;
            }
            _ => {}
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
        // `Actor::trust`'s rule: recorded settings win, and unreadable state refuses.
        let recorded = match quasar_recovery::machine::MachineDir::new(&machine_dir).load_machine()
        {
            Ok(m) => m.map(|m| m.inputs.trust).filter(|t| !t.is_empty()),
            Err(e) => {
                return SignatureEvidence::FetchError {
                    error: format!("machine state is unreadable: {e}"),
                }
            }
        };
        let (base, timeout) = match recorded {
            Some(t) => {
                match trust::parse_manifest_base_url(t.manifest_base_url.as_deref().unwrap_or("")) {
                    Ok(recorded_base) => (
                        recorded_base,
                        trust::parse_manifest_timeout(
                            t.manifest_timeout_s.as_deref().unwrap_or(""),
                        ),
                    ),
                    Err(e) => {
                        return SignatureEvidence::FetchError {
                            error: format!(
                                "QUASAR_UPDATER_MANIFEST_BASE_URL recorded in machine state: {e}"
                            ),
                        }
                    }
                }
            }
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
