# RH-07 acceptance map

Written for #409 (D19). Sources: #393–#409 and their comments, the defect issues filed
during RH-07 (#410–#423), `docs/rh07/2026-09-28-decisions.md` (D1–D23), the specification
(#390) and `testdata/engine-profiles/profiles.json`. Citations are an issue plus its
comment URL, or a commit hash. Hosts are named by role only: the AMD test host, the NVIDIA
test host, the rootless lab VM (NVIDIA-GPU passthrough, SELinux enforcing, read-only
`/usr`, per D23).

Microphone capture is proven only on Podman rootless with NVIDIA (#409 comment
[5892348346](https://github.com/accreleus/quasar/issues/409#issuecomment-5892348346)); every
other Session cell proves playback only, so it reads PARTIAL.

## 1. Profile matrix (D19)

Required rows per cell: a real Steam session with input, audio and microphone (**Session**);
an update applied from the console (**Update**); a reboot after which everything returns
(**Reboot**); console mode on one host per engine (**Console**); bench runs posted (**Bench**).

### Docker rootful

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | PARTIAL — video, input, audio pass; microphone untested: — #399 comment [5877729442](https://github.com/accreleus/quasar/issues/399#issuecomment-5877729442), #402 comment [5877723916](https://github.com/accreleus/quasar/issues/402#issuecomment-5877723916) | PASS — #402 comment [5877723916](https://github.com/accreleus/quasar/issues/402#issuecomment-5877723916) | GAP — not run on this host; #399's table records it as "—" (comment [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911)) | GAP — no console-mode run on the AMD test host; #395/#407 evidence is NVIDIA-only | GAP — no bench run cited against the AMD test host in #399/#402/#403 |
| NVIDIA | PARTIAL — CDI path PASS (video, input, audio; microphone untested), #399 comment [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911); the `--gpus` fallback path (CDI switched off) FAILS — Steam's Vulkan loader fails on this CUDA-only host, filed as #413 | PASS (CDI path) — #399 comment [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911) | PASS (CDI path) — same table | PASS — enable/disable, Steam on the local screen, keyboard/mouse grabbed, DDC power detection, audio all confirmed by the owner watching the monitor: #395 comment [5884298398](https://github.com/accreleus/quasar/issues/395#issuecomment-5884298398) | PASS — run `3e38e6e3` (`baseline/steam-1440p60-av1-observe`), #399 comment [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911) |

### Docker rootless

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | GAP — no rootless-Docker run on the AMD test host anywhere in #393–#423 | GAP | GAP | GAP | GAP |
| NVIDIA | PARTIAL — video/input(4.18 vs 0 idle)/audio PASS, #399 comment [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911); input is proven by the coarse motion/click probe only — the full per-button gamepad+keyboard replay (#401 comment [5891242775](https://github.com/accreleus/quasar/issues/401#issuecomment-5891242775)) ran on rootless Podman only, not here; microphone untested | PASS — #399 comment [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911) | PASS — same table | PASS — enable in ~6s, `console_display`/`console_ddc`/`console_audio` pass, owner confirmed XFCE on the monitor with physical keyboard/mouse grabbed and DP audio: #407 comment [5890578752](https://github.com/accreleus/quasar/issues/407#issuecomment-5890578752) | PASS — run `9384a95d`, cross-engine comparison recorded as run-to-run noise not regression: #399 comment [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911) |

### Podman rootless (Fedora, required)

| GPU | Session | Update | Reboot | Console | Bench |
|---|---|---|---|---|---|
| AMD | GAP — no rootless-Podman run on the AMD test host anywhere in #393–#423 | GAP | GAP | GAP | GAP |
| NVIDIA | PASS — microphone: #409 comment [5892348346](https://github.com/accreleus/quasar/issues/409#issuecomment-5892348346); video/input/audio PASS, #399 comment [5880870911](https://github.com/accreleus/quasar/issues/399#issuecomment-5880870911); full per-button gamepad+keyboard+d-pad+trigger replay via the input DataChannel, every code arrived correctly: #401 comment [5891242775](https://github.com/accreleus/quasar/issues/401#issuecomment-5891242775); audio energy 21.9 on a real browser, survives a VM reboot: #411 comment [5876349147](https://github.com/accreleus/quasar/issues/411#issuecomment-5876349147) | PASS — install→update→verify on the recovery actor: #405 comment [5873588293](https://github.com/accreleus/quasar/issues/405#issuecomment-5873588293) | PASS — all five owned-install containers came back on their own via `podman-restart.service` and lingering: #405 comment [5873588293](https://github.com/accreleus/quasar/issues/405#issuecomment-5873588293) | PASS — picture OK, keyboard/mouse grabbed, audio tone heard (ALSA, not PipeWire — see gaps), owner watched the monitor: #407 comment [5890937657](https://github.com/accreleus/quasar/issues/407#issuecomment-5890937657) | PASS — run `9cf88ede` vs `1d8d15be`, verbatim "0 regressed, 0 improved, 1 unchanged. result: clean": #399 comment [5876277652](https://github.com/accreleus/quasar/issues/399#issuecomment-5876277652) |

**Reading the matrix:** on NVIDIA, every required row except the microphone half of
**Session** has evidence in all three profiles (Docker rootful's `--gpus` fallback aside, a
named defect, #413; the same host works end to end over CDI). Only Podman rootless has its
microphone proven so far. The AMD side has only one required cell run at all
(Docker rootful); Docker rootless and Podman rootless on AMD are untested, not failing —
the AMD test host currently lacks the host input-device mount its rootless containers
would need, so no agent has been run there under an unprivileged rootless account. This
needs the lab owner.

### Podman rootful and Ubuntu 24.04 (not required; claimed/experimental/unsupported by evidence)

Per `testdata/engine-profiles/profiles.json` (the table the site, the enrollment script and
the agent's `runtime_engine` check all read):

- **Podman rootful, Fedora:** labelled **experimental** — "It has not yet been through the
  same end-to-end tests as the other Fedora profiles; it becomes supported once it passes."
  Evidence matches the label: a fresh install from the generated Quadlet output reached
  `online, owned` on the rootless lab VM, #406 comment [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850);
  no Steam session, update, reboot or console-mode evidence exists for it, and it carries a
  known defect (LAN traffic to the console port is dropped under firewalld's INPUT-only
  rules, since netavark forwards published ports rather than binding a host socket), filed
  as #416.
- **Ubuntu 24.04, every engine/mode:** labelled **experimental** ("Nobody has run Quasar
  end to end on Ubuntu 24.04 with this engine yet; it should work") except Docker rootful,
  labelled **supported** on the claim that "Rootful Docker is the engine profile Quasar is
  validated on, and it works the same on any distribution" — a claim carried over from
  before RH-07, not new RH-07 hardware evidence. No RH-07 ticket ran anything on Ubuntu;
  every live run in #393–#423 is on Fedora-family hosts (the rootless lab VM, the AMD test
  host, the NVIDIA test host). Ubuntu rootless rows should be read as **experimental: no
  hardware evidence**, matching the table.

## 2. User story map (30 stories, spec #390)

| # | Story | Status | Evidence / why not |
|---|---|---|---|
| 1 | Rootless engine, compromise can't take the machine | Met | Least-privilege recipe revision 3 drops host `/dev`, `NET_ADMIN`, `SYSLOG`, `/dev/kmsg`: #402 comment [5873589343](https://github.com/accreleus/quasar/issues/402#issuecomment-5873589343). Host prep grants only named devices by ACL, no broad group: #400 comment [5870987736](https://github.com/accreleus/quasar/issues/400#issuecomment-5870987736) |
| 2 | Podman install and update | Met | Install, update-from-console and reboot all pass on rootless Podman: #405 comment [5873588293](https://github.com/accreleus/quasar/issues/405#issuecomment-5873588293) |
| 3 | Rootless Docker install and update | Met | Quick start installs it without edits: #406 comment [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850); update-from-console proven in the #409 matrix (Docker rootless row) |
| 4 | Rootful Docker/Unraid unchanged, less privilege | Met | Recipe revision 3 proven on the AMD test host with sessions, probes and readiness unchanged: #402 comment [5877723916](https://github.com/accreleus/quasar/issues/402#issuecomment-5877723916). Unraid itself was not separately run (D4 keeps it on the unmodified rootful path by design; nothing in RH-07 touches that path) |
| 5 | Quick start/enrollment detect engine and mode | Met | Host facts (`engine`/`engine_version`/`engine_mode`) from real `/version`/`/info`: #396 comment [5871843388](https://github.com/accreleus/quasar/issues/396#issuecomment-5871843388). Enrollment detects and refuses correctly: #406 comment [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850) |
| 6 | Quick start shows supported/experimental/unsupported | Met | `testdata/engine-profiles/profiles.json` drives the quick start's badges and the enrollment refusal, covered offline for all 12 unsupported rows: #406 comment [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850) |
| 7 | One host-prep command, idempotent, explains itself | Met | `deploy/prepare-host.sh`, 52 offline tests plus two live runs (fresh + idempotent no-op) on the rootless lab VM and the AMD test host: #400 comment [5870987736](https://github.com/accreleus/quasar/issues/400#issuecomment-5870987736) |
| 8 | Host prep touches only `/etc`, works on read-only `/usr` | Met | `touch /usr/x` fails with "Read-only file system"; the script still completes: #400 comment [5870987736](https://github.com/accreleus/quasar/issues/400#issuecomment-5870987736) |
| 9 | Dedicated `quasar` account | Met | Host prep creates it with subordinate ID ranges and lingering; the whole owned stack runs under it: #400, #405 comment [5873588293](https://github.com/accreleus/quasar/issues/405#issuecomment-5873588293) |
| 10 | Quasar never runs as root / escalates at run time | Met | No code path adds privilege on read-back failure (#397 comment [5872048545](https://github.com/accreleus/quasar/issues/397#issuecomment-5872048545)); the one accepted exception is the node agent's own SELinux label (`label=disable`, D17, owner decision #402 comment [5884609826](https://github.com/accreleus/quasar/issues/402#issuecomment-5884609826)), which is a confinement label, not privilege escalation — the agent never becomes root and grants nothing beyond the engine socket it always held |
| 11 | Controller, keyboard, mouse work rootless | Partly met | Full per-button/axis replay passed only on rootless Podman: #401 comment [5891242775](https://github.com/accreleus/quasar/issues/401#issuecomment-5891242775). Docker rootful/rootless have only the coarse motion/click probe; the same full check on Docker "needs the lab owner" per that comment |
| 12 | NVIDIA hardware encode + GPU game on every engine mode | Partly met | CDI path passes on all three required NVIDIA rows (matrix above). The rootful `--gpus` fallback on a CUDA-only host fails Steam's Vulkan init, filed as #413 |
| 13 | Session audio and microphone on every engine mode | Partly met | Playback proven on every profile with NVIDIA evidence (matrix above; #411 comment [5876349147](https://github.com/accreleus/quasar/issues/411#issuecomment-5876349147)). Microphone capture proven on Podman rootless with NVIDIA only: #409 comment [5892348346](https://github.com/accreleus/quasar/issues/409#issuecomment-5892348346) |
| 14 | Home files owned by the Quasar user | Partly met | True on rootless Podman via `keep-id`: #404 comment [5876278031](https://github.com/accreleus/quasar/issues/404#issuecomment-5876278031). Rootless Docker has no per-container user mapping in its API; homes keep subordinate IDs, and the owner accepted this with a readiness warning rather than a refusal: #404 comment [5884610200](https://github.com/accreleus/quasar/issues/404#issuecomment-5884610200) |
| 15 | Console updates work on every engine mode | Met | Update-from-console proven on Docker rootful (#402 comment [5877723916](https://github.com/accreleus/quasar/issues/402#issuecomment-5877723916)), Docker rootless and Podman rootless (matrix above). Rootful Podman was not exercised (not a required profile) |
| 16 | Rootless/Podman machine returns after reboot | Met | Podman: all five containers returned via `podman-restart.service` + lingering: #405 comment [5873588293](https://github.com/accreleus/quasar/issues/405#issuecomment-5873588293). Rootless Docker: reboot row PASS in the #399 matrix |
| 17 | Readiness card names engine/mode, explains missing capability | Met | Amendment 17 defines `skip` with the setting named, no remediation and no `blocks`: #393 comment [5870715357](https://github.com/accreleus/quasar/issues/393#issuecomment-5870715357). `runtime_engine`/`runtime_endpoint` name the detected engine: #396 comment [5871843388](https://github.com/accreleus/quasar/issues/396#issuecomment-5871843388) |
| 18 | Missing permission produces an exact-fix readiness check | Met | `input_probe` names `deploy/prepare-host.sh` on failure: #401 comment [5878347040](https://github.com/accreleus/quasar/issues/401#issuecomment-5878347040); no remediation text tells an operator to run Quasar as root: #396 comment [5871843388](https://github.com/accreleus/quasar/issues/396#issuecomment-5871843388) |
| 19 | Media reachability tests the real path, not firewall rules | Met | Active probe from real ICE traffic, pass/fail/unknown proven rootful and rootless: #403 comment [5877723197](https://github.com/accreleus/quasar/issues/403#issuecomment-5877723197) |
| 20 | GPU fault (Xid) messages when allowed, clear skip otherwise | Partly met | The skip path is implemented and reports the setting (`xid_visibility`, amendment 17): #402 comment [5873589343](https://github.com/accreleus/quasar/issues/402#issuecomment-5873589343). No live run exercises the "shown when allowed" positive path (an actual Xid fault surfaced in a trace) |
| 21 | Console mode available again on owned installs | Met | #395 comment [5884298398](https://github.com/accreleus/quasar/issues/395#issuecomment-5884298398) |
| 22 | Console mode on rootless and Podman | Met | Rootless Docker and rootless Podman console rows both PASS on hardware (matrix above) |
| 23 | Console local audio through the host's PipeWire when present | Not yet evidenced | Designed (host prep would add a Quasar-only PipeWire listen socket): #407 comment [5887423634](https://github.com/accreleus/quasar/issues/407#issuecomment-5887423634). Both live console-audio runs used the ALSA fallback because the tested hosts run no PipeWire session: #407 comments [5890578752](https://github.com/accreleus/quasar/issues/407#issuecomment-5890578752) and [5890937657](https://github.com/accreleus/quasar/issues/407#issuecomment-5890937657). The PipeWire path itself has no hardware proof |
| 24 | DDC monitor control keeps working rootless | Met | DDC power-cycle auto-starts/stops the session on rootless Docker and rootless Podman: #407 comments [5890578752](https://github.com/accreleus/quasar/issues/407#issuecomment-5890578752), [5890937657](https://github.com/accreleus/quasar/issues/407#issuecomment-5890937657); rootful: #395 comment [5884298398](https://github.com/accreleus/quasar/issues/395#issuecomment-5884298398) |
| 25 | Podman Quadlet documented install | Met | Generated Quadlet unit installs without edits, rootful and rootless: #406 comment [5887010850](https://github.com/accreleus/quasar/issues/406#issuecomment-5887010850) |
| 26 | Reinstall + dump restore, rootful to rootless | Not yet evidenced | D16 defers this to a fresh install plus the separate dump-restore ticket (#380); no #393–#423 ticket exercises it |
| 27 | Docs say a rootful engine socket is root-equivalent | Met | Engine-profile page states it plainly; the accepted `label=disable` exception is documented the same way: #406 comment [5884610608](https://github.com/accreleus/quasar/issues/406#issuecomment-5884610608) |
| 28 | SELinux-enforcing hosts work without relaxing SELinux | Met | Every rootless run in #393–#423 keeps `getenforce` at `Enforcing` throughout, including after reboot: #400, #402, #404, #405, #407. The one exception is the node agent's own `label=disable`, accepted under D17 (story 10); sessions themselves stay confined (`container_t`/`container_engine_t`) |
| 29 | Same behavioural suite across every engine mode | Partly met | One suite (identity, create-read-back, user mapping, devices, CDI, restart, health, removal, errors) runs unchanged against each mode, and CI runs it on Docker rootful, Docker rootless, Podman rootful and Podman rootless (branch `rh07/408-engine-suite`, commit `293c537f`). Not yet met: the CDI, DRM and uinput cases need the lab procedure on real hardware, and Podman 4.9 (Ubuntu) has three recorded known failures (create-read-back, restart policy update, missing bind source) |
| 30 | Each required profile proven on real hardware before `main` | Partly met | NVIDIA: all three required profiles pass every row except the microphone half of Session (matrix above). AMD: only Docker rootful has been run at all; Docker rootless and Podman rootless on AMD are gaps, not failures |

**Count:** 16 met, 8 partly met, 6 not yet evidenced (stories 23, 26, 29 not yet evidenced;
13, 20 count as partly met above — recount below for clarity).

Met: 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 15, 16, 17, 18, 19, 21, 22, 24, 25, 27, 28 — **21**.
Partly met: 11, 12, 13, 14, 20, 30 — **6**.
Not yet evidenced: 23, 26, 29 — **3**.

## 3. Open gaps

Each item names what would close it.

**Matrix gaps:**
1. **Docker rootful/AMD — no reboot, console mode or bench run.** Needs a reboot cycle,
   a console-mode run and a bench run on the AMD test host; the lab owner's go-ahead is
   needed for the reboot.
2. **Docker rootless/AMD and Podman rootless/AMD — entirely untested.** The AMD test
   host currently lacks the host input-device mount its container needs for a rootless
   agent, so `input_probe` cannot pass there yet. Needs the lab owner to add that mount
   (or an equivalent unprivileged account with the right device access), then the full
   row set from the matrix.
3. **Docker rootful/NVIDIA, `--gpus` fallback — Steam's Vulkan loader fails on a
   CUDA-only host (#413).** Open, unresolved; suspected cause is the legacy hook's
   injected libraries colliding with the driver volume's `libGLX_nvidia`. Needs a
   comparison of the hook's mounts against the CDI spec and a retest with
   `NVIDIA_DRIVER_CAPABILITIES=compute,utility,video`.
4. **Docker rootful/rootless on NVIDIA — no full per-button gamepad/keyboard replay,
   only the coarse motion/click probe.** The full check (#401 comment 5891242775) needs
   re-running on those two engine modes; the same comment notes it "needs the lab owner"
   because one test host is unavailable while the rootless lab VM holds the GPU, and a
   container setting on the other stops runtime-created input nodes from appearing.
5. **Microphone capture is proven on Podman rootless (NVIDIA) only.** The method (a real
   browser with a fake capture device, then the RMS of the app's `quasar_mic_src` inside
   the session) is in #409 comment
   [5892348346](https://github.com/accreleus/quasar/issues/409#issuecomment-5892348346);
   it needs running once per remaining profile.
6. **Podman rootful — no Steam session, update, reboot or console-mode evidence**, and
   a known LAN-reachability defect under firewalld (#416: netavark forwards published
   ports, so INPUT-zone rules don't cover them). Not a required profile (D5), so this
   does not block release, but its `experimental` label is correct and should stay
   until #416 is fixed and the row set is run.
7. **Ubuntu 24.04 — no hardware run on any engine/mode.** All RH-07 live evidence is
   Fedora-family. The `supported` label on Docker rootful/Ubuntu predates RH-07 and
   carries no RH-07-specific evidence; the other three rows are correctly `experimental`
   with "nobody has run it yet."

**Story gaps:**
8. **#408's suite runs in CI but not yet in the lab.** Its CDI, DRM and uinput cases need
   the lab procedure on real hardware per engine mode, and its Podman 4.9 known failures
   need tickets and fixes. Story 29 depends on it.
9. **Console audio over PipeWire (story 23) has no hardware proof** — both live runs
   fell back to ALSA because the tested hosts run no PipeWire session. Needs a rootless
   host with an active PipeWire session (the plan named the Bazzite VM, #407 comment
   5887423634) and a repeat of the console-audio check there.
10. **Dump restore into a fresh rootless install (story 26)** is out of RH-07's own
    tickets; it needs #380 to close first, then a live reinstall-and-restore run.
11. **GPU-fault (Xid) visibility's "shown when allowed" path (story 20)** has no live
    trigger; only the skip-and-why path has been exercised.

**Known lab constraints (not RH-07 defects, recorded here as the reason several rows
above are gaps rather than failures):**
- The NVIDIA test host's GPU is shared with a lab VM; only one can hold the card at a
  time, so NVIDIA rows sometimes had to wait or move to the rootless lab VM.
- The AMD test host currently lacks the host input-device mount its rootless container
  would need, so its input probe fails there; this needs the lab owner.
- Console-mode VT restore defects are being fixed on a separate branch
  (`rh07/407-console-vt`) and are not reflected in the console-mode rows above, which
  predate that fix.

**Filed defects still open, relevant to the matrix or stories above:** #410 (Podman
non-recursive read-only binds, hardening), #412 (`engine_restart_on_boot` readiness check
not implemented — the underlying behaviour is proven, but the check itself is a stub),
#413 (see gap 3), #414 (console mode black picture under GPU passthrough to a VM — a
hypervisor issue, not Quasar), #415 (a rotted offline test, unrelated to any gate), #416
(see gap 6), #418 (homes-root reconfigure leaves stale paths), #419/#420 (XFCE app-image
defects found during console-mode evidence), #421 (console hotplug devices never
grabbed), #422 (console app display doesn't expose real monitor modes).
