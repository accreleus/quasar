# RH-01 #235 — GPU helper runtime acceptance

The GPU helper migration passed the agreed AMD/NVIDIA acceptance on the isolated runtime branch. NVIDIA lib32 discovery and sibling EGL now use the Quasar-owned API lifecycle. The NVIDIA userspace extractor remains an agent child process, and host-injected drivers retain precedence. Application-container CLI migration remains separate.

## Candidate

- Source: `7fc0b58142415c79c2cdb641628c5902a00c9a7a` on `feature/rh-01-runtime-api`.
- Runtime image: `quasar-node-agent:20260915-1205-rh01-235`.
- Image ID on both test hosts: `sha256:5add43038cb90b0c800134496e422c2df1f9cc8fcaa8b229c987291092e2b7f9`.
- Built locally with `deploy/build-images.sh runtime --git-ref 7fc0b58 --toolchain registry`, the existing pinned base and unchanged image contract. No Actions image build, release tag, or promotion to develop/main.
- Base digest: `sha256:ee45e721c7f7310daad9d2dbb3ab7831385155d081b3da460a679dbadc0f4b6a`; toolchain content tag: `25657c2119a40001`.

Only the intended agents were recreated, with their audio image pinned to the same candidate. Control planes, databases, identity and persistent user storage were retained.

## Checks and review

`make verify`: **428 passed, 0 warnings, 0 failures**. `make test-rust`: formatting and clippy passed; **1,427 tests passed**, with **10 explicitly ignored live/integration tests** in the default suite. The applicable ignored helper tests were run separately below. The staged privacy scan and diff whitespace check passed.

The unchanged image contract passed **147 assertions without GPU attachment** during the build, then **144 assertions with GPU attachment** on each host. These modes evaluate different conditional assertion sets; skipped GPU checks in the build are not counted as hardware evidence.

Behavioral tests at `RuntimeClient` cover ownership and request adoption, named-volume versus daemon-host bind identity, realized read-only mode, NVIDIA device requests, security, inherited versus conflicting environment values, final output, and cleanup. Regressions cover lost create/start replies without duplicate mutation, safe Docker bind-option normalization, recovery of NVIDIA helpers when generic recovery fails, and replay of the pre-migration audio journal fingerprint. Existing shared lifecycle tests cover cancellation, deadlines and cleanup retry.

The primary agent reviewed standards/correctness against captured diffs; an independent specification reviewer reviewed the approved #235/#228/#229 requirements and correction delta. Findings were corrected and re-reviewed: legacy audio fingerprints, inherited environment handling, uncertain lib32 retries, bind-option checks, recovery starvation, and live-test identity isolation. No unresolved code/specification finding remained.

The lib32 helper has a 20-second in-container timeout. Image preparation has a 60-second bound; subsequent lifecycle operations retain their runtime deadlines. This is a bounded sequence, not a claim that the entire sequence always completes within the old single CLI invocation's 60 seconds.

## Real session comparison

Each candidate run used H.264 at 720p60, an application-generated SMPTE pattern and application-generated tone. The browser harness required advancing video, visible content and received/playing audio. Success screenshots were captured and visually inspected. The actual encoder was verified in the session's agent logs. Teardown measured from acknowledged stop until the exact app/audio container IDs were absent and the session socket root was empty; every run satisfied the 35-second bound.

| Target/path | Actual encoder | Baseline teardown | Candidate teardown | Candidate browser result |
|---|---|---:|---:|---|
| AMD VA | `vah264enc` | 11.636 s | 10.915 s | Visible pattern; audio passed |
| AMD Vulkan | `vulkanh264enc` | 11.719 s | 10.958 s | Visible pattern; audio passed |
| NVIDIA, installed driver volume | `vulkanh264enc` | 11.238 s | 11.498 s | Visible pattern; audio passed |
| NVIDIA, freshly provisioned volume | `vulkanh264enc` | — | 11.512 s | Visible pattern; audio passed |

These are functional acceptance measurements, not a performance-improvement claim. Candidate mean luma was 127.26–127.48; screenshots show rendered content rather than decoded black frames. The browser audio check recorded advancing packets, decoded duration and energy plus live, unmuted playback.

