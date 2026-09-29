//! The behavioural cases. Each is written once and runs unchanged against every target;
//! what legitimately differs by engine mode comes from the target's row, never a branch here.

use crate::target::{Capability, HomeOwner};
use crate::Ctx;
use quasar_node_agent::runtime::{
    ApplicationMount, ApplicationRequest, ApplicationSecurity, ErrorKind, GpuInjection,
};
use quasar_runtime::platform::{ContainerSpec, Healthcheck, RestartPolicy};
use std::os::unix::fs::MetadataExt;
use std::time::{Duration, Instant};

pub struct Case {
    pub name: &'static str,
    /// A target lacking one of these reports the case as skipped, with the declared reason.
    pub needs: &'static [Capability],
    pub run: fn(&Ctx) -> String,
}

pub const CASES: &[Case] = &[
    Case {
        name: "identity",
        needs: &[],
        run: identity,
    },
    Case {
        name: "create-read-back",
        needs: &[],
        run: create_read_back,
    },
    Case {
        name: "user-mapping",
        needs: &[],
        run: user_mapping,
    },
    Case {
        name: "device-dri",
        needs: &[Capability::Dri],
        run: device_dri,
    },
    Case {
        name: "device-uinput",
        needs: &[Capability::Uinput],
        run: device_uinput,
    },
    Case {
        name: "cdi",
        needs: &[Capability::Cdi],
        run: cdi,
    },
    Case {
        name: "restart",
        needs: &[],
        run: restart,
    },
    Case {
        name: "health",
        needs: &[Capability::Health],
        run: health,
    },
    Case {
        name: "removal",
        needs: &[],
        run: removal,
    },
    Case {
        name: "errors",
        needs: &[],
        run: errors,
    },
];

/// The ids an app drops to. Distinct from any account a runner or lab user has, so the
/// owner of a file it writes says which mapping applied.
const APP_ID: u32 = 4321;

/// A session's capability set (`APP_CONTAINER_CAP_ADDS`, `session/container.rs`) with bit numbers.
const SESSION_CAPS: [(&str, u32); 8] = [
    ("CHOWN", 0),
    ("DAC_OVERRIDE", 1),
    ("FOWNER", 3),
    ("KILL", 5),
    ("SETGID", 6),
    ("SETUID", 7),
    ("SETPCAP", 8),
    ("SYS_NICE", 23),
];

fn session_security() -> ApplicationSecurity {
    ApplicationSecurity {
        cap_drop_all: true,
        cap_add: SESSION_CAPS.map(|(name, _)| name.to_string()).to_vec(),
        no_new_privileges: true,
        security_opt: vec!["seccomp=unconfined".into()],
        shm_size: 64 * 1024 * 1024,
        ..Default::default()
    }
}

/// Runs `script` as the app user (`APP_ID`) holding every supplementary group the
/// container was given, the way a session image's entrypoint drops from root.
fn as_app(script: &str) -> String {
    format!(
        "echo 'suite:x:{APP_ID}:{APP_ID}::/tmp:/bin/sh' >>/etc/passwd && \
         echo 'suite:x:{APP_ID}:' >>/etc/group && \
         for g in $(id -G); do [ \"$g\" = 0 ] || echo \"suite$g:x:$g:suite\" >>/etc/group; done && \
         exec su -s /bin/sh suite -c '{script}'"
    )
}

fn app_env() -> Vec<String> {
    vec![format!("PUID={APP_ID}"), format!("PGID={APP_ID}")]
}

fn poll<T>(within: Duration, mut probe: impl FnMut() -> Option<T>) -> Option<T> {
    let until = Instant::now() + within;
    loop {
        if let Some(value) = probe() {
            return Some(value);
        }
        if Instant::now() >= until {
            return None;
        }
        std::thread::sleep(Duration::from_millis(250));
    }
}

fn identity(ctx: &Ctx) -> String {
    let facts = ctx
        .runtime
        .inspect_engine()
        .wait()
        .expect("inspect the engine");
    assert_eq!(
        facts.info.kind,
        ctx.target.mode.engine,
        "the socket answers as {} {}",
        facts.info.kind.label(),
        facts.info.version
    );
    assert_eq!(
        facts.mode,
        ctx.target.mode.mode,
        "the engine reports itself {}",
        facts.mode.wire()
    );
    format!(
        "{} {} ({}), API {}, runtimes {:?}",
        facts.info.kind.label(),
        facts.info.version,
        facts.mode.wire(),
        facts.info.api_version,
        facts.runtimes
    )
}

