//! Storage readiness (#253) at the fake-root boundary: a homes root under a tempdir,
//! space facts injected, verdicts read back through `probe`.
//!
//! Identity: in the dev container the tests run as root, so the write test really hands
//! the leaf to another uid and writes as it. Outside a container the process cannot change
//! its fs identity, so the app identity is the test's own uid and the same assertions hold.

use super::super::storage::*;
use super::super::*;
use super::{get, FakeRoot};
use std::fs;
use std::os::unix::fs::PermissionsExt;

fn euid() -> u32 {
    // SAFETY: geteuid has no preconditions.
    unsafe { libc::geteuid() }
}

/// The identity the write test is asked to write as: another uid when we are root,
/// ourselves otherwise.
fn app_identity() -> (u32, u32) {
    if euid() == 0 {
        (1000, 1000)
    } else {
        // SAFETY: getegid has no preconditions.
        (euid(), unsafe { libc::getegid() })
    }
}

fn space(available_gib: u64) -> SpaceFacts {
    SpaceFacts {
        available_bytes: available_gib * GIB,
        total_bytes: 100 * GIB,
    }
}

fn root_with(path: &std::path::Path, facts: Option<SpaceFacts>) -> StorageRoot {
    StorageRoot {
        path: path.to_path_buf(),
        space: facts,
    }
}

/// A fake root with a homes directory of the given mode and the given space facts.
fn homes_env(root: &FakeRoot, mode: u32, facts: Option<SpaceFacts>) -> ProbeEnv {
    let homes = root.dir.join("var/lib/quasar/homes");
    fs::create_dir_all(&homes).unwrap();
    fs::set_permissions(&homes, fs::Permissions::from_mode(mode)).unwrap();
    let (uid, gid) = app_identity();
    ProbeEnv {
        app_uid: Some(uid),
        app_gid: Some(gid),
        storage: StorageView {
            homes: Some(root_with(&homes, facts)),
            ..Default::default()
        },
        ..root.env(false, "")
    }
}

/// Run `probe` as an agent that cannot override permissions: with the process's fs identity
/// dropped to the app identity when we are root (what a root-squashed NFS export or a FUSE
/// mount without `allow_other` looks like to the agent), unchanged otherwise. The drop lives
/// and dies with the spawned thread.
fn probe_without_dac_override(env: &ProbeEnv) -> Vec<ReadinessCheck> {
    let env = env.clone();
    std::thread::spawn(move || {
        if euid() == 0 {
            let (uid, gid) = app_identity();
            // SAFETY: setfsgid/setfsuid affect only the calling thread, which ends here.
            unsafe {
                libc::setfsgid(gid);
                libc::setfsuid(uid);
            }
        }
        probe(&env)
    })
    .join()
    .unwrap()
}

fn entries(dir: &std::path::Path) -> Vec<String> {
    let mut names: Vec<String> = fs::read_dir(dir)
        .unwrap()
        .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    names
}

#[test]
fn an_unwritable_homes_root_fails_with_remediation_and_leaves_nothing_behind() {
    let root = FakeRoot::new("storage-unwritable");
    let env = homes_env(&root, 0o500, Some(space(50)));
    let homes = env.storage.homes.as_ref().unwrap().path.clone();

    let checks = probe_without_dac_override(&env);

    let c = get(&checks, HOMES_WRITABLE_ID);
    assert_eq!(c.status, FAIL, "{c:?}");
    assert!(
        c.summary.contains(homes.to_str().unwrap()),
        "names the root: {c:?}"
    );
    assert!(!c.remediation.is_empty(), "carries a fix: {c:?}");
    assert!(
        c.summary.to_lowercase().contains("permission denied"),
        "carries the OS reason: {c:?}"
    );
    assert_eq!(entries(&homes), Vec::<String>::new(), "no residue");
    // Restore so the tempdir can be removed.
    fs::set_permissions(&homes, fs::Permissions::from_mode(0o755)).unwrap();
}

#[test]
fn low_homes_space_warns_and_names_the_floor_knob() {
    let root = FakeRoot::new("storage-low");
    let env = homes_env(&root, 0o755, Some(space(1)));

    let checks = probe(&env);

    let c = get(&checks, HOMES_FREE_SPACE_ID);
    assert_eq!(c.status, WARN, "{c:?}");
    assert!(c.summary.contains("5 GiB"), "names the floor: {c:?}");
    assert!(
        c.remediation.contains(FREE_FLOOR_ENV),
        "the fix names the one knob: {c:?}"
    );
    // A nearly full disk is not an unwritable one.
    assert_eq!(get(&checks, HOMES_WRITABLE_ID).status, PASS);
}

