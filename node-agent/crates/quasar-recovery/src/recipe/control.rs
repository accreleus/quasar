//! The recipes of a combined or control-only machine (#361): Postgres, the control plane,
//! and what a combined host's own node agent adds to the GPU-host shape.
//!
//! What they carry is what `deploy/docker-compose.yml` gives `quasar-postgres` and
//! `quasar-control-plane`, with the owned-install differences listed in
//! `tests/recipe_compose_parity.rs`: secrets arrive as read-only files (D5), the Go
//! updater's volume is replaced by the control socket, and the static enrollment token is
//! replaced by the machine's single-use local token.

use std::collections::BTreeMap;

use super::{
    bind, names, paths, safe_host_path, secrets, ContainerSpec, ControlInputs, DatabaseInputs,
    Healthcheck, ImageRef, Inputs, PublishedPort, RenderError, RestartPolicy, SecretMounts,
    ENROLLMENT_TOKEN_FILE_ENV,
};

/// The Postgres image carries no `org.quasar.recipe` label, so its revision is this actor's.
pub const POSTGRES_REVISION: u32 = 1;

/// The Postgres a combined or control-only machine gets when the operator names none: the
/// `postgres:16-alpine` index as validated for this release. Quasar creates it once and
/// never updates it (#352 R1); `QUASAR_POSTGRES_IMAGE` pins another.
pub const DEFAULT_POSTGRES_IMAGE: &str =
    "docker.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea";

/// The database and role a Quasar-owned Postgres is created with.
pub const OWNED_DATABASE: &str = "quasar";
pub const OWNED_DATABASE_USER: &str = "quasar";

/// The control plane's environment names this recipe sets (`docs/configuration.md`).
pub mod env {
    pub const CONTROL_SOCKET: &str = "QUASAR_RECOVERY_CONTROL_SOCKET";
    pub const DATABASE_PASSWORD_FILE: &str = "QUASAR_DATABASE_PASSWORD_FILE";
    pub const SECRET_KEY_FILE: &str = "QUASAR_SECRET_KEY_FILE";
    pub const LOCAL_ENROLLMENT_FILE: &str = "QUASAR_LOCAL_ENROLLMENT_FILE";
    pub const LOCAL_ENROLLMENT_NODE_NAME: &str = "QUASAR_LOCAL_ENROLLMENT_NODE_NAME";
}

const SSL_MODES: &[&str] = &[
    "disable",
    "allow",
    "prefer",
    "require",
    "verify-ca",
    "verify-full",
];

fn host_like(what: &str, value: &str) -> Result<(), RenderError> {
    let ok = !value.is_empty()
        && value.len() <= 253
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'-' | b':' | b'[' | b']'));
    if ok {
        Ok(())
    } else {
        Err(RenderError::Invalid(format!(
            "{what} {value:?} must be a host name or an address"
        )))
    }
}

fn word(what: &str, value: &str) -> Result<(), RenderError> {
    let ok = !value.is_empty()
        && value.len() <= 63
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-' | b'.'));
    if ok {
        Ok(())
    } else {
        Err(RenderError::Invalid(format!(
            "{what} {value:?} must be letters, digits, `_`, `-` and `.`"
        )))
    }
}

pub(super) fn validate(control: &ControlInputs) -> Result<(), RenderError> {
    if control.http_port == 0 || control.tls_port == 0 || control.http_port == control.tls_port {
        return Err(RenderError::Invalid(format!(
            "the control plane's ports must be two different non-zero ports (got {} and {})",
            control.http_port, control.tls_port
        )));
    }
    if let Some(host) = &control.public_host {
        host_like("the public host", host)?;
    }
    if let Some(hosts) = &control.tls_hosts {
        for host in hosts.split(',').map(str::trim).filter(|h| !h.is_empty()) {
            host_like("a TLS host", host)?;
        }
    }
    if let DatabaseInputs::External {
        host,
        port,
        user,
        name,
        sslmode,
    } = &control.database
    {
        host_like("the database host", host)?;
        if *port == 0 {
            return Err(RenderError::Invalid(
                "the database port must not be 0".into(),
            ));
        }
        word("the database user", user)?;
        word("the database name", name)?;
        if !SSL_MODES.contains(&sslmode.as_str()) {
            return Err(RenderError::Invalid(format!(
                "the database sslmode {sslmode:?} is not one of {}",
                SSL_MODES.join(", ")
            )));
        }
    }
    Ok(())
}

fn control(inputs: &Inputs) -> Result<(&ControlInputs, &str), RenderError> {
    match (&inputs.control, &inputs.socket_dir) {
        (Some(control), Some(dir)) => Ok((control, dir)),
        _ => Err(RenderError::Invalid(
            "this machine runs no control plane (no control inputs)".into(),
        )),
    }
}

