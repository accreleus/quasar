# Release completion validation — 2026-09-06

Status: in progress; no release is claimed. Scope is #129, #130, #131, #135,
#136, #145 and #146. Keep those issues open until the containing release is
published and its acceptance verified. The operator approved the #145 contract
proposal and expressly waived the separate Opus review prerequisite.

## Provisioning acceptance (#129)

Tests ran in a separate Compose project with its own control plane, database,
agent state and driver volumes on the NVIDIA validation host. The existing
production stack was not redeployed. A Docker API proxy denied all mutations
and returned an empty container listing, preventing the older test agent's
startup cleanup from reaching existing sessions. Consequently sibling EGL tests
were deliberately refused: this harness proves provisioning and reporting, not
complete host readiness or streaming.

The tested image identifies source commit
`c20193d67fae1b8086257b7bcacac0b69a069a68`, built at
`2026-09-06T10:53:40Z`. Final candidate retesting remains required; these results
cover the earlier merged provisioning implementation, not every current change.
The host driver was 610.57.04. Downloads were genuine NVIDIA HTTPS payloads,
without TLS interception or host-driver installation.

| Exercise | Observed result | Evidence / limits |
| --- | --- | --- |
| Fresh volume | Downloaded 463,025,450 bytes, extracted 44 64-bit and 27 32-bit libraries, scheduled agent restart | Completed 12:19:46 UTC; SHA-256 `b2e935c66b83bb00c0c857bc8e0ee0fd52de9286b40c9cc1eec29a7ce7eb116d` |
| Throttled download | Real setup UI moved from 50% to 60% over 15 seconds; admin host surface also inspected | Browser screenshots and API snapshots retained locally |
| SIGKILL during download | Restart recovered a 25-second-old modern lock immediately; existing crash-loop backoff delayed retry, then retry began automatically at 12:28:25 UTC | No lock/backoff file deletion; real retry completed at 12:32:16 UTC |
| Adoption after restart | Agent adopted matching 610.57.04 volume at 12:50:05 UTC without downloading again | Explicit adoption log and library counts; sibling EGL refused by test isolation |
| Insufficient storage | A disposable 64 MiB tmpfs driver volume refused provisioning before download; readiness identified 63 MiB available versus 3,072 MiB required | No host filesystem was filled; storage cause appeared in both NVIDIA readiness checks |
| Digest mismatch | Real download matched the previously observed NVIDIA hash but differed from a deliberately incorrect private-volume pin; execution was refused, scratch removed, and both readiness checks reported the integrity cause | Completed at 12:51:54 UTC; host and production driver volume untouched |
| Download failure | Controlled CONNECT proxy refusal produced explicit network/502 error in readiness | No generic host-driver replacement recommendation |

The interruption test exposed a further defect: each backoff check stored its
own waiting message as the last failure, recursively nesting retry messages and
burying the original error. The current change distinguishes pending backoff
from a new provisioning failure. Its regression proves repeated waits preserve
the original cause, attempt count and timestamp.

A real subprocess regression also holds the provisioning lock, proves another
process cannot acquire it while the holder is alive, sends SIGKILL, then proves
immediate reacquisition despite the remaining marker. The ignored child fixture
is executed by that parent test; it is not missing coverage.

## Final implementation gates

- Rust: 1,245 library, 6 binary, 3 log-convention and 1 real-process lock
  tests passed; one child fixture is intentionally ignored by ordinary discovery.
  Formatting, all-target Clippy and benchmark compilation passed. The final full run includes same-path storage repair and truthful version
  provenance in templates, driver/CUDA metadata and artifact locks.
- Go build, vet and non-database tests passed. Fresh **full** database suite
  passed with `make test-db` (`go test -p 1 -count=1 ./...`); database tests
  actually executed. Final lifecycle regressions also passed separately in the
  jobs, preparation and agent WebSocket packages.