/// A session-shaped container passes the runtime's read-back and runs with exactly what
/// was asked for; its named volume outlives it.
fn create_read_back(ctx: &Ctx) -> String {
    let volume = ctx.volume_name("session");
    let mount = |read_only| ApplicationMount::Volume {
        source: volume.clone(),
        target: "/data/vol".into(),
        read_only,
        no_copy: true,
    };
    let result = ctx.run_application(ApplicationRequest {
        command: vec![
            "sh".into(),
            "-c".into(),
            "grep -E '^(CapBnd|NoNewPrivs):' /proc/self/status; \
             df -k /dev/shm | tail -1; echo persisted >/data/vol/marker; echo readback-ok"
                .into(),
        ],
        environment: app_env(),
        typed_mounts: vec![mount(false)],
        security: session_security(),
        ..ctx.application("session")
    });
    assert_eq!(result.exit_code, Some(0), "{result:?}");
    assert!(result.stdout.contains("readback-ok"), "{result:?}");
    let field = |name: &str| {
        result
            .stdout
            .lines()
            .find_map(|l| l.strip_prefix(name))
            .map(str::trim)
            .unwrap_or_else(|| panic!("{name} missing: {}", result.stdout))
            .to_string()
    };
    let wanted = SESSION_CAPS
        .iter()
        .fold(0u64, |mask, (_, bit)| mask | 1 << bit);
    let bounding = u64::from_str_radix(&field("CapBnd:"), 16).expect("CapBnd is hex");
    assert_eq!(
        bounding, wanted,
        "capability bounding set {bounding:#x}, asked for {wanted:#x}"
    );
    assert_eq!(field("NoNewPrivs:"), "1");
    assert!(
        result
            .stdout
            .lines()
            .any(|l| l.split_whitespace().nth(1) == Some("65536")),
        "/dev/shm is the 64 MiB asked for: {}",
        result.stdout
    );

    let again = ctx.run_application(ApplicationRequest {
        command: vec!["cat".into(), "/data/vol/marker".into()],
        typed_mounts: vec![mount(true)],
        security: session_security(),
        ..ctx.application("reader")
    });
    assert_eq!(again.exit_code, Some(0), "{again:?}");
    assert_eq!(again.stdout.trim(), "persisted", "{again:?}");
    "session-shaped container read back; caps, no-new-privileges, shm and a named volume held"
        .into()
}

/// Decision D14: who owns, on the host, a file the app user writes into its home.
fn user_mapping(ctx: &Ctx) -> String {
    let home = ctx.fixture_dir("home");
    let result = ctx.run_application(ApplicationRequest {
        command: vec![
            "sh".into(),
            "-c".into(),
            as_app("echo saved >/suite/home/save"),
        ],
        environment: app_env(),
        typed_mounts: vec![ApplicationMount::Bind {
            source: home.to_string_lossy().into_owned(),
            target: "/suite/home".into(),
            read_only: false,
            consistency: None,
        }],
        security: session_security(),
        ..ctx.application("home")
    });
    assert_eq!(result.exit_code, Some(0), "{result:?}");
    let owner = std::fs::metadata(home.join("save"))
        .expect("the app's file is on the host")
        .uid();
    match ctx.target.mode.home_owner {
        HomeOwner::AppId => assert_eq!(owner, APP_ID, "owned by the app's PUID"),
        HomeOwner::EngineUser => {
            assert_eq!(owner, ctx.engine_uid, "owned by the engine's own user")
        }
        HomeOwner::SubordinateId => assert!(
            ![0, ctx.engine_uid, APP_ID].contains(&owner),
            "owned by a subordinate id of the engine user, not {owner}"
        ),
    }
    format!(
        "app user {APP_ID} wrote a home file owned on the host by uid {owner} (engine user {})",
        ctx.engine_uid
    )
}

/// Host device nodes passed in, opened by the app user through the groups the launcher's
/// rule grants.
fn pass_devices(ctx: &Ctx, what: &str, nodes: &[String]) -> String {
    let mut groups: Vec<u32> = nodes
        .iter()
        .map(|n| std::fs::metadata(n).unwrap_or_else(|e| panic!("{n}: {e}")))
        .filter(|md| quasar_node_agent::session::container::dri_group_granted(md.mode(), md.gid()))
        .map(|md| md.gid())
        .collect();
    groups.sort_unstable();
    groups.dedup();
    let opens = nodes
        .iter()
        .map(|n| format!("exec 3<>{n} && echo open-ok {n} && exec 3>&-"))
        .collect::<Vec<_>>()
        .join("; ");
    let result = ctx.run_application(ApplicationRequest {
        command: vec!["sh".into(), "-c".into(), as_app(&opens)],
        environment: app_env(),
        devices: nodes.to_vec(),
        group_add: groups.iter().map(u32::to_string).collect(),
        security: session_security(),
        ..ctx.application(what)
    });
    for node in nodes {
        assert!(
            result.stdout.contains(&format!("open-ok {node}")),
            "the app user could not open {node}: {result:?}"
        );
    }
    format!("app user opened {nodes:?} with groups {groups:?}")
}

