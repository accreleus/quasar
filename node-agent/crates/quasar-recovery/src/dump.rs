//! The final dump an `uninstall --purge` takes of a Quasar-owned database before it deletes
//! anything (#352 R1-Q2: "D7's dump, not a bundle").
//!
//! A plain `pg_dump`, nothing more: this is not the pre-update dump of a migrating update
//! (#364 owns that, with its retention and `restore`). The database is dumped **offline**,
//! from its data volume, by a one-shot helper on the machine's own Postgres image that
//! starts a private postmaster with no TCP listener, runs `pg_dump` over its local socket,
//! and stops it. So the dump does not depend on the Postgres service still running (an
//! earlier `uninstall` may have removed it), needs no password (the image's local
//! connections are `trust`), and cannot race a writer: the control plane is removed first.
//!
//! The dump is written as `<name>.partial` and renamed only once `pg_dump` succeeded, so a
//! file under the final name is always whole.

use std::collections::BTreeMap;
use std::time::Duration;

use crate::engine::{ContainerSpec, EngineError, PlatformEngine, RestartPolicy};
use crate::recipe::{self, control, labels, names, paths, Bind, ImageRef};

/// What the helper container is, on its `io.quasar.helper` label.
pub const HELPER: &str = "final-dump";

/// How long a dump may take. Generous: a household database is small, but the helper also
/// replays WAL when the database was stopped uncleanly.
pub const DUMP_TIMEOUT: Duration = Duration::from_secs(30 * 60);

/// Where the dump goes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Dest {
    /// A named volume, created if missing, that a purge never deletes.
    Volume(String),
    /// An absolute directory on the engine host.
    HostDir(String),
}

impl Dest {
    /// Where the operator finds the dump.
    pub fn describe(&self, name: &str) -> String {
        match self {
            Dest::Volume(v) => format!("{name} in the {v} volume (copy it out with: docker run --rm -v {v}:/dump -v \"$PWD\":/out alpine cp /dump/{name} /out/)"),
            Dest::HostDir(d) => format!("{}/{name} on this machine", d.trim_end_matches('/')),
        }
    }
}

/// The dump's file name for a dump taken at `now` (RFC 3339): `quasar-final-20260926T101500Z.dump`.
pub fn file_name(now: &str) -> String {
    let stamp: String = now.chars().filter(|c| c.is_ascii_alphanumeric()).collect();
    format!("quasar-final-{stamp}.dump")
}

/// Runs the helper to completion. `Ok` is the dump's file name; `Err` says why there is none.
/// The helper is removed on every path.
pub fn take(
    engine: &dyn PlatformEngine,
    postgres_image: &ImageRef,
    installation_id: &str,
    dest: &Dest,
    name: &str,
) -> Result<String, String> {
    let dump_mount = match dest {
        Dest::Volume(volume) => {
            let exists = engine
                .inspect_volume(volume)
                .map_err(|e| format!("inspect the {volume} volume: {e}"))?
                .is_some();
            if !exists {
                let labels = BTreeMap::from([
                    (
                        labels::INSTALLATION.to_string(),
                        installation_id.to_string(),
                    ),
                    (labels::HELPER.to_string(), HELPER.to_string()),
                ]);
                engine
                    .create_volume(volume, &labels)
                    .map_err(|e| format!("create the {volume} volume: {e}"))?;
            }
            volume.clone()
        }
        Dest::HostDir(dir) => {
            if !dir.starts_with('/') {
                return Err(format!(
                    "the dump directory {dir:?} must be an absolute path on this machine"
                ));
            }
            dir.clone()
        }
    };
    // A helper an interrupted run left behind.
    if let Some(stale) = engine
        .inspect_container(names::FINAL_DUMP)
        .map_err(|e| format!("inspect {}: {e}", names::FINAL_DUMP))?
    {
        if stale.labels.get(labels::HELPER).map(String::as_str) != Some(HELPER) {
            return Err(format!(
                "a container this uninstall did not create holds the name {}; remove it and run again",
                names::FINAL_DUMP
            ));
        }
        engine
            .remove_container(&stale.id)
            .map_err(|e| format!("remove a leftover dump helper: {e}"))?;
    }
    let spec = helper_spec(postgres_image, &dump_mount, name);
    let id = engine
        .create_container(&spec)
        .map_err(|e| format!("create the dump helper: {e}"))?;
    let outcome = run(engine, &id);
    let removed = engine.remove_container(&id);
    let code = outcome?;
    if let Err(e) = removed {
        tracing::warn!(
            token = "uninstall-dump-helper-left",
            "the dump helper could not be removed: {e}"
        );
    }
    if code != 0 {
        return Err(format!("pg_dump exited {code}"));
    }
    Ok(name.to_string())
}

fn run(engine: &dyn PlatformEngine, id: &str) -> Result<i64, String> {
    engine
        .start_container(id)
        .map_err(|e| format!("start the dump helper: {e}"))?;
    match engine.wait_container(id, DUMP_TIMEOUT) {
        Ok(0) => Ok(0),
        Ok(code) => {
            let tail = engine.logs_tail(id, 20).unwrap_or_default();
            Err(format!(
                "the dump helper exited {code}{}",
                if tail.trim().is_empty() {
                    String::new()
                } else {
                    format!(":\n{}", tail.trim_end())
                }
            ))
        }
        Err(EngineError::Runtime(crate::engine::ErrorKind::Timeout)) => Err(format!(
            "the dump did not finish within {} minutes",
            DUMP_TIMEOUT.as_secs() / 60
        )),
        Err(e) => Err(format!("wait for the dump helper: {e}")),
    }
}

/// The script reads its inputs from the environment, so nothing is interpolated into it.
const SCRIPT: &str = r#"set -eu
as_pg() { if command -v su-exec >/dev/null 2>&1; then su-exec postgres "$@"; else gosu postgres "$@"; fi; }
as_pg pg_ctl -D "$PGDATA" -w -t 300 -o "-c listen_addresses=''" start
rc=0
as_pg pg_dump -U "$DUMP_USER" -d "$DUMP_DB" --format=custom > "/dump/$DUMP_NAME.partial" || rc=$?
as_pg pg_ctl -D "$PGDATA" -m fast -w stop || true
if [ "$rc" -eq 0 ]; then mv "/dump/$DUMP_NAME.partial" "/dump/$DUMP_NAME"; else rm -f "/dump/$DUMP_NAME.partial"; fi
exit "$rc"
"#;

fn helper_spec(image: &ImageRef, dump_mount: &str, name: &str) -> ContainerSpec {
    ContainerSpec {
        name: names::FINAL_DUMP.into(),
        image: image.reference(),
        entrypoint: Some(vec!["/bin/sh".into(), "-c".into(), SCRIPT.into()]),
        cmd: None,
        env: BTreeMap::from([
            ("PGDATA".to_string(), paths::POSTGRES_DATA_DIR.to_string()),
            ("DUMP_USER".into(), control::OWNED_DATABASE_USER.into()),
            ("DUMP_DB".into(), control::OWNED_DATABASE.into()),
            ("DUMP_NAME".into(), name.into()),
        ]),
        labels: BTreeMap::from([(labels::HELPER.to_string(), HELPER.to_string())]),
        network_mode: Some("none".into()),
        binds: vec![
            recipe::bind(names::POSTGRES_DATA_VOLUME, paths::POSTGRES_DATA_DIR, false),
            Bind {
                source: dump_mount.into(),
                target: "/dump".into(),
                read_only: false,
            },
        ],
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::No,
        ports: Vec::new(),
        healthcheck: None,
    }
}
