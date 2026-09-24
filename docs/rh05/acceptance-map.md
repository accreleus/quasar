# RH05 acceptance map

Baseline: `07b8a4d62af97edf3a959a09e0c3e68ee707a1ce`, the #345 integration commit on `initiative/resilient-host-architecture`. This is a **traceability draft for #346**, not a completion verdict. The authoritative [specification](https://github.com/accreleus/quasar/issues/333) has 41 numbered stories and 17 numbered implementation decisions; [#346](https://github.com/accreleus/quasar/issues/346) requires a fixed-commit combined journey. Ticket numbers below refer to their accepted issue criteria and completion comments. Their individual checks do not by themselves prove the combined journey on this baseline.

Evidence abbreviations: `R<n>` means the completion report cited in issue #<n> (quasar-bench report keyed by that issue's initiative merge); `T` names a repository test file; `C` names a review vector in [`rh05-conformance.json`](../design/rh05-conformance.json). `C` is a **structural contract vector only**: `scripts/verify/rh05-conformance.py` without `--adapter` does not execute behavior. All ticket reports and tests require a fresh #346 run on one fixed source, protocol, image and schema identity before milestone acceptance. A report citation is evidence of what that ticket recorded, not a new claim that this draft reran it.

| Story in #333 | Owning accepted criterion and existing evidence | #346 integrated check or limit |
| --- | --- | --- |
| 1. Evidence-based automatic hardware | #340 R340; `T: control-plane/internal/hostcfg/policy_candidate_test.go` | Recheck actual accessible device and probe on both GPU roles; record unavailable vendors. |
| 2. Preserve deliberate settings | #334 contract, #335 R335, #336 R336; `C: legacy_clear_preserves_agent_detection` | Upgrade/migration example with deployment baseline and existing override. |
| 3. Source choice | #335–#336, #340; `T: control-plane/internal/hostcfg/policy_catalog_test.go` | Exercise Automatic, deployment and explicit at operator interface. |
| 4. Resolved provenance | #336 R336, #340 R340; Fleet tests | Capture live Fleet source, resolved value and evidence. |
| 5. Unsafe automatic pending | #340 R340; policy candidate tests | Wrong or inaccessible device must remain pending with remedy. |
| 6. Offline durable edit | #335 R335; `C: offline_reconnect_duplicate_and_old_report` | Offline save, reconnect and final read on same stack. |
| 7. Stale edit refusal | #335 R335; `T: control-plane/internal/hostcfg/policy_handler_test.go`; `C: concurrent_edit_rejects_stale_snapshot` | Two operator revisions; show intervening field and no partial write. |
| 8. Three separate outcomes | #335, #343, #340 reports; `C: config_applies_independently_of_image_failure` | Combined Fleet/Placement visual with divergent configuration, preparation and readiness states. |
| 9. Running session preserved | #335 R335; hostcfg/agent next-session tests | Session before and after safe edit; original launch snapshot unchanged. |
| 10. Independent valid work | #336 R336, #343 R343; `C: config_applies_independently_of_image_failure` | One image failure while unrelated setting and image succeed. |
| 11. Older agent truth | #335 R335, #338 R338; `C: old_agent_does_not_claim_application` | Mixed-version operator status; no false applied state. |
| 12. Bounded retry | #336 R336, #343 R343; `T: control-plane/internal/images/rh05_retry_db_test.go` | Exhaustion and explicit Retry without infinite restart/pull loop. |
| 13. Scoped approval | #338 R338; `C: unrelated_edit_preserves_scoped_approval`, `relevant_edit_supersedes_approval_and_stale_grant` | Safe edit preserves; relevant change forces reapproval. |
| 14. Wait without killing sessions | #338–#339; `T: control-plane/internal/hostcfg/idle_apply_db_test.go` | Live session remains until normal stop; admission blocks new assignment. |
| 15. All idle blockers | #338 R338; idle/agent inventory tests | Assigned, starting, stopping, local and preparation cases; unknown remains blocked. |
| 16. Cancel waiting apply | #338 R338; `C: cancel_delayed_grant_requires_agent_nonacceptance` | Saved intent survives cancellation; own restriction releases after proof. |
| 17. Overlapping admission | #337 R337; `T: control-plane/internal/admission/store_db_test.go`; `C: idle_release_preserves_manual_and_platform_restrictions` | Manual/platform/idle overlap and owner-scoped release under real Postgres. |
| 18. One safe recovery | #339 R339; `C: crash_during_activation_recovers_last_verified_once` | Durable journal failure and at-most-one restore on combined build. |
| 19. Original failure visible | #339 R339; idle executor tests | Show failed request and verified recovered configuration together. |
| 20. Uncertain recovery protected | #339 R339; `C: crash_during_recovery_never_reactivates` | Admission stays restricted with actionable remedy. |
| 21. Boot expires unstarted approval | #338 R338, ADR 0006; `C: restore_discovers_orphan_journal_attempt` | Restart and stopped-stack restore both require reapproval. |
| 22. Started attempt reconciliation | #339 R339; durable journal tests | Lost ack, reconnect and crash do not repeat disruptive execution. |
| 23. Dynamic or fixed placement | #342 R342; `T: control-plane/internal/crud/app_placement_db_test.go` | Both modes through app editor and launch. |
| 24. Dynamic new host | #342 R342; placement DB tests | New eligible host appears without edit; fixed set stays fixed. |
| 25. Derived inheritance | #342 R342; `T: control-plane/internal/session/app_placement_db_test.go` | Parent and tile resolve same placement, including launch. |
| 26. Removal blocks new launch | #342 R342; placement/reservation DB tests | Race removal against launch; preserve accepted session and data. |
| 27. Existing home owner | #341 R341; `T: control-plane/internal/session/home_claim_db_test.go` | Launch elsewhere refuses without a second home. |
| 28. Concurrent first home | #341 R341; `C: home_first_claim_and_conflict` | Real-Postgres simultaneous claims with one owner. |
| 29. Conflicting legacy homes | #341 R341; `T: control-plane/internal/storage/home_claims_db_test.go` | Admin claim view shows protected repair conflict; no merge/delete. |
| 30. Selected managed preparation | #343 R343; `T: control-plane/internal/images/rh05_requirements_db_test.go` | Selected/unselected app on enrolled GPU host. |
| 31. Shared requirements | #343 R343, #345 R345; requirements/cleanup DB tests | Removing one app retains another app's image dependency. |
| 32. Adopted custom image | #343 R343; requirements DB tests | Custom app sharing adopted image participates in preparation. |
| 33. Unmanaged image | #343 R343; Placement UI tests | Explicit unmanaged status, with launch subject to placement. |
| 34. Pins and adoption policy | #343 R343; `T: control-plane/internal/images/provider_db_test.go` | Manual/notify/auto and pinned version remain authoritative. |
| 35. Steam seeding | #344 R344; `T: control-plane/internal/session/home_seed_db_test.go` | Real first launch using pinned official Steam image. |
| 36. Reflink/copy/cold visibility | #344 R344; agent template/home tests | Retain distinct live outcomes and visual proof; only reflink claims sharing. |
| 37. Cold fallback | #344 R344; agent template/home tests | Compatible-template absence permits launch. |
| 38. Existing home intact | #344 R344; home seed DB tests | Repeated launch preserves an existing marker/content byte for byte. |
| 39. Explicit protected cleanup | #345 R345; `T: control-plane/internal/images/rh05_cleanup_fence_db_test.go`, `control-plane/internal/agentws/image_cleanup_test.go`; `C: cleanup_racing_new_reference` | Current requirement, stopped container, pending work, previous version, stale preview and confirmed removal. |
| 40. Actionable console | #335–#345 reports and UI tests | One final visual audit of Fleet, Settings, Placement, Homes, Steam and Cleanup states. |
| 41. Enrollment/mount boundary | #333 D2 and [operator handoff](operator-handoff.md) | Confirm documentation against final image/compose state; RH06 remains separate. |

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

## Remaining #346 proof on a fixed commit

The 22 contract vectors cover review intent, but no repository adapter executes them end to end. The required proof is therefore the actual operator/launch/preparation tests, real ephemeral Postgres transaction tests, durable agent-journal tests and live role-based acceptance. Record each test command, source and protocol SHA, schema version, local image digest, stack identity, active-session ownership, result and limitation in the completion report. Recheck the tracker and report links when #346 chooses its fixed integration SHA. The individual ticket reports include live evidence from differing commits and are **not** interchangeable with a combined fixed-build run.

The most consequential integration cases are: offline and stale edits; mixed agents; overlapping holds; approval supersession, cancel, restart and stopped-stack restore; started-attempt recovery; first-home and placement races; requirement/cleanup races; and Steam seed and fallback paths. Capture browser views of configuration, preparation and readiness separately. Real GPU evidence is required for hardware conclusions; unavailable Intel remains a limit. Do not mark #346 complete or promote any branch based on this map alone.