Screenshots: [AMD VA](rh01-235/amd-va.png), [AMD Vulkan](rh01-235/amd-vulkan.png), [NVIDIA fresh volume](rh01-235/nvidia-fresh.png).

Additional input sessions passed on both vendors: mouse dragging visibly selected terminal text (mean frame delta 6.49 AMD / 6.51 NVIDIA against idle deltas below 0.04); keyboard events sent `quasar` plus Enter through the stream, and an independent readback from the application container confirmed exactly `quasar`. The rendered terminal displayed the success result. Teardown took 10.906 s on AMD and 11.383 s on NVIDIA, with no remaining containers or socket paths. Both temporary input catalog entries were removed. Screenshots: [AMD input](rh01-235/amd-input.png), [NVIDIA input](rh01-235/nvidia-input.png). [Sanitized machine-readable results](rh01-235/acceptance-summary.json) include media, input, cleanup and final host state.

Tested hardware/runtime:

- AMD Ryzen 5 4500U integrated GPU (`1002:1636`), selected render node `/dev/dri/renderD128`; Docker 29.8.0/API 1.56, kernel `7.0.0-31-generic`.
- NVIDIA RTX 5090, driver `610.57.04`; Docker 29.7.2/API 1.55, kernel `7.1.7-200.fc44.x86_64`.

AMD baseline source was `65a53b0b62f1885311ea601d7dc88af2dc3f1a72`; NVIDIA baseline source was `81d5f078e4847162912834787929c618944b2c0d`. Application images were held constant within each before/after comparison; agent and audio images used the candidate.

## NVIDIA provisioning and negative evidence

The installed volume was first reused successfully. The idle agent was then recreated with an explicitly named, empty, uniquely owned disposable driver volume. Its normal startup detected missing graphics userspace, downloaded the matching installer, ran **extract-only**, populated the volume, and restarted through its normal policy. It produced **44 lib64 and 27 lib32 libraries**, layout version 2, and returned healthy. No host kernel driver was installed or changed.

Downloaded installer SHA-256: `b2e935c66b83bb00c0c857bc8e0ee0fd52de9286b40c9cc1eec29a7ce7eb116d`. The fresh manifest matched the installed volume's driver version and installer digest.

The locally compiled ignored helper test ran inside the candidate with an isolated test ownership identity. On this CUDA-only host, the public `/usr` lib32 probe correctly found no host libraries; its completed journal recorded **exit 1**, distinguishing absence from an unavailable runtime. The fresh volume's ELF32 payload was checked, and the owned sibling EGL helper returned **exit 0**. Both helper journals reached `Completed`, and both exact container IDs were confirmed absent. The subsequent fresh-volume session provided the rendering/audio/teardown evidence above.

The live host-path nonce test passed on **both hosts**, including matching-directory success, wrong-directory rejection, absent bind-path rejection without creating that path, marker removal and helper recovery. Four focused fixture tests also ran inside the NVIDIA candidate: empty/current/stale/corrupt manifest classification, the provisioning/host-driver precedence decision table, reviewed-digest mismatch refusal, and stale-version/wrong-ELF-class lib32 rejection followed by matching ELF32 success. These were disposable filesystem fixtures; the installed driver volume was not corrupted to manufacture failures. Device/security/lost-reply faults were exercised through the runtime-interface fixtures, not destructive host-wide injection.

## Final state and limits

Both test hosts remain online and healthy on the exact candidate image with zero active test sessions. AMD's original VA setting is restored. NVIDIA's installed driver volume is restored; the unused disposable volume, remote transfer/test payloads and NVIDIA-only acceptance catalog entry were removed. The local test stack and its existing test app remain available.

Intel's shipped iHD/Mesa packaging, VA default and Vulkan opt-in were preserved. **Intel hardware validation is pending**, with the [external procedure](rh01-intel-external-validation.md) prepared for i3-12100, Arc A-series and Battlemage testers. This report does not certify other GPU models, Unraid versions, Podman/rootless configurations, or codecs beyond the tested paths.

Sanitized results and screenshots are committed here. Full logs, source/image records, browser reports, exact teardown evidence and private inspections are retained in the gitignored `.diagnostics/rh01-235/` directory. Next implementation increment: #236; no work on it is included here.
