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
`2026-09-06T10:53:40Z`. These results
cover the earlier merged provisioning implementation; the entire destructive
failure campaign was not repeated on the final runtime candidate.
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
  jobs, preparation and agent WebSocket packages. After the live origin-cache
  correction at `5122713`, Go build/vet/tests, focused settings/access/origins
  database tests and another fresh full canonical database suite all passed.
- Database environment: two disk-backed runs encountered setup TRUNCATE
  deadlines in different packages before assertions, with PostgreSQL waiting on
  filesystem synchronization. The successful full run used a validation-only
  Docker wrapper to place only this invocation's disposable PostgreSQL data on
  a 512 MiB tmpfs, matching the repository's existing devtools approach. The
  canonical test command, fresh credentials, assertions and cleanup were unchanged.
- Web: 231 files / 2,933 tests, API schema drift, type checks and build passed.
  Sources and image preparation states were visually checked at desktop/mobile
  sizes. Site: 28 tests and the 216-page build passed.
- Preflight: 377 passes, 1 warning, 0 failures. The warning is missing host
  ShellCheck. Containerized ShellCheck found all 31 canonical DX scripts clean;
  enrollment/build/Compose warnings were compared with the starting commit and
  none were introduced. Existing warnings are recorded rather than suppressed.
- `git diff --check` passed. Additive protocol contracts merged in
  [protocol PR #18](https://github.com/accreleus/quasar-protocol/pull/18),
  merge commit `a47d3821d4968eece5b1937455c1cf6810f66b86`. The parent pin
  `128cbae` remains reachable from protocol main. Final pre-merge preflight
  again reported 377 passes, one missing-host-ShellCheck warning and no failures.

These automated gates do not replace the live acceptance below.

## Candidate image and isolation checks

The first candidate source `617db41248aefef2dc601ee00ceaceff963e473a` was pushed in
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
  API snapshots and positive resolution logs are retained locally. A later real
  GPU app request was accepted asynchronously as assigned, then failed within
  about one second with the exact invalid-path diagnostic; no app container was
  created. Requiring HTTP rejection at creation would misread this asynchronous
  contract. Cleanup restored automatic discovery and readiness.
- **#146:** 11/11 bounded real-Docker assertions passed. Two actual candidate agent
  processes held distinct ownership leases; one removed its own two orphan
  fixtures while preserving the other agent's running fixture, legacy/unowned
  and malformed fixtures, and an unrelated name. Duplicate shared state and
  malformed identity refused startup before cleanup. All disposable containers
  were removed. These were sleep-container surrogates and explicitly injected
  malformed Docker inspection data, not a live media-survival experiment.
- **#145:** default preparation scheduled automatically for the adopted official
  Steam image. Final runtime preparation and both native reflink and Unraid
  user-share copy consumption succeeded. Measured results and isolation checks
  are below. The initial audio READY failure no longer blocks final preparation;
  the clipped status layout and technical fallback wording were corrected and
  visually checked at desktop/mobile sizes.

## Follow-up live findings

Final runtime/control candidates at `9ef2875a` again passed 139 and 23 image
contract checks. The final runtime image is
`quasar-node-agent:20260906-1358-release-completion-145-final`, image ID
`sha256:e79e1b0d534200577f445d49271255a4931b4fadc1fdd5333bec839da4cd9662`,
source `9ef2875a9a3a4cc0a87f161e172d0f0ad1ec5b90`. Fresh preparation published a
matching template; the last scan counted 23,701 files and 2,759,891,468 bytes,
with metadata correctly reporting `dev`. The later control-only origin fix is
`5122713`, deployed as `quasar-control-plane:20260906-1415-release-completion-origin-final`.
Its Docker healthcheck was healthy and `/health` returned both service and
database `ok`; the agent was not recreated for this control-only correction.

The deployed older-edge test inserted one uniquely identified synthetic cache row
with the installed schema and an older build time in the isolated database.
Fleet and host apply requests, with and without force, all returned the exact
initial `release_not_offered` rejection. No apply history or host scheduling
state changed. The actual UI showed Older than installed without an update
control. The test restored the channel and removed exactly its one cache row.
This does not claim GitHub discovery or registry validation. A mobile layout
overlap exposed by that capture was corrected and visually checked, with the full
2,933-test web gate passing.

An initial cold/prepared measurement was **discarded**: its test-created app
omitted GPU access, exited, and the first harness counted placeholder frames.
Screenshot and container-log review caught the false result. Corrected measurements
require GPU mapping, a real app-surface presentation, a live app container through
completion, advancing decoded frames, and screenshot inspection. The failed
run's sessions were stopped and settings restored; its timings are not evidence
of Steam performance.

The actual setup Continue button exposed a separate #131 defect: saving the
origin succeeded, but immediate verification read the old two-second resolver
cache and displayed a failure. The saved database value alone was therefore not
a successful wizard acceptance. The fix invalidates the existing shared resolver immediately after commit.
Authenticated add/read and clear/read database regressions pass without sleeps;
disabling only the invalidation makes the new regression fail. The complete fresh database gate passed again. At 14:16:58 UTC, a controlled
retest cleared only the isolated allowed-origin list through its API, then
clicked the actual browser Continue button. It advanced to Host & GPU check;
one settings PATCH saved exactly `http://127.0.0.1:19181`, registration remained
closed, and immediate access-check reported the database-backed origin. Both
origin/public-base environment overrides were absent. This was an explicitly
reset acceptance run, not a claim that the database had never been used. The
subsequent real Steam sessions also established signaling and decoded media
through that address without restoring the overrides.

## Real Steam preparation and storage measurements (#145)

All three accepted sessions used H.264, 1920×1080 at 60 fps and 8,000 kbit/s on
profile `1080p60`, with distinct new users. The native cold/prepared pair is in
`steam-first-launch-2026-09-06T14-17-29.920Z`; the user-share prepared run is in
`steam-first-launch-2026-09-06T14-25-09.383Z`. Each retained a live app container,
advanced decoded frames, logged an actual app-surface presentation and finished
without browser errors. Final screenshots were independently inspected: all
three display the Steam sign-in screen with readable text and QR code, not the
compositor placeholder. Cleanup reports contain no failures.

| Mode | Home seeding | App-container start to first app presentation | Decoded frames at end |
| --- | --- | --- | --- |
| Cold, preparation disabled | No seed confirmed | 36,652 ms | 7,043 |
| Prepared, native cache filesystem | Reflink, 543 ms | 4,762 ms | 7,024 |
| Prepared, Unraid user share | Full copy, 37,872 ms | 15,421 ms | 4,836 |

These are individual observations, not a benchmark distribution or a universal
speedup promise. The app-presentation timer excludes the preceding home copy.
The user-share run took about 39.9 seconds merely to produce its first video
frame, followed by the app presentation; a prepared template on a filesystem
requiring full copy can therefore make launch slower. The UI truthfully reports
that copy fallback. Initial browser video frames are not used as proof that
Steam is ready, because they may be placeholders.

The final template is for the adopted Steam `2026.09.02` digest
`sha256:586c67412921821287a5cf68c390aebbaf00fc750ce390170f219b465460bde5`.
`template-final-account-scan.json` reports zero account names, zero Accounts
blocks, zero special files and zero root-owned entries across 23,701 files,
2,863 directories and 2,569 symlinks. This is a bounded structural scan of the
prepared template, not proof about arbitrary future Steam data formats.
`home-isolation-result.json` records distinct template/home file identities,
independent markers, and a controlled change to one home's Steam script that
changed neither the template nor the other home. The changed file was restored.

The existing-home policy smoke at 14:39:50 UTC also passed: disabling reached
acknowledged revision 15, re-enabling reached revision 16, template identity
remained unchanged, and decoded frames advanced from 855 to 1,900 to 2,957.
The same app process stayed alive, the current session did not seed the existing
home again, and cleanup reported no failures. Off/on screenshots show readable
Steam sign-in. An earlier harness assertion incorrectly included an old session's
seed log; the accepted run requires the exact current session ID. Its initial
pre-toggle capture still included the startup overlay, so only the later
screenshots are claimed as visible Steam evidence.

## Actual-stream ownership survival (#146)

The earlier 11/11 fixture assertions were supplemented with a genuine Steam
session. Between 14:18:14 and 14:18:26 UTC, another final-runtime agent started
with fresh ownership/state, no network, no GPU and the real Docker socket.
The existing session `7ef8d860-0259-4fbe-934c-08b1ca0eaf97`, container
`71b9dbdf6470e0d869d2324b4aa7cd5b65f1256fc622945b7ffc39769142a4ec`, remained
running across that startup. The extra agent reached the code after the awaited
orphan sweep; no sweep failure was logged. Only that extra agent was removed,
and its exact validation-label cleanup query returned no containers. The cold
measurement continued to its final Steam screenshot and 7,043 decoded frames.

The supplemental harness initially expected a zero-removal log line which the
source emits only for nonzero removals. Its original failed assertion is kept;
`146-real-stream-assessment.json` records the corrected, source-backed reading
of the startup logs and exact container states. This proves real session
container survival, while the separate browser measurements establish media
continued; it is not a frame-by-frame no-jitter guarantee.

## Remaining release acceptance

- Review the final release diff and required gates after any further changes.
- Promote to main, publish v0.2.4 and
  verify the published manifest, all three image tags and tagged agent identity.
- Validate deployment/update using those published artifacts and publish the
  matching public site. The operator explicitly authorized autonomous promotion
  and publication after validation.
- Close #129, #130, #131, #135, #136, #145 and #146 only after the containing
  release is published and accepted. Candidate checks do not prove future
  published-image provenance; earlier provisioning campaign scope remains as
  stated above.

Raw captures remain in the worktree's ignored
`.diagnostics/release-completion/` directory. Credential files in that directory
are private and must never be attached. Publish only sanitized evidence.
