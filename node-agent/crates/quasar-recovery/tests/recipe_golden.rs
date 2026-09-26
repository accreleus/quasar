//! Golden rendered specifications per role, revision and GPU vendor (ADR 0008). A change
//! to a rendered shape must show up as a reviewed diff of these files; regenerate with
//! `QUASAR_UPDATE_GOLDEN=1 cargo test -p quasar-recovery --test recipe_golden`.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

use quasar_recovery::recipe::{
    names, render, secrets, Book, ControlInputs, DatabaseInputs, GpuFacts, GpuVendor, HostDevices,
    ImageRef, Inputs, RenderError, Role, SecretMounts,
};

const AGENT_IMAGE: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:bb22000000000000000000000000000000000000000000000000000000000000";
const ACTOR_IMAGE: &str = "registry.example.invalid/quasar/quasar-recovery@sha256:cc33000000000000000000000000000000000000000000000000000000000000";

fn golden_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/recovery/recipes")
}

pub fn inputs(vendor: Option<GpuVendor>) -> Inputs {
    let (render_node, gpus_served) = match vendor {
        Some(GpuVendor::Nvidia) => (Some("/dev/dri/renderD128"), true),
        Some(GpuVendor::Amd) => (Some("/dev/dri/renderD129"), false),
        Some(GpuVendor::Intel) => (Some("/dev/dri/renderD128"), false),
        None => (None, false),
    };
    Inputs {
        unknown: Default::default(),
        installation_id: "5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89".into(),
        node_name: "gpu-host-01".into(),
        home_root: "/srv/quasar/homes".into(),
        template_root: "/var/lib/quasar/templates".into(),
        docker_socket: "/var/run/docker.sock".into(),
        gpu: GpuFacts {
            unknown: Default::default(),
            vendor,
            render_node: render_node.map(str::to_owned),
            gpus_served,
            fallback: None,
        },
        devices: HostDevices {
            unknown: Default::default(),
            dri: vendor.is_some(),
            uinput: true,
            kmsg: true,
        },
        control: None,
        socket_dir: None,
        trust: Default::default(),
        enroll: Default::default(),
        app: Default::default(),
    }
}

fn agent_secrets() -> SecretMounts {
    SecretMounts {
        volume: Some(names::NODE_AGENT_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::ENROLLMENT.to_string()]),
    }
}

fn check(name: &str, spec: &impl serde::Serialize) {
    let path = golden_dir().join(name);
    let rendered = serde_json::to_string_pretty(spec).unwrap() + "\n";
    if std::env::var_os("QUASAR_UPDATE_GOLDEN").is_some() {
        std::fs::create_dir_all(golden_dir()).unwrap();
        std::fs::write(&path, &rendered).unwrap();
    }
    let golden = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("{}: {e} (QUASAR_UPDATE_GOLDEN=1 writes it)", path.display()));
    assert_eq!(rendered, golden, "{name} differs from its golden file");
}

#[test]
fn node_agent_revision_1_renders_its_golden_specification_per_vendor() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for (vendor, file) in [
        (Some(GpuVendor::Nvidia), "node-agent-r1-nvidia.json"),
        (Some(GpuVendor::Amd), "node-agent-r1-amd.json"),
        (Some(GpuVendor::Intel), "node-agent-r1-intel.json"),
        (None, "node-agent-r1-none.json"),
    ] {
        let spec = render(
            Role::NodeAgent,
            1,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        check(file, &spec);
    }
}

#[test]
fn recovery_actor_revision_1_renders_its_golden_specification() {
    let image = ImageRef::parse(ACTOR_IMAGE).unwrap();
    let spec = render(
        Role::RecoveryActor,
        1,
        &inputs(None),
        &image,
        &SecretMounts::default(),
    )
    .unwrap();
    check("recovery-actor-r1.json", &spec);
}

#[test]
fn a_revision_the_book_does_not_carry_is_recipe_unsupported() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for (role, revision) in [
        (Role::NodeAgent, 0),
        (Role::NodeAgent, 3),
        (Role::RecoveryActor, 2),
        (Role::ControlPlane, 0),
        (Role::ControlPlane, 3),
        (Role::Postgres, 2),
    ] {
        assert_eq!(
            render(
                role,
                revision,
                &inputs(None),
                &image,
                &SecretMounts::default()
            ),
            Err(RenderError::Unsupported { role, revision })
        );
        assert!(!Book::supports(role, revision));
    }
}