fn socket_subdir(dir: &str, sub: &str) -> Result<String, RenderError> {
    let path = format!("{}/{sub}", dir.trim_end_matches('/'));
    safe_host_path("the socket directory", &path)?;
    Ok(path)
}

fn secret_path(name: &str) -> String {
    format!("{}/{name}", paths::SECRETS_DIR)
}

/// Postgres, revision 1: `quasar-postgres` of `deploy/docker-compose.yml`, its password
/// from a file.
pub(super) fn postgres_r1(
    inputs: &Inputs,
    image: &ImageRef,
    secrets: &SecretMounts,
) -> Result<ContainerSpec, RenderError> {
    let (control, _) = control(inputs)?;
    if control.database != DatabaseInputs::Owned {
        return Err(RenderError::Invalid(
            "an external database gets no Quasar-owned Postgres".into(),
        ));
    }
    if !secrets.files.contains(secrets::DATABASE_PASSWORD) {
        return Err(RenderError::Invalid(
            "Postgres needs its password file".into(),
        ));
    }
    let volume = secrets
        .volume
        .clone()
        .unwrap_or_else(|| names::POSTGRES_SECRETS_VOLUME.into());
    Ok(ContainerSpec {
        name: names::POSTGRES.into(),
        image: image.reference(),
        entrypoint: None,
        cmd: None,
        env: BTreeMap::from([
            ("POSTGRES_DB".to_string(), OWNED_DATABASE.to_string()),
            ("POSTGRES_USER".into(), OWNED_DATABASE_USER.into()),
            (
                "POSTGRES_PASSWORD_FILE".into(),
                secret_path(secrets::DATABASE_PASSWORD),
            ),
        ]),
        labels: BTreeMap::new(),
        network_mode: Some(names::PLATFORM_NETWORK.into()),
        binds: vec![
            bind(&volume, paths::SECRETS_DIR, true),
            bind(names::POSTGRES_DATA_VOLUME, paths::POSTGRES_DATA_DIR, false),
        ],
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::UnlessStopped,
        ports: Vec::new(),
        healthcheck: Some(Healthcheck {
            test: vec![
                "CMD-SHELL".into(),
                format!("pg_isready -U {OWNED_DATABASE_USER} -d {OWNED_DATABASE}"),
            ],
            interval_s: 5,
            timeout_s: 3,
            retries: 12,
            start_period_s: 0,
        }),
    })
}

