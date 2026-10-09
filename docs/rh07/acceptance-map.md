# RH-07 acceptance map

Written for #409 (D19); reconciled 2026-10-02 against the newest hardware reports and updated
2026-10-09 to close the milestone. Sources:
#393–#437 and their comments, `docs/rh07/2026-09-28-decisions.md` (D1–D23), the
specification (#390) and `testdata/engine-profiles/profiles.json`. Citations are an issue
comment or a commit. Hosts are named by role: the AMD test host, the NVIDIA test host, and
the rootless lab VM (SELinux enforcing, read-only `/usr`, per D23), which ran with either the
NVIDIA card or the host's AMD iGPU passed through.

**Console mode was rebuilt after these runs.** The direct-display console (#453, ADR 0009)
replaced the agent-drawn console on 2026-10-06. It is proven on NVIDIA under rootful Docker
(E9) and rootless Podman with SELinux enforcing (E10). The other Console cells are evidence
for the old console path.

**Candidate coverage.** The AMD rootful Docker rows (E8) ran on the release candidate
itself, built from `develop` `a7c12b98`. NVIDIA rootful Docker had a fresh quick-start
install on the near-final build (#440). The other rows predate it: they are real evidence
for the code paths they ran, not a hardware pass of the final build.

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
| E8 | #409 [5949253849](https://github.com/accreleus/quasar/issues/409#issuecomment-5949253849): AMD rootful Docker on the release candidate: update, machine reboot, bench run `0f990801` |
| E9 | #458 [6030011254](https://github.com/accreleus/quasar/issues/458#issuecomment-6030011254): direct-display console, rootful Docker (NVIDIA; host named in #453 [5994740293](https://github.com/accreleus/quasar/issues/453#issuecomment-5994740293)): auto-start at 3840x2160@240, monitor power cycle, controller, sound, a streamed session beside it |
| E10 | #460 [6037368775](https://github.com/accreleus/quasar/issues/460#issuecomment-6037368775): direct-display console, rootless Podman with SELinux enforcing (NVIDIA): KDE at 3840x2160@240, keyboard, mouse, sound, monitor power cycle |

### Docker rootful

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | PASS (E1) | PASS: #402 [5877723916](https://github.com/accreleus/quasar/issues/402#issuecomment-5877723916); on the candidate (E8) | PASS (E8) | Not required: NVIDIA covers rootful Docker | PASS: run `0f990801`, "result: clean" (E8) |
| NVIDIA | PASS over CDI (E1, E4); the `--gpus` fallback is fixed (#413), checked with session-shaped containers, not an agent-created Steam session | PASS (E4) | PASS (E4) | PASS: #395 [5884298398](https://github.com/accreleus/quasar/issues/395#issuecomment-5884298398); direct display (E9) | PASS: run `3e38e6e3` (E4) |

### Docker rootless

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | PASS (E1, E2) | GAP (#502) | GAP: container restarts only (#502) | GAP (#502) | GAP (#502) |
| NVIDIA | PASS (E1, E2, E4) | PASS (E4) | PASS (E4) | PASS: #407 [5890578752](https://github.com/accreleus/quasar/issues/407#issuecomment-5890578752) | PASS: run `9384a95d` (E4) |

### Podman rootless (Fedora family)

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | PASS (E3): gamepad sequence identical to the reference, 30/30 keys, microphone RMS 2902 | GAP: E3 has no update row (#502) | PASS: VM reboot (E3) | PASS: HDMI picture, mouse, audio, DDC (E3; #407 [5912107863](https://github.com/accreleus/quasar/issues/407#issuecomment-5912107863)); no physical keyboard attached | PASS with no baseline: XFCE 1080p60 soak, `no_comparable_runs` (E3) |
| NVIDIA | PASS (E4, E6, E7; audio: #411 [5876349147](https://github.com/accreleus/quasar/issues/411#issuecomment-5876349147)) | PASS (E5) | PASS (E5) | PASS: #407 [5890937657](https://github.com/accreleus/quasar/issues/407#issuecomment-5890937657); direct display (E10) | PASS: run `9cf88ede`, "result: clean" (#399 [5876277652](https://github.com/accreleus/quasar/issues/399#issuecomment-5876277652)) |

The AMD Podman bench is an XFCE soak, not a Steam game.

### Not required

- **Podman rootful (Fedora):** experimental. A fresh Quadlet install reached `online, owned`
  (#406 [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850));
  no session, update, reboot or console evidence. LAN traffic to a published port was
  dropped by Docker's `FORWARD DROP` policy beside firewalld; the documented
  `ip-forward-no-drop` fix holds through a reboot (#416
  [5963605444](https://github.com/accreleus/quasar/issues/416#issuecomment-5963605444)).
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
| 12 | NVIDIA encode and GPU games on every engine mode | Met | CDI passes on all three required NVIDIA rows. The rootful `--gpus` fallback is fixed (#413 [5953691498](https://github.com/accreleus/quasar/issues/413#issuecomment-5953691498)), checked on the NVIDIA test host with session-shaped containers, not an agent-created Steam session |
| 13 | Session audio and microphone on every engine mode | Met | Microphone: E1 (Docker, both vendors), E6 and E3 (Podman, both vendors) |
| 14 | Home files owned by the Quasar user | Partly met | Rootless Podman: #404 [5876278031](https://github.com/accreleus/quasar/issues/404#issuecomment-5876278031); rootless Docker keeps subordinate IDs, accepted with a readiness warning: #404 [5884610200](https://github.com/accreleus/quasar/issues/404#issuecomment-5884610200) |
| 15 | Console updates work on every engine mode | Met | One host per required engine mode (matrix Update column) |
| 16 | A rootless or Podman machine returns after reboot | Met | E5, E3, E4 |
| 17 | Readiness names engine and mode, explains a missing capability | Met | #393 [5870715357](https://github.com/accreleus/quasar/issues/393#issuecomment-5870715357), #396 [5871843388](https://github.com/accreleus/quasar/issues/396#issuecomment-5871843388) |
| 18 | A missing permission gives an exact-fix readiness check | Met | #401 [5878347040](https://github.com/accreleus/quasar/issues/401#issuecomment-5878347040) |
| 19 | Media reachability tests the real path | Met | #403 [5877723197](https://github.com/accreleus/quasar/issues/403#issuecomment-5877723197) |
| 20 | GPU fault (Xid) messages when allowed, a clear skip otherwise | Partly met | The skip path is live (#402 [5873589343](https://github.com/accreleus/quasar/issues/402#issuecomment-5873589343)); no live Xid fault has exercised the shown path |
| 21 | Console mode on owned installs | Met | #395 [5884298398](https://github.com/accreleus/quasar/issues/395#issuecomment-5884298398) |
| 22 | Console mode on rootless and Podman | Met | Console column: rootless Docker and rootless Podman on both vendors' evidence; the direct-display console on rootless Podman (E10) |
| 23 | Console audio through the host's PipeWire | Met | #407 [5912430462](https://github.com/accreleus/quasar/issues/407#issuecomment-5912430462): Quasar's Pulse socket bound, HDMI playback audible with ALSA owned by PipeWire (rootless Podman, NVIDIA). The direct-display console runs its own PipeWire on the host's sound devices instead; sound proven in E9 and E10 |
| 24 | DDC monitor control rootless | Met | #407 [5890578752](https://github.com/accreleus/quasar/issues/407#issuecomment-5890578752), [5890937657](https://github.com/accreleus/quasar/issues/407#issuecomment-5890937657), [5912107863](https://github.com/accreleus/quasar/issues/407#issuecomment-5912107863) |
| 25 | Documented Podman Quadlet install | Met | #406 [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850) |
| 26 | Reinstall plus dump restore, rootful to rootless | Partly met | A v0.3.0 dump restored and a Steam session streamed (#380, landed `da0ffdca`); not into a fresh rootless install, no v0.2.x dump, and the mid-restore crash was tested in memory only |
| 27 | Docs say a rootful socket is root-equivalent | Met | #406 [5884610608](https://github.com/accreleus/quasar/issues/406#issuecomment-5884610608) |
| 28 | SELinux-enforcing hosts work without relaxing SELinux | Met | Every rootless run stayed `Enforcing`; sessions stay confined; the agent's `label=disable` per D17 |
| 29 | One behavioural suite across every engine mode | Met | #408 [5893020059](https://github.com/accreleus/quasar/issues/408#issuecomment-5893020059), in CI on all four modes; its Podman findings (#424, #425, #426) are fixed |
| 30 | Each required profile proven on hardware before `main` | Partly met | Every NVIDIA row and every AMD rootful Docker row passes (E8). AMD gaps: Docker rootless update, reboot, console, bench; Podman rootless update. Accepted for the v0.4.0 release; tracked in #502 |

Met: 1–13, 15–19, 21–25, 27–29 (**26**). Partly met: 14, 20, 26, 30 (**4**). Not
evidenced: none.

## 3. Open gaps

The milestone closed on 2026-10-09 with these gaps accepted. Every remaining run is tracked in
#502; none is a known defect.

1. **AMD, Docker rootless:** update, machine reboot, console, bench.
2. **AMD, Podman rootless:** update from the console; the direct-display console with a
   physical keyboard; a Steam bench with a baseline.
3. **Story 20:** no live Xid fault on the shown path.
4. **Story 26:** restore into a fresh rootless install, a v0.2.x dump, a live mid-restore crash.
5. **Not required:** Podman rootful and Ubuntu keep their experimental labels.

Every defect this map listed as open on 2026-10-02 (#410, #412, #413, #414, #416, #418,
#421, #422, #424, #425, #426, #432, #433, #434, #439) is closed.