#[test]
fn inputs_that_could_widen_host_access_are_refused() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let mut bad = Vec::new();
    for home in ["/", "relative/homes", "/srv/../etc", "/srv/a:b", "/srv/a,b"] {
        let mut i = inputs(None);
        i.home_root = home.into();
        bad.push(i);
    }
    let mut i = inputs(None);
    i.template_root = i.home_root.clone();
    bad.push(i);
    let mut i = inputs(None);
    i.node_name = "gpu host; rm".into();
    bad.push(i);
    let mut i = inputs(Some(GpuVendor::Amd));
    i.gpu.render_node = Some("/dev/sda".into());
    bad.push(i);
    for i in bad {
        assert!(
            matches!(
                render(Role::NodeAgent, 1, &i, &image, &agent_secrets()),
                Err(RenderError::Invalid(_))
            ),
            "{i:?}"
        );
    }
    for reference in [
        "registry.example.invalid/quasar/quasar-node-agent:latest",
        "registry.example.invalid/quasar/quasar-node-agent@sha256:BB22",
        "registry.example.invalid/quasar/quasar-node-agent:dev@sha256:bb22000000000000000000000000000000000000000000000000000000000000",
    ] {
        assert!(ImageRef::parse(reference).is_err(), "{reference}");
    }
}

#[test]
fn the_same_inputs_always_render_the_same_specification_and_digest() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let a = render(
        Role::NodeAgent,
        1,
        &inputs(Some(GpuVendor::Amd)),
        &image,
        &agent_secrets(),
    )
    .unwrap();
    let b = render(
        Role::NodeAgent,
        1,
        &inputs(Some(GpuVendor::Amd)),
        &image,
        &agent_secrets(),
    )
    .unwrap();
    assert_eq!(a, b);
    let c = render(
        Role::NodeAgent,
        1,
        &inputs(Some(GpuVendor::Intel)),
        &image,
        &agent_secrets(),
    )
    .unwrap();
    assert_ne!(a.labels["io.quasar.spec"], c.labels["io.quasar.spec"]);
}

fn declared_revision(file: &Path) -> u32 {
    let text = std::fs::read_to_string(file).unwrap();
    let line = text
        .lines()
        .find(|l| l.starts_with("pub const RECIPE_REVISION: u32 = "))
        .unwrap_or_else(|| {
            panic!(
                "{}: no `pub const RECIPE_REVISION: u32 = N;`",
                file.display()
            )
        });
    line.trim_start_matches("pub const RECIPE_REVISION: u32 = ")
        .trim_end_matches(';')
        .parse()
        .unwrap()
}

/// The release-time rule in miniature (ADR 0008): the actor built from this tree carries
/// every revision the platform images built from this tree declare. The constants are the
/// ones `deploy/build-images.sh` stamps as `org.quasar.recipe`.
#[test]
fn every_revision_the_tree_declares_is_carried_by_the_book() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let agent = declared_revision(&root.join("../../src/recipe.rs"));
    assert!(
        Book::supports(Role::NodeAgent, agent),
        "node-agent revision {agent}"
    );
    let actor = declared_revision(&root.join("src/recipe/revision.rs"));
    assert!(
        Book::supports(Role::RecoveryActor, actor),
        "recovery-actor revision {actor}"
    );
    let go =
        std::fs::read_to_string(root.join("../../../control-plane/internal/buildinfo/recipe.go"))
            .unwrap();
    let control: u32 = go
        .lines()
        .find_map(|l| l.strip_prefix("const RecipeRevision = "))
        .expect("control-plane/internal/buildinfo/recipe.go: const RecipeRevision = N")
        .trim()
        .parse()
        .unwrap();
    assert!(
        Book::supports(Role::ControlPlane, control),
        "control-plane revision {control}"
    );
}

