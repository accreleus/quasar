# RH-07 acceptance map

Written for #409 (D19); reconciled 2026-10-01 against the newest hardware reports. Sources:
#393–#437 and their comments, `docs/rh07/2026-09-28-decisions.md` (D1–D23), the
specification (#390) and `testdata/engine-profiles/profiles.json`. Citations are an issue
comment or a commit. Hosts are named by role: the AMD test host, the NVIDIA test host, and
the rootless lab VM (SELinux enforcing, read-only `/usr`, per D23), which ran with either the
NVIDIA card or the host's AMD iGPU passed through.

**Candidate coverage.** Every hardware row below predates the final candidate: the #429
and #432 fixes, the dependency updates and the Go 1.26 toolchain landed afterwards. The rows
are real evidence for the code paths they ran; they are not a hardware pass of the final
build.

## 1. Profile matrix (D19)

Required per cell: a real session with input, audio and microphone (**Session**); an update
applied from the console (**Update**); a machine reboot after which everything returns
(**Reboot**; a container restart does not count); console mode on one host per engine
(**Console**); a bench run posted (**Bench**).

Evidence keys used in the tables:

| Key | Source |
|---|---|
| E1 | #409 [5900734046](https://github.com/accreleus/quasar/issues/409#issuecomment-5900734046): full input and microphone on rootful Docker (AMD, NVIDIA); keyboard and microphone on rootless Docker (AMD, NVIDIA) |
| E2 | #428 [5914198622](https://github.com/accreleus/quasar/issues/428#issuecomment-5914198622): every gamepad event reaches the app on rootless Docker (AMD, NVIDIA) with the published 2026.09.30 images |
| E3 | #409 [5912107407](https://github.com/accreleus/quasar/issues/409#issuecomment-5912107407): AMD rows on the lab VM (rootless Podman) |
| E4 | #399 [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911): NVIDIA engine-mode table |
| E5 | #405 [5873588293](https://github.com/accreleus/quasar/issues/405#issuecomment-5873588293): rootless Podman install, update, reboot (NVIDIA) |
| E6 | #409 [5892348346](https://github.com/accreleus/quasar/issues/409#issuecomment-5892348346): microphone, rootless Podman (NVIDIA) |
| E7 | #401 [5891242775](https://github.com/accreleus/quasar/issues/401#issuecomment-5891242775): full per-button input replay, rootless Podman (NVIDIA) |

### Docker rootful

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | PASS (E1) | PASS: #402 [5877723916](https://github.com/accreleus/quasar/issues/402#issuecomment-5877723916) | GAP | GAP | GAP |
| NVIDIA | PASS over CDI (E1, E4); the `--gpus` fallback fails on a CUDA-only host (#413) | PASS (E4) | PASS (E4) | PASS: #395 [5884298398](https://github.com/accreleus/quasar/issues/395#issuecomment-5884298398) | PASS: run `3e38e6e3` (E4) |

### Docker rootless

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | PASS (E1, E2) | GAP | GAP (container restarts only) | GAP | GAP |
| NVIDIA | PASS (E1, E2, E4) | PASS (E4) | PASS (E4) | PASS: #407 [5890578752](https://github.com/accreleus/quasar/issues/407#issuecomment-5890578752) | PASS: run `9384a95d` (E4) |

### Podman rootless (Fedora family)

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | PASS (E3): gamepad sequence identical to the reference, 30/30 keys, microphone RMS 2902 | GAP: E3 has no update row | PASS: VM reboot (E3) | PASS: HDMI picture, mouse, audio, DDC (E3; #407 [5912107863](https://github.com/accreleus/quasar/issues/407#issuecomment-5912107863)); no physical keyboard attached | PASS with no baseline: XFCE 1080p60 soak, `no_comparable_runs` (E3) |
| NVIDIA | PASS (E4, E6, E7; audio: #411 [5876349147](https://github.com/accreleus/quasar/issues/411#issuecomment-5876349147)) | PASS (E5) | PASS (E5) | PASS: #407 [5890937657](https://github.com/accreleus/quasar/issues/407#issuecomment-5890937657) | PASS: run `9cf88ede`, "result: clean" (#399 [5876277652](https://github.com/accreleus/quasar/issues/399#issuecomment-5876277652)) |

The AMD Podman bench is an XFCE soak, not a Steam game.

### Not required

- **Podman rootful (Fedora):** experimental. A fresh Quadlet install reached `online, owned`
  (#406 [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850));
  no session, update, reboot or console evidence; LAN traffic to the console port is
  dropped under firewalld (#416).
- **Ubuntu 24.04:** no RH-07 hardware run on any engine. Docker rootful's `supported` label
  predates RH-07; the other rows are experimental.

## 2. User stories (spec #390)

| # | Story | Status | Evidence |
|---|---|---|---|
| 1 | Rootless engine; a compromise can't take the machine | Met | Recipe revision 3: #402 [5873589343](https://github.com/accreleus/quasar/issues/402#issuecomment-5873589343); ACL-only device grants: #400 [5870987736](https://github.com/accreleus/quasar/issues/400#issuecomment-5870987736) |
| 2 | Podman install and update | Met | E5 |
| 3 | Rootless Docker install and update | Met | #406 [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850); update on NVIDIA (E4) |
| 4 | Rootful Docker and Unraid unchanged, with less privilege | Met | #402 [5877723916](https://github.com/accreleus/quasar/issues/402#issuecomment-5877723916); Unraid's path is unchanged by design (D4) |
| 5 | Quick start and enrollment detect engine and mode | Met | #396 [5871843388](https://github.com/accreleus/quasar/issues/396#issuecomment-5871843388), #406 [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850) |
| 6 | Quick start shows supported, experimental, unsupported | Met | #406 [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850) |
| 7 | One idempotent host-preparation command | Met | #400 [5870987736](https://github.com/accreleus/quasar/issues/400#issuecomment-5870987736) |
| 8 | Host preparation touches only `/etc` | Met | #400 [5870987736](https://github.com/accreleus/quasar/issues/400#issuecomment-5870987736) |
| 9 | Dedicated `quasar` account | Met | #400; E5 |
| 10 | Quasar never runs as root or escalates at run time | Met | #397 [5872048545](https://github.com/accreleus/quasar/issues/397#issuecomment-5872048545); the agent's `label=disable` is an accepted confinement exception (D17, #402 [5884609826](https://github.com/accreleus/quasar/issues/402#issuecomment-5884609826)) |
| 11 | Controller, keyboard and mouse work rootless | Met | Full replay on rootless Podman (E7, E3); full input on rootful Docker (E1); rootless Docker gamepad (E2) |
| 12 | NVIDIA encode and GPU games on every engine mode | Partly met | CDI passes on all three required NVIDIA rows; the rootful `--gpus` fallback on a CUDA-only host fails (#413) |
| 13 | Session audio and microphone on every engine mode | Met | Microphone: E1 (Docker, both vendors), E6 and E3 (Podman, both vendors) |
| 14 | Home files owned by the Quasar user | Partly met | Rootless Podman: #404 [5876278031](https://github.com/accreleus/quasar/issues/404#issuecomment-5876278031); rootless Docker keeps subordinate IDs, accepted with a readiness warning: #404 [5884610200](https://github.com/accreleus/quasar/issues/404#issuecomment-5884610200) |
| 15 | Console updates work on every engine mode | Met | One host per required engine mode (matrix Update column) |
| 16 | A rootless or Podman machine returns after reboot | Met | E5, E3, E4 |
| 17 | Readiness names engine and mode, explains a missing capability | Met | #393 [5870715357](https://github.com/accreleus/quasar/issues/393#issuecomment-5870715357), #396 [5871843388](https://github.com/accreleus/quasar/issues/396#issuecomment-5871843388) |
| 18 | A missing permission gives an exact-fix readiness check | Met | #401 [5878347040](https://github.com/accreleus/quasar/issues/401#issuecomment-5878347040) |
| 19 | Media reachability tests the real path | Met | #403 [5877723197](https://github.com/accreleus/quasar/issues/403#issuecomment-5877723197) |
| 20 | GPU fault (Xid) messages when allowed, a clear skip otherwise | Partly met | The skip path is live (#402 [5873589343](https://github.com/accreleus/quasar/issues/402#issuecomment-5873589343)); no live Xid fault has exercised the shown path |
| 21 | Console mode on owned installs | Met | #395 [5884298398](https://github.com/accreleus/quasar/issues/395#issuecomment-5884298398) |
| 22 | Console mode on rootless and Podman | Met | Console column: rootless Docker and rootless Podman on both vendors' evidence |
| 23 | Console audio through the host's PipeWire | Met | #407 [5912430462](https://github.com/accreleus/quasar/issues/407#issuecomment-5912430462): Quasar's Pulse socket bound, HDMI playback audible with ALSA owned by PipeWire (rootless Podman, NVIDIA) |
| 24 | DDC monitor control rootless | Met | #407 [5890578752](https://github.com/accreleus/quasar/issues/407#issuecomment-5890578752), [5890937657](https://github.com/accreleus/quasar/issues/407#issuecomment-5890937657), [5912107863](https://github.com/accreleus/quasar/issues/407#issuecomment-5912107863) |
| 25 | Documented Podman Quadlet install | Met | #406 [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850) |
| 26 | Reinstall plus dump restore, rootful to rootless | Partly met | A v0.3.0 dump restored and a Steam session streamed (#380, landed `da0ffdca`); not into a fresh rootless install, no v0.2.x dump, and the mid-restore crash was tested in memory only |
| 27 | Docs say a rootful socket is root-equivalent | Met | #406 [5884610608](https://github.com/accreleus/quasar/issues/406#issuecomment-5884610608) |
| 28 | SELinux-enforcing hosts work without relaxing SELinux | Met | Every rootless run stayed `Enforcing`; sessions stay confined; the agent's `label=disable` per D17 |
| 29 | One behavioural suite across every engine mode | Met | #408 [5893020059](https://github.com/accreleus/quasar/issues/408#issuecomment-5893020059), in CI on all four modes; its Podman findings are open (#424, #425, #426) |
| 30 | Each required profile proven on hardware before `main` | Partly met | Every NVIDIA row passes. AMD gaps: Docker rootful reboot, console, bench; Docker rootless update, reboot, console, bench; Podman rootless update |

Met: 1–11, 13, 15–19, 21–25, 27–29 (**23**). Partly met: 12, 14, 20, 26, 30 (**5**). Not
evidenced: none.

## 3. Open gaps

Each needs a run, or the owner's named acceptance under #409.

1. **AMD, Docker rootful:** reboot, console mode, bench. (#409 asks for console on one host
   per engine; NVIDIA covers that.)
2. **AMD, Docker rootless:** update, machine reboot, console, bench.
3. **AMD, Podman rootless:** update from the console; console run with a physical keyboard;
   a Steam bench with a baseline.
4. **Final candidate:** no hardware run of the release build itself (see Candidate coverage).
5. **#413:** the rootful `--gpus` fallback on a CUDA-only NVIDIA host (CDI works).
6. **Known Podman defects, need a release decision:** #425 (a crash-looping service can
   restart after an explicit stop), #426 (Podman creates a missing bind source), #424
   (restart-policy update unsupported on Podman 4.9).
7. **Story 26:** restore into a fresh rootless install, a v0.2.x dump, a live mid-restore
   crash.
8. **Story 20:** no live Xid fault on the shown path.
9. **Podman rootful and Ubuntu:** not required; labels stay experimental (#416).
10. **#429 and #432:** fixed in code, not yet verified on hardware.

**Open defects touching these rows:** #410, #412, #413, #414, #416, #418, #421, #422, #424,
#425, #426, #429, #432, #433, #434.