fn device_dri(ctx: &Ctx) -> String {
    let mut nodes: Vec<String> = std::fs::read_dir("/dev/dri")
        .into_iter()
        .flatten()
        .flatten()
        .map(|e| e.path().to_string_lossy().into_owned())
        .filter(|p| p.contains("/renderD") || p.contains("/card"))
        .collect();
    nodes.sort();
    assert!(
        !nodes.is_empty(),
        "no DRM node on this host: declare dri=<reason> for this run"
    );
    pass_devices(ctx, "dri", &nodes)
}

fn device_uinput(ctx: &Ctx) -> String {
    assert!(
        std::path::Path::new("/dev/uinput").exists(),
        "no /dev/uinput on this host: declare uinput=<reason> for this run"
    );
    pass_devices(ctx, "uinput", &["/dev/uinput".to_string()])
}

/// Decision D10: the NVIDIA GPU reaches a session through CDI.
fn cdi(ctx: &Ctx) -> String {
    let host = ctx.runtime.engine_host().wait().expect("engine host");
    assert_eq!(
        host.gpu_injection,
        Some(GpuInjection::Cdi),
        "the engine injects NVIDIA through CDI (devices it lists: {:?})",
        host.cdi_devices
    );
    let result = ctx.run_application(ApplicationRequest {
        command: vec![
            "sh".into(),
            "-c".into(),
            "exec 3<>/dev/nvidiactl && ls /dev/nvidia* && echo cdi-ok".into(),
        ],
        nvidia_gpu: true,
        security: session_security(),
        ..ctx.application("cdi")
    });
    assert!(result.stdout.contains("cdi-ok"), "{result:?}");
    let nodes: Vec<&str> = result
        .stdout
        .lines()
        .filter(|l| l.starts_with("/dev/"))
        .collect();
    format!("CDI gave the session {nodes:?}")
}

fn log_count(ctx: &Ctx, id: &str, marker: &str) -> usize {
    ctx.runtime
        .container_logs_tail(id, 200)
        .wait()
        .expect("logs")
        .matches(marker)
        .count()
}

/// An `unless-stopped` service comes back after it exits, and stays down once stopped.
fn restart(ctx: &Ctx) -> String {
    let spec = ctx.service("restart", "echo suite-run; exit 3");
    assert_eq!(spec.restart, RestartPolicy::No);
    let id = ctx.create(spec);
    ctx.runtime
        .set_restart_policy(&id, RestartPolicy::UnlessStopped)
        .wait()
        .expect("set the restart policy");
    ctx.start(&id);
    let runs = poll(Duration::from_secs(60), || {
        let n = log_count(ctx, &id, "suite-run");
        (n >= 2).then_some(n)
    })
    .unwrap_or_else(|| {
        panic!(
            "never restarted after exiting: {:?}",
            ctx.inspect(&id).map(|c| c.status)
        )
    });
    let seen = ctx.inspect(&id).expect("still exists");
    assert_eq!(seen.restart, Some(RestartPolicy::UnlessStopped));
    ctx.runtime
        .stop_container(&id, Duration::from_secs(2))
        .wait()
        .expect("stop");
    let stopped = log_count(ctx, &id, "suite-run");
    std::thread::sleep(Duration::from_secs(4));
    let after = ctx.inspect(&id).expect("still exists");
    assert!(
        !after.running,
        "an explicit stop was undone: {}",
        after.status
    );
    assert_eq!(
        log_count(ctx, &id, "suite-run"),
        stopped,
        "it ran again after an explicit stop"
    );
    format!("restarted by the engine ({runs} runs seen), stayed down after stop")
}