const CONTROL_IMAGE: &str = "registry.example.invalid/quasar/quasar-control-plane@sha256:aa11000000000000000000000000000000000000000000000000000000000000";
const POSTGRES_IMAGE: &str = "docker.io/library/postgres@sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const SOCKET_DIR: &str = "/var/lib/docker/volumes/quasar-recovery-agent/_data";

pub fn combined(database: DatabaseInputs) -> Inputs {
    let mut i = inputs(Some(GpuVendor::Amd));
    i.node_name = "living-room-pc".into();
    i.control = Some(ControlInputs {
        unknown: Default::default(),
        machine_role: quasar_recovery::recipe::ControlRole::Combined,
        trusted_proxies: None,
        http_port: 8080,
        tls_port: 8443,
        public_host: Some("quasar.example.invalid".into()),
        tls_hosts: None,
        database,
    });
    i.socket_dir = Some(SOCKET_DIR.into());
    i
}

fn external() -> DatabaseInputs {
    DatabaseInputs::External {
        unknown: Default::default(),
        host: "db.example.invalid".into(),
        port: 5433,
        user: "quasar_app".into(),
        name: "quasar_prod".into(),
        sslmode: "require".into(),
    }
}

fn control_secrets(local: bool) -> SecretMounts {
    let mut files = BTreeSet::from([
        secrets::DATABASE_PASSWORD.to_string(),
        secrets::SECRET_KEY.to_string(),
    ]);
    if local {
        files.insert(secrets::LOCAL_ENROLLMENT.to_string());
    }
    SecretMounts {
        volume: Some(names::CONTROL_PLANE_SECRETS_VOLUME.into()),
        files,
    }
}

fn local_agent_secrets() -> SecretMounts {
    SecretMounts {
        volume: Some(names::NODE_AGENT_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::LOCAL_ENROLLMENT.to_string()]),
    }
}

#[test]
fn the_combined_and_control_only_recipes_render_their_golden_specifications() {
    let cp = ImageRef::parse(CONTROL_IMAGE).unwrap();
    let pg = ImageRef::parse(POSTGRES_IMAGE).unwrap();
    let agent = ImageRef::parse(AGENT_IMAGE).unwrap();
    let pg_secrets = SecretMounts {
        volume: Some(names::POSTGRES_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::DATABASE_PASSWORD.to_string()]),
    };
    let owned = combined(DatabaseInputs::Owned);
    let spec = render(Role::Postgres, 1, &owned, &pg, &pg_secrets).unwrap();
    check("postgres-r1.json", &spec);
    let spec = render(Role::ControlPlane, 1, &owned, &cp, &control_secrets(true)).unwrap();
    check("control-plane-r1-combined.json", &spec);
    let mut control_only = combined(external());
    control_only.home_root = String::new();
    control_only.template_root = quasar_recovery::recipe::default_template_root();
    control_only.node_name = "attic-server".into();
    control_only.control.as_mut().unwrap().machine_role =
        quasar_recovery::recipe::ControlRole::ControlOnly;
    let spec = render(
        Role::ControlPlane,
        1,
        &control_only,
        &cp,
        &control_secrets(false),
    )
    .unwrap();
    check("control-plane-r1-control-only-external.json", &spec);
    let spec = render(Role::NodeAgent, 2, &owned, &agent, &local_agent_secrets()).unwrap();
    check("node-agent-r2-combined-amd.json", &spec);

    // Revision 2 (#365): Add host's install-time images are fallbacks below the installed
    // release, and only the operator's overrides reach QUASAR_ENROLL_*.
    let mut enrolling = owned.clone();
    enrolling.enroll = quasar_recovery::recipe::EnrollImages {
        seed: Some(ImageRef::parse(ACTOR_IMAGE).unwrap()),
        agent: Some(agent.clone()),
        agent_override: Some(ImageRef::parse(OVERRIDE_AGENT_IMAGE).unwrap()),
        ..Default::default()
    };
    let spec = render(
        Role::ControlPlane,
        2,
        &enrolling,
        &cp,
        &control_secrets(true),
    )
    .unwrap();
    check("control-plane-r2-combined.json", &spec);
    assert_eq!(spec.env["QUASAR_ENROLL_SEED_IMAGE"], "");
    assert_eq!(spec.env["QUASAR_ENROLL_AGENT_IMAGE"], OVERRIDE_AGENT_IMAGE);
    assert_eq!(spec.env["QUASAR_ENROLL_FALLBACK_SEED_IMAGE"], ACTOR_IMAGE);
    assert_eq!(spec.env["QUASAR_ENROLL_FALLBACK_AGENT_IMAGE"], AGENT_IMAGE);
    let spec = render(
        Role::ControlPlane,
        2,
        &control_only,
        &cp,
        &control_secrets(false),
    )
    .unwrap();
    check("control-plane-r2-control-only-external.json", &spec);

    // Revision 1 has no fallback variables: an override, else the install-time image.
    let spec = render(
        Role::ControlPlane,
        1,
        &enrolling,
        &cp,
        &control_secrets(true),
    )
    .unwrap();
    assert_eq!(spec.env["QUASAR_ENROLL_SEED_IMAGE"], ACTOR_IMAGE);
    assert_eq!(spec.env["QUASAR_ENROLL_AGENT_IMAGE"], OVERRIDE_AGENT_IMAGE);
    assert!(!spec.env.contains_key("QUASAR_ENROLL_FALLBACK_SEED_IMAGE"));
}