/// The control plane, revision 1: `quasar-control-plane` of `deploy/docker-compose.yml`
/// with its database password and secret key as files, the control socket in place of the
/// updater's volume, and, on a combined host, the local enrollment token.
pub(super) fn control_plane_r1(
    inputs: &Inputs,
    image: &ImageRef,
    secrets: &SecretMounts,
) -> Result<ContainerSpec, RenderError> {
    let (control, socket_dir) = control(inputs)?;
    for needed in [secrets::DATABASE_PASSWORD, secrets::SECRET_KEY] {
        if !secrets.files.contains(needed) {
            return Err(RenderError::Invalid(format!(
                "the control plane needs its {needed} file"
            )));
        }
    }
    let volume = secrets
        .volume
        .clone()
        .unwrap_or_else(|| names::CONTROL_PLANE_SECRETS_VOLUME.into());
    let public_host = control.public_host.clone().unwrap_or_default();
    let (db_host, db_port, db_user, db_name, sslmode) = match &control.database {
        DatabaseInputs::Owned => (
            names::POSTGRES.to_string(),
            5432,
            OWNED_DATABASE_USER.to_string(),
            OWNED_DATABASE.to_string(),
            "disable".to_string(),
        ),
        DatabaseInputs::External {
            host,
            port,
            user,
            name,
            sslmode,
        } => (
            host.clone(),
            *port,
            user.clone(),
            name.clone(),
            sslmode.clone(),
        ),
    };
    let mut env: BTreeMap<String, String> = [
        ("LISTEN_ADDR", ":8080".to_string()),
        ("QUASAR_TLS", "auto".into()),
        ("QUASAR_TLS_ADDR", ":8443".into()),
        // Browser routes on plain HTTP redirect to this external HTTPS port; agent
        // enrollment and /health stay on HTTP.
        ("QUASAR_TLS_REDIRECT_PORT", control.tls_port.to_string()),
        ("QUASAR_HTTP_REDIRECT", "auto".into()),
        (
            "QUASAR_TLS_HOSTS",
            control.tls_hosts.clone().unwrap_or_default(),
        ),
        ("QUASAR_PUBLIC_HOST", public_host),
        ("QUASAR_DATABASE_HOST", db_host),
        ("QUASAR_DATABASE_PORT", db_port.to_string()),
        ("QUASAR_DATABASE_USER", db_user),
        ("QUASAR_DATABASE_NAME", db_name),
        ("QUASAR_DATABASE_SSLMODE", sslmode),
        (
            env::DATABASE_PASSWORD_FILE,
            secret_path(secrets::DATABASE_PASSWORD),
        ),
        (env::SECRET_KEY_FILE, secret_path(secrets::SECRET_KEY)),
        (env::CONTROL_SOCKET, paths::CONTROL_PLANE_SOCKET.into()),
        (
            "QUASAR_UPDATER_ALLOWED_NAMESPACES",
            inputs.trust.allowed_namespaces.clone().unwrap_or_default(),
        ),
        (
            "QUASAR_PLATFORM_INSECURE_REGISTRIES",
            inputs.trust.insecure_registries.clone().unwrap_or_default(),
        ),
        (
            "QUASAR_ENROLL_SEED_IMAGE",
            inputs
                .enroll
                .seed
                .as_ref()
                .map(ImageRef::reference)
                .unwrap_or_default(),
        ),
        (
            "QUASAR_ENROLL_AGENT_IMAGE",
            inputs
                .enroll
                .agent
                .as_ref()
                .map(ImageRef::reference)
                .unwrap_or_default(),
        ),
        ("LOG_LEVEL", "info".into()),
        ("AUTH_TOKEN_TTL", "24h".into()),
        ("QUASAR_PPROF_ADDR", "127.0.0.1:6060".into()),
        ("GOMEMLIMIT", "1GiB".into()),
    ]
    .into_iter()
    .map(|(k, v)| (k.to_string(), v))
    .collect();
    if !inputs.home_root.is_empty() {
        env.insert("QUASAR_HOME_ROOT".into(), inputs.home_root.clone());
    }
    if secrets.files.contains(secrets::LOCAL_ENROLLMENT) {
        env.insert(
            env::LOCAL_ENROLLMENT_FILE.into(),
            secret_path(secrets::LOCAL_ENROLLMENT),
        );
        env.insert(
            env::LOCAL_ENROLLMENT_NODE_NAME.into(),
            inputs.node_name.clone(),
        );
    }
    let mut binds = vec![
        bind(
            &socket_subdir(socket_dir, paths::CONTROL_SOCKET_SUBDIR)?,
            paths::CONTROL_PLANE_SOCKET_DIR,
            true,
        ),
        bind(&volume, paths::SECRETS_DIR, true),
        bind(names::CONTROL_DATA_VOLUME, paths::CONTROL_DATA_DIR, false),
    ];
    binds.sort_by(|a, b| a.target.cmp(&b.target));
    Ok(ContainerSpec {
        name: names::CONTROL_PLANE.into(),
        image: image.reference(),
        entrypoint: None,
        cmd: None,
        env,
        labels: BTreeMap::new(),
        network_mode: Some(names::PLATFORM_NETWORK.into()),
        binds,
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::UnlessStopped,
        ports: vec![
            PublishedPort {
                container_port: 8080,
                host_port: control.http_port,
                host_ip: None,
            },
            PublishedPort {
                container_port: 8443,
                host_port: control.tls_port,
                host_ip: None,
            },
        ],
        // The image's own (`curl /health`, which also reaches the database).
        healthcheck: None,
    })
}

/// A combined host's own agent, from revision 2: it reaches the control plane on this
/// machine's loopback (a published port; plaintext never leaves the host, which the agent
/// allows for loopback only), enrolls with the local token, and is given only the agent
/// subdirectory of the socket volume.
pub(super) fn local_agent(
    spec: &mut ContainerSpec,
    inputs: &Inputs,
    secrets: &SecretMounts,
) -> Result<(), RenderError> {
    let (control, socket_dir) = control(inputs)?;
    spec.env.insert(
        "CONTROL_PLANE_URL".into(),
        format!("ws://127.0.0.1:{}", control.http_port),
    );
    if secrets.files.contains(secrets::LOCAL_ENROLLMENT) {
        spec.env.insert(
            ENROLLMENT_TOKEN_FILE_ENV.into(),
            secret_path(secrets::LOCAL_ENROLLMENT),
        );
    }
    let agent_dir = socket_subdir(socket_dir, paths::AGENT_SOCKET_SUBDIR)?;
    let socket = spec
        .binds
        .iter_mut()
        .find(|b| b.source == names::AGENT_SOCKET_VOLUME)
        .ok_or_else(|| RenderError::Invalid("the agent recipe mounts no agent socket".into()))?;
    socket.source = agent_dir;
    Ok(())
}