#[test]
fn exhausted_homes_space_fails() {
    let root = FakeRoot::new("storage-exhausted");
    let env = homes_env(
        &root,
        0o755,
        Some(SpaceFacts {
            available_bytes: 0,
            total_bytes: 100 * GIB,
        }),
    );

    let checks = probe(&env);

    let c = get(&checks, HOMES_FREE_SPACE_ID);
    assert_eq!(c.status, FAIL, "{c:?}");
    assert!(!c.remediation.is_empty(), "{c:?}");
}

#[test]
fn a_floor_is_respected_when_lowered() {
    let root = FakeRoot::new("storage-floor");
    let mut env = homes_env(&root, 0o755, Some(space(1)));
    env.storage.free_floor_bytes = GIB / 2;

    let checks = probe(&env);

    assert_eq!(get(&checks, HOMES_FREE_SPACE_ID).status, PASS);
}

#[test]
fn a_healthy_homes_root_passes_and_never_touches_an_existing_home() {
    let root = FakeRoot::new("storage-healthy");
    let env = homes_env(&root, 0o755, Some(space(50)));
    let homes = env.storage.homes.as_ref().unwrap().path.clone();
    let save = homes.join("alice/steam/save.dat");
    fs::create_dir_all(save.parent().unwrap()).unwrap();
    fs::write(&save, "progress").unwrap();

    let checks = probe(&env);

    let writable = get(&checks, HOMES_WRITABLE_ID);
    assert_eq!(writable.status, PASS, "{writable:?}");
    assert!(writable.remediation.is_empty());
    let free = get(&checks, HOMES_FREE_SPACE_ID);
    assert_eq!(free.status, PASS, "{free:?}");
    assert!(free.summary.contains("50"), "reports the number: {free:?}");
    assert_eq!(
        entries(&homes),
        vec!["alice".to_string()],
        "only the home remains"
    );
    assert_eq!(fs::read_to_string(&save).unwrap(), "progress");
}

#[test]
fn the_write_test_says_which_identity_wrote() {
    let root = FakeRoot::new("storage-identity");
    let env = homes_env(&root, 0o755, Some(space(50)));
    let (uid, _) = app_identity();

    let c = probe(&env)
        .into_iter()
        .find(|c| c.id == HOMES_WRITABLE_ID)
        .unwrap();

    assert_eq!(c.status, PASS, "{c:?}");
    assert!(
        c.summary.contains(&uid.to_string()),
        "the summary names the app uid it wrote as: {c:?}"
    );
}

#[test]
fn no_homes_root_configured_skips_the_homes_checks() {
    let root = FakeRoot::new("storage-unconfigured");
    let env = ProbeEnv {
        storage: StorageView::default(),
        ..root.env(false, "")
    };

    let checks = probe(&env);

    assert_eq!(get(&checks, HOMES_WRITABLE_ID).status, SKIP);
    assert_eq!(get(&checks, HOMES_FREE_SPACE_ID).status, SKIP);
    assert_eq!(get(&checks, TEMPLATE_FREE_SPACE_ID).status, SKIP);
    assert_eq!(get(&checks, IMAGE_FREE_SPACE_ID).status, SKIP);
}

#[test]
fn a_missing_homes_root_fails_naming_the_mount() {
    let root = FakeRoot::new("storage-missing");
    let missing = root.dir.join("var/lib/quasar/homes");
    let (uid, gid) = app_identity();
    let env = ProbeEnv {
        app_uid: Some(uid),
        app_gid: Some(gid),
        storage: StorageView {
            homes: Some(root_with(&missing, None)),
            ..Default::default()
        },
        ..root.env(false, "")
    };

    let checks = probe(&env);

    let c = get(&checks, HOMES_WRITABLE_ID);
    assert_eq!(c.status, FAIL, "{c:?}");
    assert!(
        c.remediation.contains("mount"),
        "a root absent inside the container is a mount problem: {c:?}"
    );
    // Space could not be read either: indeterminate, never a failure of its own.
    let free = get(&checks, HOMES_FREE_SPACE_ID);
    assert_eq!(free.status, WARN, "{free:?}");
}

#[test]
fn an_indeterminate_io_error_warns_with_its_reason() {
    let root = FakeRoot::new("storage-indeterminate");
    // The configured root is a regular file: the write test cannot start, and that is
    // neither a permission nor a space problem.
    root.file("var/lib/quasar/homes", "");
    let (uid, gid) = app_identity();
    let env = ProbeEnv {
        app_uid: Some(uid),
        app_gid: Some(gid),
        storage: StorageView {
            homes: Some(root_with(
                &root.dir.join("var/lib/quasar/homes"),
                Some(space(50)),
            )),
            ..Default::default()
        },
        ..root.env(false, "")
    };

    let c = probe(&env)
        .into_iter()
        .find(|c| c.id == HOMES_WRITABLE_ID)
        .unwrap();

    assert_eq!(c.status, WARN, "{c:?}");
    assert!(
        c.summary.to_lowercase().contains("not a directory"),
        "carries the OS reason: {c:?}"
    );
}

