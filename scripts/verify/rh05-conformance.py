#!/usr/bin/env python3
"""Validate RH05 contract vectors, or execute them via an implementation adapter.

Adapter module must export Adapter with reset(given), act(step), observe(assertion).
This script's no-adapter mode checks the review vectors only; it is not a
substitute for the implementation's Postgres, agent-journal, or hardware tests.
"""

import argparse
import importlib
import json
from pathlib import Path


parser = argparse.ArgumentParser()
parser.add_argument("--adapter", help="Python module exporting Adapter")
args = parser.parse_args()

path = Path(__file__).resolve().parents[2] / "docs/design/rh05-conformance.json"
data = json.loads(path.read_text())
assert data["format"] == 1
cases = data["scenarios"]
ids = [case["id"] for case in cases]
assert len(ids) == len(set(ids))
assert {case["seam"] for case in cases} == {"operator", "launch", "preparation"}
required_ids = {
    "config_applies_independently_of_image_failure",
    "unrelated_edit_preserves_scoped_approval",
    "relevant_edit_supersedes_approval_and_stale_grant",
    "changed_device_evidence_rejects_stale_grant",
    "idle_release_preserves_manual_and_platform_restrictions",
    "legacy_disruptive_patch_requires_separate_approval",
    "config_update_preserves_settings_while_rh05_applies",
    "intentional_restart_resumes_verification_once",
    "crash_during_candidate_initialization_consumes_startup_marker",
    "crash_during_recovery_initialization_consumes_startup_marker",
    "crash_during_activation_recovers_last_verified_once",
    "crash_during_recovery_never_reactivates",
    "restore_reseeds_revision_and_recomputes_digest",
    "revision_above_javascript_safe_integer_is_exact",
}
assert required_ids <= set(ids)
for case in cases:
    assert case["given"] and case["when"] and case["expect"]
    for field in ("given", "when", "expect"):
        assert all(isinstance(item, dict) and item for item in case[field])


def validate_wire_revisions(value):
    """JSON numbers lose precision in browser clients; revisions use strings."""
    if isinstance(value, list):
        for item in value:
            validate_wire_revisions(item)
    elif isinstance(value, dict):
        for key, item in value.items():
            if (key.endswith("revision") or key.endswith("revision_high_water")) and item is not None:
                if isinstance(item, dict):
                    if key in {"setting_applied_revision", "desired_group_revision", "agent_revision_high_water"}:
                        assert all(
                            revision is None
                            or (isinstance(revision, str) and revision.isascii() and revision.isdecimal())
                            for revision in item.values()
                        ), (key, item)
                    else:
                        validate_wire_revisions(item)
                else:
                    assert isinstance(item, str) and item.isascii() and item.isdecimal(), (key, item)
                    assert item == "0" or not item.startswith("0"), (key, item)
            else:
                validate_wire_revisions(item)


validate_wire_revisions(cases)

by_id = {case["id"]: case for case in cases}
legacy_clear = by_id["legacy_clear_preserves_agent_detection"]
detected = legacy_clear["given"][0]["detected_encoder"]
assert detected == "$agent_detected_encoder"
assert legacy_clear["when"][-1]["report_verified_post_restart"] == detected
assert legacy_clear["expect"][-1]["setting_applied_value"]["encoder"] == detected
offline = by_id["offline_reconnect_duplicate_and_old_report"]
saved_revisions = [step["save"].get("expected_revision") for step in offline["when"] if "save" in step]
assert len(saved_revisions) == 2
assert saved_revisions[0] == offline["given"][0]["policy_revision"]
assert int(saved_revisions[1]) == int(saved_revisions[0]) + 1

legacy_patch = by_id["legacy_disruptive_patch_requires_separate_approval"]
assert {"http_status": 200} in legacy_patch["expect"]
assert {"legacy_response": {"restart_triggered": False, "pending_restart": False}} in legacy_patch["expect"]
assert {"idle_approval_created": False} in legacy_patch["expect"]
coexist = by_id["config_update_preserves_settings_while_rh05_applies"]
assert coexist["when"][0]["config_update"]["settings"] is None
assert "console_config" in coexist["when"][0]["config_update"]
assert "source_policies" in coexist["when"][0]["config_update"]
restore = by_id["restore_reseeds_revision_and_recomputes_digest"]
assert int(restore["expect"][0]["policy_revision"]) > int(
    restore["given"][0]["agent_revision_high_water"]["hardware"]
)
assert {"desired_digest_recomputed": {"hardware": True}} in restore["expect"]

if args.adapter:
    adapter_type = importlib.import_module(args.adapter).Adapter
    for case in cases:
        adapter = adapter_type(case["seam"])
        adapter.reset(case["given"])
        for step in case["when"]:
            adapter.act(step)
        for assertion in case["expect"]:
            if not adapter.observe(assertion):
                raise AssertionError(f"{case['id']}: {assertion}")
    print(f"RH05 conformance: {len(cases)} scenarios passed through {args.adapter}")
else:
    print(f"RH05 conformance: {len(cases)} review vectors valid (execution needs --adapter)")
