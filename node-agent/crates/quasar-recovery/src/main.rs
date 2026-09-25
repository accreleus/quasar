//! `quasar-recovery`: the recovery actor binary. Its inputs are environment variables so a
//! manager's stack or a single `docker run -e …` can supply them; `docs/configuration.md`
//! "Recovery actor" lists them.

use std::path::PathBuf;
use std::process::ExitCode;
use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::actor::{Actor, ActorConfig, OperatorInputs, ResumeError};
use quasar_recovery::engine::DockerEngine;
use quasar_recovery::recipe::paths;
use quasar_recovery::socket::MachineRole;
use quasar_recovery::{identity, server};
use tracing::{error, info};

const USAGE: &str = "usage: quasar-recovery <command>

commands:
  actor     run the recovery actor: install or complete this machine's services, then
            serve the agent socket (inputs: docs/configuration.md \"Recovery actor\")
  status    print this machine's inventory, as the running actor serves it
  version   print this build's version and commit

seed, restore, uninstall and reconfigure are not in this build.";

/// A status answer must come back well inside the agent's own socket timeout.
const STATUS_ENGINE_DEADLINE: Duration = Duration::from_secs(3);

fn env(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.trim().is_empty())
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
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
        Some("seed") | Some("restore") | Some("uninstall") | Some("reconfigure") => {
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

/// Fixed, not configurable: the agent's recipe names the same path (`QUASAR_RECOVERY_SOCKET`
/// in the agent), and an actor listening anywhere else would never be reached.
fn agent_socket() -> PathBuf {
    paths::AGENT_SOCKET.into()
}

fn status() -> ExitCode {
    match server::fetch_status(&agent_socket()) {
        Ok(body) => {
            println!("{body}");
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("quasar-recovery status: {e}");
            ExitCode::FAILURE
        }
    }
}

/// Only `gpu` installs in this build; the combined and control-only roles arrive with #361.
fn parse_role(raw: &str) -> Result<MachineRole, String> {
    match raw {
        "gpu" => Ok(MachineRole::Gpu),
        "combined" | "control-only" => Err(format!(
            "QUASAR_ROLE={raw} is not installed by this build (combined and control-only machines arrive with RH06-09, #361); use gpu"
        )),
        other => Err(format!("QUASAR_ROLE={other:?} is not a role; this build installs gpu")),
    }
}

fn actor() -> ExitCode {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .with_writer(std::io::stderr)
        .init();
    info!(
        version = identity::version(),
        commit = identity::source_commit(),
        "recovery actor starting"
    );

    let role = match parse_role(&env("QUASAR_ROLE").unwrap_or_else(|| "gpu".into())) {
        Ok(role) => role,
        Err(why) => {
            error!(token = "actor-role-invalid", "{why}");
            return ExitCode::from(2);
        }
    };
    let operator = OperatorInputs {
        enrollment: env("QUASAR_ENROLLMENT"),
        home_root: env("QUASAR_HOME_ROOT"),
        template_root: env("QUASAR_TEMPLATE_ROOT"),
        node_name: env("QUASAR_NODE_NAME"),
        agent_image: env("QUASAR_AGENT_IMAGE"),
    };
    let machine_dir = env("QUASAR_MACHINE_DIR").unwrap_or_else(|| paths::MACHINE_DIR.into());
    let mut config = ActorConfig::new(machine_dir, role, operator);
    config.self_container = quasar_runtime::self_inspection::self_container_id();
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
    let actor =
        Arc::new(Actor::new(Arc::new(engine), config).with_status_engine(Arc::new(status_engine)));

    match actor.resume() {
        Ok(()) => info!("this machine's services are installed and running"),
        Err(e @ (ResumeError::LeaseHeld | ResumeError::State(_))) if !actor.holds_lease() => {
            error!(token = "actor-lease-unavailable", "{e}");
            return ExitCode::FAILURE;
        }
        Err(e) => error!(
            token = "actor-resume-failed",
            "{e}; the install is retried on the next start, and status keeps being served"
        ),
    }

    let socket = agent_socket();
    let listener = match server::bind(&socket) {
        Ok(listener) => listener,
        Err(e) => {
            error!(
                token = "actor-socket-bind-failed",
                "cannot create the agent socket at {}: {e} (is the {} volume mounted at {}?)",
                socket.display(),
                quasar_recovery::recipe::names::AGENT_SOCKET_VOLUME,
                paths::AGENT_SOCKET_DIR
            );
            return ExitCode::FAILURE;
        }
    };
    info!(socket = %socket.display(), "serving the agent socket");
    let e = server::serve(listener, actor);
    error!(
        token = "actor-socket-failed",
        "the agent socket stopped: {e}"
    );
    ExitCode::FAILURE
}