#[test]
fn the_probe_leaf_is_removed_even_when_the_write_fails_part_way() {
    let root = FakeRoot::new("storage-partial");
    let homes = root.dir.join("homes");
    fs::create_dir_all(&homes).unwrap();
    let (uid, gid) = app_identity();

    let outcome = write_probe(&homes, WriteIdentity::App { uid, gid }, |_file| {
        Err(std::io::Error::other("disk went away"))
    });

    match outcome {
        WriteOutcome::Indeterminate { stage, reason } => {
            assert_eq!(stage, WriteStage::WriteFile);
            assert!(reason.contains("disk went away"), "{reason}");
        }
        other => panic!("expected an indeterminate outcome, got {other:?}"),
    }
    assert_eq!(entries(&homes), Vec::<String>::new(), "the leaf is gone");
}

#[test]
fn a_write_that_runs_out_of_space_is_exhausted_not_indeterminate() {
    let root = FakeRoot::new("storage-enospc");
    let homes = root.dir.join("homes");
    fs::create_dir_all(&homes).unwrap();
    let (uid, gid) = app_identity();

    let outcome = write_probe(&homes, WriteIdentity::App { uid, gid }, |_file| {
        Err(std::io::Error::from_raw_os_error(libc::ENOSPC))
    });

    assert!(
        matches!(
            outcome,
            WriteOutcome::Exhausted {
                stage: WriteStage::WriteFile,
                ..
            }
        ),
        "{outcome:?}"
    );
    assert_eq!(entries(&homes), Vec::<String>::new());
}

#[test]
fn template_and_image_storage_only_ever_warn() {
    let root = FakeRoot::new("storage-warn-only");
    // A host that passes the vendor-neutral checks, so the only findings are storage's.
    root.file("dev/dri/renderD128", "")
        .file("dev/uinput", "")
        .file("proc/sys/user/max_user_namespaces", "15000\n");
    let exhausted = SpaceFacts {
        available_bytes: 0,
        total_bytes: 100 * GIB,
    };
    let env = ProbeEnv {
        storage: StorageView {
            templates: Some(root_with(&root.dir.join("templates"), Some(exhausted))),
            images: Some(root_with(&root.dir.join("docker"), Some(exhausted))),
            ..Default::default()
        },
        ..root.env(false, "")
    };

    let checks = probe(&env);

    for id in [TEMPLATE_FREE_SPACE_ID, IMAGE_FREE_SPACE_ID] {
        let c = get(&checks, id);
        assert_eq!(c.status, WARN, "{c:?}");
        assert!(!c.remediation.is_empty(), "{c:?}");
    }
    assert_eq!(
        log_report(&checks),
        0,
        "warn-only storage never reports as failed: {checks:?}"
    );
}

#[test]
fn template_and_image_storage_pass_with_room_and_warn_when_unreadable() {
    let root = FakeRoot::new("storage-aux");
    let env = ProbeEnv {
        storage: StorageView {
            templates: Some(root_with(&root.dir.join("templates"), Some(space(40)))),
            images: Some(root_with(&root.dir.join("docker"), None)),
            ..Default::default()
        },
        ..root.env(false, "")
    };

    let checks = probe(&env);

    assert_eq!(get(&checks, TEMPLATE_FREE_SPACE_ID).status, PASS);
    let images = get(&checks, IMAGE_FREE_SPACE_ID);
    assert_eq!(images.status, WARN, "{images:?}");
}

#[test]
fn the_free_space_floor_is_one_knob_with_a_safe_default() {
    assert_eq!(parse_free_floor_gib(None), DEFAULT_FREE_FLOOR_GIB * GIB);
    assert_eq!(parse_free_floor_gib(Some("")), DEFAULT_FREE_FLOOR_GIB * GIB);
    assert_eq!(
        parse_free_floor_gib(Some("lots")),
        DEFAULT_FREE_FLOOR_GIB * GIB
    );
    assert_eq!(
        parse_free_floor_gib(Some("0")),
        DEFAULT_FREE_FLOOR_GIB * GIB
    );
    assert_eq!(parse_free_floor_gib(Some(" 20 ")), 20 * GIB);
}

#[test]
fn the_identity_comes_from_the_app_uid_and_gid() {
    assert_eq!(
        WriteIdentity::from_env_pair(Some(99), Some(100)),
        WriteIdentity::App { uid: 99, gid: 100 }
    );
    assert_eq!(
        WriteIdentity::from_env_pair(Some(99), None),
        WriteIdentity::App { uid: 99, gid: 99 }
    );
    assert_eq!(
        WriteIdentity::from_env_pair(None, Some(100)),
        WriteIdentity::Agent
    );
}
