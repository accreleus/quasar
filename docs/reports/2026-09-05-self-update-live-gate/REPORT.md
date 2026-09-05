# Self-update live gate — 2026-09-05 (#119)

Live exercise of the self-update feature (#104, tickets #105–#118) on the `gpu-test` role
host, driven by the coordinating session while the operator was away. No GHCR push and no
`main` promotion were involved: publishing on tag push (#108) and cutting a real prerelease
(#109) remain to be exercised after the operator promotes `develop` to `main`. Everything
below used images built on the host from `develop` commits and pushed to a throwaway local
registry (`localhost:5000/quasar`), with `platform_releases` rows inserted by hand in place of
the GitHub Releases the detector will read once real releases exist.

## What was proven

| step | commit under test | result |
|---|---|---|
| Component identity (#107) | 1117f0b | control plane serves version/commit/built_at/schema; agent reports commit, built_at, install_mode, updater_present; host drawer shows them. Two live defects fixed first (agent build args in redeploy.sh; self-container discovery via mountinfo). |
| Steam regression after identity deploy | 19c08ad | 9865 frames decoded at 2560x1440, 60 fps, clean DELETE. |
| Detection job (#110) vs the real GitHub API | 63932be | ran on demand; `releases_seen=0` — the repo has no GitHub Releases yet (correct). |
| Updater acceptance (#115) | 63932be | 422 `namespace_rejected` for a foreign namespace; 400 `invalid` for a self-target; apply of the agent by digest from the local registry succeeded in ~6 s; `.env` rewritten, `.env.prev` kept. Start-order defect fixed (agent registered before the updater existed). |
| Per-host apply (#116) | CP 60cee82, agent 63932be → 60cee82 | `waiting_sessions` with `sessions_remaining=1` while a Steam session streamed 8776 frames untouched; proceeded when the session ended; new agent's register resolved the attempt `succeeded`; cordon restored; audited `platform.apply.host`. |
| Fleet apply (#117) | CP 5e494dd → 638d654, agent → 638d654 | 409 on a second POST; control-plane self-apply recreated the control plane via its own updater; **defect**: the source-built stack's root-owned TLS volume made the uid-1000 registry image crash-loop for 15 min (updater reported `unhealthy`, no restore by design). After a manual chown the new control plane adopted the run, resolved its own attempt by identity match, then applied the host. Run `succeeded`. Fixes: control-plane install-mode eligibility (#117 follow-up) and uid parity + one-shot volume chown (#115 follow-up). |
| Reload toast (#117) | bundle 5e494dd vs served 638d654 | "Quasar was updated — This page is running the previous build. Reload" shown (screenshot). |
| Revert (#118) | agent 638d654 → 60cee82 | `kind=revert` succeeded in ~4 s; reverted-from digest recorded; audited `platform.revert.host`. |
| Session survival across a control-plane recreate | 638d654 | **NOT MET, pre-existing**: the agent stops every session when its control-plane WebSocket drops (`connection ended: signalling session … to stop`), reconnecting 2 s later. Reproduced with a real invite-registered user. Filed as its own bug; the fleet run now drains the fleet before its control-plane step (#117 follow-up) so an update never kills sessions silently. |

## Final state and gates (develop `730d03c`)

- Host restored to a plain source install (`redeploy.sh nvidia develop`): identity `730d03c`, schema 75, agent `install_mode=source`, `updater_present=true`; control plane and agent both read `install_mode_source` on the Releases tab, so nothing is offered for apply (correct for a source install); fixture rows and apply history removed from the host database.
- Final Steam gate: 10580 frames decoded at 2560x1440, 60 fps, clean DELETE (`steam-regression-final.log/.png`).
- Test suites on `730d03c`, all green: `make test-go` (unit + OpenAPI drift), `make test-rust` (fmt, clippy -D warnings, tests), `make test-web` (typecheck, 2860 tests, build), DB-backed `go test -p 1 ./...` on a fresh ephemeral Postgres (40 packages ok).

## Not exercised here (needs `main` promotion)

The tag-push publish lane (#108) and cutting a real prerelease with `make release` (#109), and with them the stable channel's first real detection from a GitHub Release. A source-built control plane can no longer be moved by the console (by design after this gate); the fleet path was proven on registry-installed components.

## Files
- `redeploy-summaries.txt` — every REDEPLOY summary line of the day
- `regression-gate-after-107.log`, `steam-regression-after-107.png`
- `host-apply-session-gate.log` — the session that streamed through the per-host apply's drain
- `attempts-after-116.json`
- `releases-tab-110.png`, `releases-after-fleet-apply.png` (with the reload toast)
- `fleet-apply.log`, `cp-survival.log`
