# RH-02 #264 readiness-faults report

| field | value |
| --- | --- |
| role | gpu-test-amd-baseline-b134085 |
| rid | rh02h-f3ef5689 |
| gpu vendor | amd |
| gpu index | 1 |
| control image | <registry>/quasar-control-plane:dev-b134085 |
| agent image | <registry>/quasar-node-agent:dev-b134085 |
| control image source commit | b134085201a70fe5cc149dbfdf812832dbc2d28d |
| agent image source commit | b134085201a70fe5cc149dbfdf812832dbc2d28d |
| schema_migrations version | 84 |
| pass | 19 |
| fail | 54 |
| unperformed | 1 |
| verdict | fail |

## Matrix rows

| # | result | message |
| --- | --- | --- |
| preflight | pass | preflight: engine clear (foreign_quasar_projects=0, port 18080 free, /var/lib/rh02h-f3ef5689 free) |
| image-clean | pass | image-clean: agent image carries no readiness-fixture path |
| image-clean | pass | image-clean: agent binary has no harness_synthetic_ string |
| fixture | pass | fixture: readiness-fixture image built |
| fixture | pass | fixture: relay running, rule off (transparent) |
| stack | pass | stack: control plane answering /health |
| login | pass | login: admin token obtained |
| nonadmin | pass | nonadmin: user provisioned |
| seed | pass | seed: app 'rh02h-f3ef5689: app-nohome' ready (9a76fc2e-015f-4ed8-be97-ea53d66af1b8) |
| seed | pass | seed: app 'rh02h-f3ef5689: app-home' ready (2cc50239-c2df-45b6-9a7e-218b24951306) |
| baseline | fail | baseline: host never reached online+active-gate+empty-blocking within bound (pre-#264 image, or a real fault) |
| 1a | fail | 1a: runtime_endpoint never reported fail within 120s of the engine stopping (status='') |
| 1b | fail | 1b: runtime_endpoint never reported fail within 120s of the engine stopping (status='') |
| 1c | fail | 1c: runtime_endpoint never reported fail within 120s of the engine stopping (status='') |
| 1d | fail | 1d: runtime_endpoint never reported fail within 120s of the engine stopping (status='') |
| 1e | fail | 1e: runtime_endpoint never reported fail within 120s of the engine stopping (status='') |
| 1 | pass | 1: nested host row deleted 204 |
| 2a | fail | 2a: homes_root_writable never reported fail within 90s |
| 2a | fail | 2a: app-home launch got HTTP 503 code=no_host_available (want 503 host_not_ready) |
| 2a | fail | 2a: app-nohome launch got HTTP 503 (want 201) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 2a | fail | 2a: homes_root_writable did not recover to pass |
| 2a | fail | 2a: app-home launch after recovery got HTTP 503 (want 201) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 2b | fail | 2b: homes_free_space never reported fail within 90s |
| 2b | fail | 2b: app-home launch got HTTP 503 code=no_host_available (want 503 host_not_ready) |
| 2b | fail | 2b: app-nohome launch got HTTP 503 (want 201) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 2b | fail | 2b: homes_free_space is '' after removing the fill file (want warn or pass) |
| 2b | fail | 2b: app-home launch after recovery got HTTP 503 (want 201) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 3 | fail | 3: input_probe never reported fail within 90s |
| 3 | fail | 3: launch expected 503 host_not_ready, got 503 code=no_host_available |
| 3 | fail | 3: input_probe did not recover to pass |
| 3 | fail | 3: recovery launch got HTTP 503 (want 201) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 4a | fail | 4a: media_probe_gpu1 is not reported at all under the fault |
| 4a | fail | 4a: application_gpu_probe_gpu1 is not reported at all under the fault |
| 4a | fail | 4a: media_probe_gpu1 status='' (want fail) |
| 4a | fail | 4a: launch expected 503 host_not_ready, got 503 code=no_host_available |
| 4c (4a) | fail | 4c (4a): scripted host never reached online+active-gate |
| 4c (4a) | pass | 4c (4a): scripted host row deleted 204 |
| 4a | fail | 4a: probes did not recover to pass |
| 4a | fail | 4a: recovery launch got HTTP 503 (want 201) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 4a' | fail | 4a': media_probe_gpu1 not pass with only Vulkan ICD gone |
| 4a' | fail | 4a': blocking has absent entries (want 0) |
| 4a' | fail | 4a': launch got HTTP 503 (want 201) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| UNPERFORMED 4b | skip | UNPERFORMED 4b: host GPU vendor is 'amd', not nvidia |
| 5a | pass | 5a: dri_node_app_access fail carries no blocks |
| 5a | fail | 5a: dri_node_app_access in readiness_gate.blocking: absent (want 0) |
| 5a | fail | 5a: launch got HTTP 503 (want 201, dri_node_app_access must never block) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 5b | fail | 5b: audio_probe is not pass before the fault (status=''), so an unknown would prove nothing |
| 6a | fail | 6a: scripted host A is 'online' but its readiness gate never became active (state '') |
| 6b | pass | 6b: launch refused 503 no_host_available with nothing online |
| 6c/6d | fail | 6c/6d: scripted host B is 'online' with gate '' and never listed its failing check in blocking |
| 7a | fail | 7a: expected 503 host_not_ready before override, got 503 |
| 7a | fail | 7a: override PUT got 404 (want 200) |
| 7a | fail | 7a: repeat PUT status 404, audit 0 -> 0 |
| 7a | fail | 7a: audit severity '' (want warn) |
| 7a | fail | 7a: audit detail node_name='' (want rh02h-f3ef5689-real) |
| 7b | fail | 7b: blocking=0 overridden= readiness=fail |
| 7c | fail | 7c: launch got HTTP 503 (want 201, override in effect) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 7d | fail | 7d: no override was set by 7a, so a lapse cannot be shown |
| 7e | fail | 7e: override PUT got 404 (want 200) |
| 7e | fail | 7e: DELETE got 404 (want 204) |
| 7e | fail | 7e: repeat DELETE got 404 (want 204) |
| 7e | fail | 7e: 0 .cleared audit row(s) for harness_synthetic_gate (want exactly 1) |
| 7e | fail | 7e: launch after clearing got 503 (want 503 host_not_ready) code=no_host_available real_host_blocking=[] real_host={"status":"online","gate":null,"capacity_detection":"ok"} gpus=[{"gpu_index":1,"slots_total":2,"slots_reserved":0,"vram_mb_total":2048}] |
| 7f | fail | 7f: override PUT got 404 (want 200) |
| 7f | fail | 7f: the renamed check is reported as fail with blocks but readiness_gate.blocking does not list it |
| 7f | fail | 7f: blocking not empty after clearing |
| 7g | fail | 7g: got 404 (want 403) |
| 7h | fail | 7h: bad check_id got 404 (want 400 validation_failed) |
| 7h | fail | 7h: 129-byte check_id got 404 (want 400 validation_failed) |
| 8 | pass | 8: no readiness check id/summary/remediation implies browser-reachability without an allow-listed negation |
| 9 | pass | 9: host row rh02h-f3ef5689-real deleted 204 |
| 9 | pass | 9: ownership cleanup — zero labelled containers/volumes/networks, /var/lib/rh02h-f3ef5689 gone |
| 9 | pass | 9: no volume exists that was not there at preflight (labelled or anonymous) |
| 9 | pass | 9: zero quasar-sess-*/quasar-pulse-*/quasar-probe-* containers remain |