/// The engine runs a service's own healthcheck, both ways.
fn health(ctx: &Ctx) -> String {
    let check = |test: &str| Healthcheck {
        test: vec!["CMD-SHELL".into(), test.into()],
        interval_s: 1,
        timeout_s: 2,
        retries: 2,
        start_period_s: 0,
    };
    let mut good = ctx.service("healthy", "touch /tmp/suite-ready; exec sleep 300");
    good.healthcheck = Some(check("test -f /tmp/suite-ready"));
    let mut bad = ctx.service("unhealthy", "exec sleep 300");
    bad.healthcheck = Some(check("test -f /tmp/never-there"));
    let (good, bad) = (ctx.create(good), ctx.create(bad));
    ctx.start(&good);
    ctx.start(&bad);
    let settle = |id: &str, want: &str| {
        poll(Duration::from_secs(60), || {
            let health = ctx.inspect(id).and_then(|c| c.health);
            (health.as_deref() == Some(want)).then_some(())
        })
        .unwrap_or_else(|| {
            panic!(
                "health never became {want}: {:?}",
                ctx.inspect(id).map(|c| c.health)
            )
        })
    };
    settle(&good, "healthy");
    settle(&bad, "unhealthy");
    "a passing check read healthy and a failing one unhealthy".into()
}

/// Removing a service leaves its named volume; a volume in use refuses removal; removing
/// what is already gone is not an error.
fn removal(ctx: &Ctx) -> String {
    let volume = ctx.volume_name("removal");
    let created = ctx
        .runtime
        .create_volume(&volume, ctx.labels())
        .wait()
        .expect("create a volume");
    assert_eq!(created.labels, ctx.labels());
    let mut spec = ctx.service("removal", "exec sleep 300");
    spec.binds.push(quasar_runtime::platform::Bind {
        source: volume.clone(),
        target: "/data".into(),
        read_only: false,
    });
    let id = ctx.create(spec);
    ctx.start(&id);
    assert!(ctx.inspect(&id).expect("exists").running);
    assert_eq!(
        ctx.runtime.remove_volume(&volume).wait().unwrap_err().kind,
        ErrorKind::Busy,
        "a volume in use refuses removal"
    );
    ctx.runtime
        .remove_container(&id)
        .wait()
        .expect("remove a running container");
    assert!(ctx.inspect(&id).is_none(), "the container is gone");
    assert!(
        ctx.runtime
            .inspect_volume(&volume)
            .wait()
            .unwrap()
            .is_some(),
        "a named volume outlives its container"
    );
    ctx.runtime
        .remove_volume(&volume)
        .wait()
        .expect("remove the volume");
    assert!(ctx
        .runtime
        .inspect_volume(&volume)
        .wait()
        .unwrap()
        .is_none());
    ctx.runtime
        .remove_container(&id)
        .wait()
        .expect("removing a missing container is not an error");
    ctx.runtime
        .remove_volume(&volume)
        .wait()
        .expect("removing a missing volume is not an error");
    "container removed while running; volume refused while in use, then removed".into()
}

/// What the engine cannot do is a named error, and leaves nothing behind.
fn errors(ctx: &Ctx) -> String {
    let absent = format!("localhost/quasar-engine-suite-absent-{}:none", ctx.run);

    let spec = ContainerSpec {
        image: absent.clone(),
        ..ctx.service("absent-image", "true")
    };
    let name = spec.name.clone();
    let refused = ctx
        .runtime
        .create_container(spec)
        .wait()
        .expect("the engine answers")
        .expect_err("a missing image cannot be created");
    assert_eq!(refused.status, 404, "{refused:?}");
    assert!(ctx.inspect(&name).is_none(), "nothing created");

    let request = ApplicationRequest {
        image: absent,
        command: vec!["true".into()],
        ..ctx.application("absent-image")
    };
    let name = request.name.clone();
    let error = ctx.start_application_err(request);
    assert_eq!(error.kind, ErrorKind::Missing, "{error}");
    assert!(ctx.inspect(&name).is_none(), "nothing created");

    let request = ApplicationRequest {
        command: vec!["true".into()],
        typed_mounts: vec![ApplicationMount::Bind {
            source: ctx
                .fixture_path("never-created")
                .to_string_lossy()
                .into_owned(),
            target: "/suite/missing".into(),
            read_only: false,
            consistency: None,
        }],
        ..ctx.application("missing-bind")
    };
    let name = request.name.clone();
    let error = ctx.start_application_err(request);
    assert!(ctx.inspect(&name).is_none(), "nothing left: {error}");

    assert!(ctx
        .runtime
        .inspect_platform_container(format!("quasar-engine-suite-{}-nothing", ctx.run))
        .wait()
        .expect("a missing container is an answer, not an error")
        .is_none());
    format!(
        "missing image refused (404, {:?}); missing bind source refused ({:?}); nothing left",
        ErrorKind::Missing,
        error.kind
    )
}