const OVERRIDE_AGENT_IMAGE: &str =
    "registry.example.invalid/dev/quasar-node-agent@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee";

#[test]
fn a_gpu_hosts_agent_has_the_same_shape_at_revisions_1_and_2() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    for vendor in [Some(GpuVendor::Nvidia), Some(GpuVendor::Amd), None] {
        let mut one = render(
            Role::NodeAgent,
            1,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        let mut two = render(
            Role::NodeAgent,
            2,
            &inputs(vendor),
            &image,
            &agent_secrets(),
        )
        .unwrap();
        for spec in [&mut one, &mut two] {
            spec.labels.remove("io.quasar.recipe");
            spec.labels.remove("io.quasar.spec");
        }
        assert_eq!(one, two, "{vendor:?}");
    }
}

#[test]
fn a_combined_hosts_agent_needs_revision_2_and_is_given_only_its_own_socket() {
    let image = ImageRef::parse(AGENT_IMAGE).unwrap();
    let owned = combined(DatabaseInputs::Owned);
    assert_eq!(
        render(Role::NodeAgent, 1, &owned, &image, &local_agent_secrets()),
        Err(RenderError::Unsupported {
            role: Role::NodeAgent,
            revision: 1
        })
    );
    let spec = render(Role::NodeAgent, 2, &owned, &image, &local_agent_secrets()).unwrap();
    assert_eq!(spec.env["CONTROL_PLANE_URL"], "ws://127.0.0.1:8080");
    assert_eq!(
        spec.env["ENROLLMENT_TOKEN_FILE"],
        "/run/quasar-secrets/local-enrollment"
    );
    assert!(!spec.env.contains_key("ENROLLMENT_TOKEN"));
    assert!(!spec.env.contains_key("QUASAR_ENROLLMENT_FILE"));
    let sockets: Vec<_> = spec
        .binds
        .iter()
        .filter(|b| b.target == "/run/quasar-recovery")
        .collect();
    assert_eq!(sockets.len(), 1);
    assert_eq!(sockets[0].source, format!("{SOCKET_DIR}/agent"));
    assert!(sockets[0].read_only);
    let cp = render(
        Role::ControlPlane,
        1,
        &owned,
        &ImageRef::parse(CONTROL_IMAGE).unwrap(),
        &control_secrets(true),
    )
    .unwrap();
    // Neither is given the other's socket, nor the actor's whole volume.
    for s in [&spec, &cp] {
        assert!(s
            .binds
            .iter()
            .all(|b| b.source != names::AGENT_SOCKET_VOLUME && b.source != names::MACHINE_VOLUME));
    }
    let cp_socket = cp
        .binds
        .iter()
        .find(|b| b.target == "/run/quasar-recovery")
        .unwrap();
    assert_eq!(cp_socket.source, format!("{SOCKET_DIR}/control"));
}