- Database environment: two disk-backed runs encountered setup TRUNCATE
  deadlines in different packages before assertions, with PostgreSQL waiting on
  filesystem synchronization. The successful full run used a validation-only
  Docker wrapper to place only this invocation's disposable PostgreSQL data on
  a 512 MiB tmpfs, matching the repository's existing devtools approach. The
  canonical test command, fresh credentials, assertions and cleanup were unchanged.
- Web: 231 files / 2,933 tests, API schema drift, type checks and build passed.
  Sources and image preparation states were visually checked at desktop/mobile
  sizes. Site: 28 tests and the 216-page build passed.
- Preflight: 376 passes, 1 warning, 0 failures. The warning is missing host
  ShellCheck. Containerized ShellCheck found all 31 canonical DX scripts clean;
  enrollment/build/Compose warnings were compared with the starting commit and
  none were introduced. Existing warnings are recorded rather than suppressed.
- `git diff --check` passed. Additive protocol contracts are pushed in
  [protocol PR #18](https://github.com/accreleus/quasar-protocol/pull/18),
  commit `128cbae`; this is not a claim that the PR has merged.

These automated gates do not replace the live acceptance below.

## Candidate image and isolation checks

Candidate source `617db41248aefef2dc601ee00ceaceff963e473a` is pushed in
[PR #147](https://github.com/accreleus/quasar/pull/147). The runtime image passed
139/139 GPU-enabled contract checks and the control-plane image passed 23/23
checks (162 total). Runtime image ID is
`sha256:7ef38b34c98be739a6f1713530271b975487057df004405bdce02750704c7a9d`.
Its startup reports `dev`, the correct source commit and build time
`2026-09-06T13:35:26Z`, appropriate for this untagged source build. This does not
replace verifying a published release's explicit version stamp.

A separate control/database/agent stack started on that candidate. Its health
listener and Docker healthcheck use a private port to avoid sharing the existing
host-networked agent's default port. The test configuration initially omitted
that override and was corrected before recording health acceptance.

- **#130:** automatic discovery adopted the matching driver volume and passed
  real sibling EGL startup. An explicit override to a different existing empty
  directory produced a failed `nvidia_driver_mount` readiness check identifying
  the same-directory validation failure. Pointing the override to the actual
  test volume restored sibling EGL success. Driver replacement was not needed.
  API snapshots and positive resolution logs are retained locally. This check
  exercised readiness, not a negative app-launch attempt.
- **#146:** 11/11 bounded real-Docker assertions passed. Two actual candidate agent
  processes held distinct ownership leases; one removed its own two orphan
  fixtures while preserving the other agent's running fixture, legacy/unowned
  and malformed fixtures, and an unrelated name. Duplicate shared state and
  malformed identity refused startup before cleanup. All disposable containers
  were removed. These were sleep-container surrogates and explicitly injected
  malformed Docker inspection data, not a live media-survival experiment.
- **#145, in progress:** with no feature flags, the adopted official Steam image
  scheduled preparation, and the host acknowledged production/consumption enabled.
  Actual user-share mount probing reported full copy. Disabling the source reached
  acknowledged disabled state. The first warm-up failed at audio pipeline READY;
  diagnosis and successful preparation/consumption acceptance remain outstanding.
  Live screenshots also exposed preparation-cell clipping and technical fallback
  wording; those are being corrected before final candidate validation.

## Remaining release acceptance

- Final candidate image contracts and provenance (#135).
- Live verified driver path override and wrong-path refusal (#130).
- Isolated two-agent restart/cleanup survival (#146).
- Fresh setup through real signaling with persisted origin (#131).
- Older-edge plan/apply behavior on the final control plane (#136).
- Complete #145 policy, lifecycle, UI, storage, account isolation/sanitization,
  disable/re-enable and cold-versus-prepared real Steam measurements.
- Required component gates, preflight, documentation and release evidence.
- Main promotion, v0.2.4 publication and verified live update. The operator
  explicitly authorized autonomous promotion/publication after validation.

Raw captures remain in the worktree's ignored
`.diagnostics/release-completion/` directory. Credential files in that directory
are private and must never be attached. Publish only sanitized evidence.
