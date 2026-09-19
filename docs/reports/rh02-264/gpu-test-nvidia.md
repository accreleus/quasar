# RH-02 #264 readiness-faults report

| field | value |
| --- | --- |
| role | gpu-test-nvidia |
| rid | rh02h-ed01e9f5 |
| gpu vendor | nvidia |
| gpu index | 0 |
| control image | <registry>/quasar-control-plane:dev-901058f |
| agent image | <registry>/quasar-node-agent:dev-97fd0d9 |
| control image source commit | 901058fbb503382eed758c8f718fba5b4cffc912 |
| agent image source commit | 97fd0d9b93e17933afd4f9e151c3bf44541800bd |
| schema_migrations version | 85 |
| pass | 96 |
| fail | 0 |
| unperformed | 4 |
| verdict | incomplete |

## Matrix rows

| # | result | message |
| --- | --- | --- |
| preflight | pass | preflight: engine clear (foreign_quasar_projects=0, port 18080 free, /var/lib/rh02h-ed01e9f5 free) |
| image-clean | pass | image-clean: agent image carries no readiness-fixture path |
| image-clean | pass | image-clean: agent binary has no harness_synthetic_ string |
| fixture | pass | fixture: readiness-fixture image built |
| fixture | pass | fixture: relay running, rule off (transparent) |
| stack | pass | stack: control plane answering /health |
| login | pass | login: admin token obtained |
| nonadmin | pass | nonadmin: user provisioned |
| seed | pass | seed: app 'rh02h-ed01e9f5: app-nohome' ready (5f08de49-ad76-4b06-89d5-c4bfd6009ceb) |
| seed | pass | seed: app 'rh02h-ed01e9f5: app-home' ready (916b9a80-e8bc-4d27-835e-b04d26d476dc) |
| baseline | pass | baseline: input_probe passes before any fault |
| baseline | pass | baseline: audio_probe passes before any fault |
| baseline | pass | baseline: launch reached running |
| 1a | pass | 1a: host is online while its runtime is down |
| 1a | pass | 1a: runtime_endpoint fails with its fix text |
| 1a | pass | 1a: runtime_endpoint carries blocks {scope: host, enforced_by: agent} |
| 1a | pass | 1a: runtime_endpoint listed in readiness_gate.blocking, enforced_by agent |
| 1a | pass | 1a: startup_cleanup fails with its fix text |
| 1a | pass | 1a: startup_cleanup carries blocks {scope: host, enforced_by: agent} |
| 1a | pass | 1a: startup_cleanup listed in readiness_gate.blocking, enforced_by agent |
| 1b | pass | 1b: agent health answers 503 while the runtime is down |
| 1d | pass | 1d: override PUT on runtime_endpoint refused 409 |
| 1d | pass | 1d: override PUT on startup_cleanup refused 409 |
| UNPERFORMED 1c | skip | UNPERFORMED 1c: the nested host could not serve a launch before the fault on this host (a pre-fault launch got HTTP 503 code=no_host_available), so a refusal could not have readiness as its sole reason |
| 1a | pass | 1a: runtime_endpoint recovers to pass |
| 1a | pass | 1a: both checks left readiness_gate.blocking |
| 1b | pass | 1b: agent health returns to 200 |
| 1e | pass | 1e: agent container never restarted (StartedAt and RestartCount unchanged) |
| 1e | pass | 1e: the agent process that reported the fault is the one that resumed (pid unchanged) |
| UNPERFORMED 1c | skip | UNPERFORMED 1c: recovery launch — the nested host could not serve a launch before the fault either (a pre-fault launch got HTTP 503 code=no_host_available) |
| 1 | pass | 1: nested host row deleted 204 |
| 2a | pass | 2a: homes_root_writable fails with scope homes |
| 2a | pass | 2a: app-home launch refused 503 host_not_ready |
| 2a | pass | 2a: app-nohome launch reaches running |
| 2a | pass | 2a: homes_root_writable recovers to pass |
| 2a | pass | 2a: app-home launch reaches running after recovery |
| 2b | pass | 2b: homes_free_space fails with scope homes |
| 2b | pass | 2b: app-home refused 503 host_not_ready |
| 2b | pass | 2b: app-nohome runs |
| 2b | pass | 2b: homes_free_space no longer fails after removing the fill file (status 'warn') |
| 2b | pass | 2b: app-home launch reaches running after recovery |
| 3 | pass | 3: input_probe fails with scope host |
| 3 | pass | 3: launch refused 503 host_not_ready |
| 3 | pass | 3: input_probe recovers to pass |
| 3 | pass | 3: launch runs after recovery |
| UNPERFORMED 4a | skip | UNPERFORMED 4a: host GPU vendor is 'nvidia', not amd |
| UNPERFORMED 4a' | skip | UNPERFORMED 4a': host GPU vendor is 'nvidia', not amd |
| 4b | pass | 4b: media_probe_gpu0 fail carries blocks.scope=gpu gpu_index=0 |
| 4b | pass | 4b: application_gpu_probe_gpu0 fail carries blocks.scope=gpu gpu_index=0 |
| 4b | pass | 4b: media_probe_gpu0 is fail |
| 4b | pass | 4b: launch refused host_not_ready |
| 4c (4b) | pass | 4c (4b): launch placed on the free scripted host (session.host_id matches) |
| 4c (4b) | pass | 4c (4b): scripted host row deleted 204 |
| 4b | pass | 4b: probes recover to pass |
| 4b | pass | 4b: launch runs after recovery |
| 5a | pass | 5a: dri_node_app_access fail carries no blocks |
| 5a | pass | 5a: dri_node_app_access absent from blocking |
| 5a | pass | 5a: launch still reaches running |
| 5b | pass | 5b: audio_probe reports unknown |
| 5b | pass | 5b: audio_probe absent from blocking |
| 5b | pass | 5b: launch is placed (201) |
| 5b | pass | 5b: audio_probe recovers to pass |
| 6a | pass | 6a: the first launch fills the ready host |
| 6a | pass | 6a: second launch refused 503 capacity_exhausted with the blocked host also online |
| 6b | pass | 6b: launch refused 503 no_host_available with nothing online |
| 6c | pass | 6c: launch refused host_not_ready with only the blocked host online |
| 6c | pass | 6c: no Retry-After header |
| 6c | pass | 6c: the message names none of the 36 check ids reported in this run |
| 6d | pass | 6d: the message names none of the 36 check ids reported in this run |
| 6d | pass | 6d: GET /v1/hosts is 403 for non-admin |
| 6d | pass | 6d: GET /v1/hosts/{id} is 403 for non-admin |
| 7a | pass | 7a: launch refused host_not_ready before the override |
| 7a | pass | 7a: override PUT -> 200 |
| 7a | pass | 7a: repeat PUT -> 200, no second audit row |
| 7a | pass | 7a: audit host.readiness_override.set severity warn |
| 7a | pass | 7a: the audit detail carries check_id and node_name |
| 7b | pass | 7b: still visible — blocking lists it, overridden true, readiness[] still fail |
| 7c | pass | 7c: launch succeeds and reaches running with the override |
| 7d | pass | 7d: check reads pass in readiness[] |
| 7d | pass | 7d: override lapsed (gone from readiness_overrides) |
| 7d | pass | 7d: audit .lapsed exists with actor null |
| 7e | pass | 7e: override set again -> 200 |
| 7e | pass | 7e: DELETE -> 204 |
| 7e | pass | 7e: repeat DELETE -> 204 (idempotent) |
| 7e | pass | 7e: exactly one .cleared audit row for this check (the idempotent repeat wrote none) |
| 7e | pass | 7e: launch refused host_not_ready again after clearing |
| 7f | pass | 7f: override set before the rename -> 200 |
| 7f | pass | 7f: old override listed inert |
| 7f | pass | 7f: new id in blocking, overridden false |
| 7f | pass | 7f: launch refused host_not_ready for the renamed check |
| 7f | pass | 7f: blocking empty after rule off + override delete |
| 7f | pass | 7f: launch runs after full clear |
| 7g | pass | 7g: non-admin PUT on unknown host -> 403 (not 404) |
| 7h | pass | 7h: bad check_id -> 400 validation_failed |
| 7h | pass | 7h: 129-byte check_id -> 400 validation_failed |
| 8 | pass | 8: no readiness check id/summary/remediation implies browser-reachability without an allow-listed negation |
| 9 | pass | 9: host row rh02h-ed01e9f5-real deleted 204 |
| 9 | pass | 9: ownership cleanup — zero labelled containers/volumes/networks, /var/lib/rh02h-ed01e9f5 gone |
| 9 | pass | 9: no volume exists that was not there at preflight (labelled or anonymous) |
| 9 | pass | 9: zero quasar-sess-*/quasar-pulse-*/quasar-probe-* containers remain |