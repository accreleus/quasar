//! Golden rendered specifications per role, revision and GPU vendor (ADR 0008). A change
//! to a rendered shape must show up as a reviewed diff of these files; regenerate with
//! `QUASAR_UPDATE_GOLDEN=1 cargo test -p quasar-recovery --test recipe_golden`.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

use quasar_recovery::recipe::{
    names, render, secrets, Book, GpuFacts, GpuVendor, HostDevices, ImageRef, Inputs, RenderError,
    Role, SecretMounts,
};

const AGENT_IMAGE: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:bb22000000000000000000000000000000000000000000000000000000000000";
const ACTOR_IMAGE: &str = "registry.example.invalid/quasar/quasar-recovery@sha256:cc33000000000000000000000000000000000000000000000000000000000000";

fn golden_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/recovery/recipes")
}

pub fn inputs(vendor: Option<GpuVendor>) -> Inputs {
    let (render_node, nvidia_runtime) = match vendor {
        Some(GpuVendor::Nvidia) => (Some("/dev/dri/renderD128"), true),
        Some(GpuVendor::Amd) => (Some("/dev/dri/renderD129"), false),
        Some(GpuVendor::Intel) => (Some("/dev/dri/renderD128"), false),
        None => (None, false),
    };
    Inputs {
        installation_id: "5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89".into(),
        node_name: "gpu-host-01".into(),
        home_root: "/srv/quasar/homes".into(),
        template_root: "/var/lib/quasar/templates".into(),
        docker_socket: "/var/run/docker.sock".into(),
        gpu: GpuFacts {
            vendor,
            render_node: render_node.map(str::to_owned),
            nvidia_runtime,
        },
        devices: HostDevices {
            dri: vendor.is_some(),
            uinput: true,
            kmsg: true,
        },
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
        (Role::NodeAgent, 2),
        (Role::RecoveryActor, 2),
        (Role::ControlPlane, 1),
        (Role::Postgres, 1),
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
}
