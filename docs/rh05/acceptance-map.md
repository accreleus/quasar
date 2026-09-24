# RH05 acceptance map

Fixed integration commit: **`7bd1234bd0dd6603634d5b0308c3205dca5fb6bc`** on `feat/rh05-346-operator-journey` (protocol `0a9019e9280738c9b0ad106c13ce54c0ed3dc449`, schema version 94). It contains the #334–#345 integration (`07b8a4d`) plus the #346 fixes found during this acceptance: lazy digest admission, lazy template and lazy digest preparation before assignment, and the agent's refusal to pull a missing local build tag. The authoritative [specification](https://github.com/accreleus/quasar/issues/333) has 41 numbered stories and 17 numbered implementation decisions; [#346](https://github.com/accreleus/quasar/issues/346) requires the combined journey on one fixed commit.

**Gate** means the containerized suites on the fixed commit: `make verify` (465/0/0), `make test-go`, `TESTDB_CONTAINERISED=1 make test-db` (real ephemeral PostgreSQL), `make test-rust`, `make test-web` and `make preflight`. **Live** means the isolated AMD or NVIDIA acceptance stack running images built from the fixed commit with `deploy/build-images.sh` and served from the local registry, driven through the real web client or admin API. **Limit** names what this run did not prove. Ticket reports (`R<n>`, the completion report cited on issue #<n>) remain supporting evidence from their own commits. `C` vectors in [`rh05-conformance.json`](../design/rh05-conformance.json) are structural review vectors only.

| Story in #333 | Fixed-build result on `7bd1234` |
| --- | --- |
| 1. Evidence-based automatic hardware | **Live.** AMD: Automatic encoder and render node resolved from the accessible device and host probe, approved in the console and applied by the agent's post-restart probe. NVIDIA: the Automatic hardware applied on this isolated stack before the final build remains `applied` and fresh on the final agent's current connection, and the final agent image passed the NVIDIA GPU contract (147/0). Intel unavailable (**Limit**). |
| 2. Preserve deliberate settings | **Gate** (hostcfg migration and legacy-clear tests). **Live:** `cuda_device` stayed on its deployment source through the AMD apply. |
| 3. Source choice | **Live:** Automatic and deployment sources shown and applied; an explicit `home_root` that would strand existing homes was refused with `400 validation_failed` and no write. **Gate** for the remaining combinations. |
| 4. Resolved provenance | **Live:** Fleet settings show the resolved value, source and "verified on the current connection". |
| 5. Unsafe automatic pending | **Gate.** **Live:** the AMD hardware group stayed `pending` with a remedy until approval. |
| 6. Offline durable edit | **Gate** only (**Limit**: not repeated live on this commit). |
| 7. Stale edit refusal | **Gate** only. |
| 8. Three separate outcomes | **Live:** the Placement tab shows Selected, Prepared and Ready separately; idle apply shows saved configuration apart from approval and execution. |
| 9. Running session preserved | **Live:** a Steam session kept decoding at 60 fps through a waiting approval and a control-plane restart. **Gate** for next-session snapshots. |
| 10. Independent valid work | **Gate** only. |
| 11. Older agent truth | **Gate** only (no older agent was deployed live). |
| 12. Bounded retry | **Gate.** A failed lazy preparation is retried only by the next launch (Retry refuses lazy images). |
| 13. Scoped approval | **Live:** approval bound to a reviewed revision and content digest. **Gate** for supersession. |
| 14. Wait without killing sessions | **Live:** approval `waiting`, host `draining` with one `idle_apply` restriction, a new launch refused `503 no_host_available`, the running session untouched. |
| 15. All idle blockers | **Live:** running session and conflicting preparation named as blockers. **Gate** for assigned, starting, stopping and local cases. See the preparation deadlock under Remaining limits. |
| 16. Cancel waiting apply | **Gate** only. |
| 17. Overlapping admission | **Gate** only. |
| 18. One safe recovery | **Gate** only (durable journal tests). |
| 19. Original failure visible | **Gate** only. |
| 20. Uncertain recovery protected | **Gate** only. |
| 21. Boot expires unstarted approval | **Live:** after a control-plane restart the waiting approval became `revoked_unstarted`, no attempt had started, admission reopened and the session survived. Stopped-stack restore: **Gate** only. |
| 22. Started attempt reconciliation | **Gate.** **Live:** the re-approved AMD attempt reached `applied` once, with no recovery. |
| 23. Dynamic or fixed placement | **Live:** dynamic (all eligible) on both stacks. Fixed set: **Gate**. |
| 24. Dynamic new host | **Gate** only. |
| 25. Derived inheritance | **Gate** only. |
| 26. Removal blocks new launch | **Gate** only. |
| 27. Existing home owner | **Gate.** **Live:** the agent refused a mount outside its deployment home root rather than creating a second home. |
| 28. Concurrent first home | **Gate** only. |
| 29. Conflicting legacy homes | **Gate** only. |
| 30. Selected managed preparation | **Live:** the selected lazy Steam image was prepared on the placed host before assignment; cold pull on NVIDIA, cached image on AMD. |
| 31. Shared requirements | **Gate** only. |
| 32. Adopted custom image | **Gate** only. |
| 33. Unmanaged image | **Gate** only. |
| 34. Pins and adoption policy | **Live:** the pinned official Steam digest was used unchanged. **Gate** for policy modes. |
| 35. Steam seeding | **Live:** real browser launches of the pinned official Steam image on both GPU roles. |
| 36. Reflink/copy/cold visibility | **Live:** `reflink` (AMD, new user after template publication) and `cold` (NVIDIA, `template_unavailable`). **Limit:** `copy` was not re-exercised on this commit; see #344's accepted report. |
| 37. Cold fallback | **Live:** NVIDIA first launch reported `cold` and streamed. |
| 38. Existing home intact | **Live:** AMD relaunch reported `existing`; a 64 KiB marker kept its SHA-256 while a newer template existed. |
| 39. Explicit protected cleanup | **Live:** the Steam image is listed as managed and refused as `required` (`409`). Confirmed removal: **Gate** and #345's report. |
| 40. Actionable console | **Live:** Fleet, Settings, Placement, idle apply and loader captures in the #346 report. |
| 41. Enrollment/mount boundary | Documented in the [operator handoff](operator-handoff.md); RH06 remains separate. |

## Q1–Q27 decision aliases

The owner-approved design discussion recorded Q1–Q27 (and a Steam clarification Q13a) locally before #333 publication. **Its individual Q paragraphs are not published as separately addressable authority.** #333's 17 Implementation Decisions (`D1`–`D17`) and 41 stories are the public authority. This crosswalk preserves those decision labels as traceability aliases; it does not claim a public Q-level citation or add scope. ADR 0006 explicitly identifies Q27.

| Alias | Published authority in #333 | Tickets/evidence |
| --- | --- | --- |
| Q1 automatic configuration | Stories 1, 3–5; D3 | #334, #340 R340 |
| Q2 enrolled-host boundary | Story 41; D2; Out of Scope | #334, #346 handoff |
| Q3 simple placement | Stories 23–24; D12–D13 | #342 R342, #343 R343 |
| Q4 explicit idle apply | Stories 13–16; D9 | #338 R338, #339 R339 |
| Q5 upgrade preservation | Stories 2–4; D2 | #334, #335 R335 |
| Q6 placement controls launch | Stories 23, 26–27; D12 | #341 R341, #342 R342 |
| Q7 idle admission | Stories 14–17; D8–D9 | #337 R337, #338 R338 |
| Q8 independent results | Stories 8, 10; D4 | #336 R336, #343 R343 |
| Q9 established real evidence | Stories 1, 5; D3 | #340 R340 |
| Q10 independent outcomes | Story 8; D1, D5 | #335, #343, #340 reports |
| Q11 approval scope | Story 13; D9 | #338 R338 |
| Q12 removal without deletion | Stories 26, 39; D12, D15 | #342 R342, #345 R345 |
| Q13 Steam template; Q13a fallback | Stories 35–38; D14 | #344 R344 |
| Q14 offline durability | Story 6; D4–D5 | #335 R335 |
| Q15 older agents | Story 11; D6 | #335 R335, #338 R338 |
| Q16 bounded retry | Story 12; D7 | #336 R336, #343 R343 |
| Q17 full idle inventory | Story 15; D9 | #338 R338 |
| Q18 bounded recovery | Stories 18–20; D10 | #339 R339 |
| Q19 three source modes | Stories 2–4; D2–D3 | #334, #335, #340 reports |
| Q20 dynamic membership | Stories 23–24; D12 | #342 R342 |
| Q21 owner-scoped release | Story 17; D8–D9 | #337 R337, #338 R338 |
| Q22 stale edits and restore | Stories 7, 21–22; D4, D11 | #335 R335, #339 R339 |
| Q23 derived placement and sharing | Stories 25, 31; D12–D13 | #342 R342, #343 R343 |
| Q24 custom images | Stories 32–34; D13 | #343 R343 |
| Q25 managed-home uniqueness | Stories 27–29; D12 | #341 R341 |
| Q26 cleanup retention | Story 39; D15 | #345 R345 |
| Q27 restart/restore expiry | Stories 21–22; D11; ADR 0006 | #338 R338, #339 R339 |

## Remaining limits recorded by #346

- **Preparation can hold idle apply.** The frozen idle-apply contract treats Steam preparation `waiting_image`, `deferred` and `failed` as conflicting work. On the AMD role, Steam template warmup failed because the Automatic render node was not yet applied, so preparation blocked the very apply that fixes it. The operator remedy used here was to turn **Prepare Steam for faster first launch** off, apply, then turn it back on; warmup then succeeded. A contract change that stops non-running preparation from blocking a restart needs the prescribed review and owner sign-off.
- **`copy` not repeated live** (story 36). Changing a host's deployment home root is correctly refused while it would strand existing managed homes, and the isolated stack could not put homes and templates on a non-reflink filesystem without moving homes.
- **A cached lazy digest is subject to the agent's 2 GiB free-disk guard**, like eager images. An agent that echoes an empty `image_state.version` would time out lazy preparation after 15 minutes. A same-version re-adoption to a different digest re-acknowledges without a pull.
- **No bench verdict.** `make bench-check` returned nothing comparable (rc 4); this change is not on the streaming path.
- Intel hardware was unavailable.