#[test]
fn the_control_plane_gets_its_secrets_as_files_never_as_values() {
    let owned = combined(DatabaseInputs::Owned);
    let cp = ImageRef::parse(CONTROL_IMAGE).unwrap();
    let spec = render(Role::ControlPlane, 1, &owned, &cp, &control_secrets(true)).unwrap();
    for key in [
        "DATABASE_URL",
        "QUASAR_DATABASE_PASSWORD",
        "QUASAR_SECRET_KEY",
        "ENROLLMENT_TOKEN",
        "POSTGRES_PASSWORD",
    ] {
        assert!(!spec.env.contains_key(key), "{key}");
    }
    assert_eq!(
        spec.env["QUASAR_DATABASE_PASSWORD_FILE"],
        "/run/quasar-secrets/database-password"
    );
    assert_eq!(
        spec.env["QUASAR_SECRET_KEY_FILE"],
        "/run/quasar-secrets/secret-key"
    );
    assert_eq!(
        spec.env["QUASAR_LOCAL_ENROLLMENT_NODE_NAME"],
        "living-room-pc"
    );
    // Its TLS pair lives in a named volume, which outlives every replacement.
    assert!(spec
        .binds
        .iter()
        .any(|b| b.source == names::CONTROL_DATA_VOLUME && b.target == "/var/lib/quasar-control"));
    let mut missing = control_secrets(true);
    missing.files.remove(secrets::SECRET_KEY);
    assert!(matches!(
        render(Role::ControlPlane, 1, &owned, &cp, &missing),
        Err(RenderError::Invalid(_))
    ));
}

#[test]
fn an_external_database_gets_no_postgres_and_its_settings_reach_the_control_plane() {
    let mut i = combined(external());
    i.home_root = String::new();
    let pg = ImageRef::parse(POSTGRES_IMAGE).unwrap();
    let pg_secrets = SecretMounts {
        volume: Some(names::POSTGRES_SECRETS_VOLUME.into()),
        files: BTreeSet::from([secrets::DATABASE_PASSWORD.to_string()]),
    };
    assert!(matches!(
        render(Role::Postgres, 1, &i, &pg, &pg_secrets),
        Err(RenderError::Invalid(_))
    ));
    let spec = render(
        Role::ControlPlane,
        1,
        &i,
        &ImageRef::parse(CONTROL_IMAGE).unwrap(),
        &control_secrets(false),
    )
    .unwrap();
    assert_eq!(spec.env["QUASAR_DATABASE_HOST"], "db.example.invalid");
    assert_eq!(spec.env["QUASAR_DATABASE_PORT"], "5433");
    assert_eq!(spec.env["QUASAR_DATABASE_SSLMODE"], "require");
    assert!(!spec.env.contains_key("QUASAR_HOME_ROOT"));
    assert!(!spec.env.contains_key("QUASAR_LOCAL_ENROLLMENT_FILE"));
}

#[test]
fn control_inputs_that_could_inject_are_refused() {
    let cp = ImageRef::parse(CONTROL_IMAGE).unwrap();
    let mut bad = Vec::new();
    for host in ["db host", "db;rm", "a,b", ""] {
        let mut i = combined(external());
        if let Some(c) = i.control.as_mut() {
            c.database = DatabaseInputs::External {
                unknown: Default::default(),
                host: host.into(),
                port: 5432,
                user: "quasar".into(),
                name: "quasar".into(),
                sslmode: "disable".into(),
            };
        }
        bad.push(i);
    }
    let mut i = combined(DatabaseInputs::Owned);
    i.control.as_mut().unwrap().public_host = Some("evil host".into());
    bad.push(i);
    let mut i = combined(DatabaseInputs::Owned);
    i.control.as_mut().unwrap().tls_port = 8080;
    bad.push(i);
    let mut i = combined(DatabaseInputs::Owned);
    i.socket_dir = None;
    bad.push(i);
    let mut i = combined(DatabaseInputs::Owned);
    i.socket_dir = Some("relative".into());
    bad.push(i);
    for i in bad {
        assert!(
            matches!(
                render(Role::ControlPlane, 1, &i, &cp, &control_secrets(true)),
                Err(RenderError::Invalid(_))
            ),
            "{i:?}"
        );
    }
}
