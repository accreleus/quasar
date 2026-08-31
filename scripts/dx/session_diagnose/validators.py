#!/usr/bin/env python3
"""
validators.py — shared validation logic for the session-diagnosis tooling.

Imported by report_gen.py / ir_experiment_runner.py and by the quasar-diagnose
skill's `validate` self-test. Never run directly.
All functions raise ValueError with a clear human-readable message on failure.

This module does NOT resolve credentials and does NOT own a verdict vocabulary.
Credentials come from scripts/dx/admin_token.sh; the classifier's verdict strings
belong to the control plane (control-plane/internal/session/classifier.go) and are
passed through verbatim.
"""

import json
import os
import sys
from pathlib import Path

# The analysis code lives in scripts/dx/session_diagnose/; the ANALYSIS FACTS
# (thresholds, experiment matrices, netem shapes) still live with the skill.
# $QDIAG_CONFIG overrides for a test or a fixture.
DEFAULT_CONFIG = (
    Path(__file__).resolve().parents[3]
    / ".claude" / "skills" / "quasar-diagnose" / "config.json"
)


def load_config() -> dict:
    """Load the quasar-diagnose analysis config (config.json)."""
    cfg_path = Path(os.environ.get("QDIAG_CONFIG") or DEFAULT_CONFIG)
    if not cfg_path.exists():
        raise FileNotFoundError(f"config.json not found at {cfg_path}")
    with open(cfg_path) as f:
        return json.load(f)


# --------------------------------------------------------------------------- #
# Input validators                                                             #
# --------------------------------------------------------------------------- #

# --------------------------------------------------------------------------- #
# Bundle shape validator                                                       #
# --------------------------------------------------------------------------- #

def validate_bundle_shape(bundle: dict, cfg: dict) -> list[str]:
    """
    Validate the diagnostic bundle has the required §B.5 shape.
    Returns a list of warning strings (non-fatal oddities).
    Raises ValueError on hard failures (missing required keys, bad verdict).
    """
    warnings = []
    shape_cfg = cfg["bundle_shape"]

    # Required top-level keys.
    for key in shape_cfg["required_keys"]:
        if key not in bundle:
            raise ValueError(
                f"Bundle missing required key '{key}'. "
                f"Expected keys: {shape_cfg['required_keys']}"
            )

    # classifier block.
    classifier = bundle["classifier"]
    if not isinstance(classifier, dict):
        raise ValueError(f"classifier must be an object, got {type(classifier)}")
    for key in shape_cfg["classifier_required_keys"]:
        if key not in classifier:
            raise ValueError(f"classifier missing required key '{key}'")

    # The verdict is OPAQUE. Shape only: it must be present and be a string.
    # The control plane owns the vocabulary and has grown it twice (ST-07 #324
    # split "unknown" three ways); a copy of that enum here fails healthy
    # sessions the moment the two drift, which is exactly what happened on
    # 2026-08-22 when `nominal` was rejected as unknown and exited 2.
    verdict = classifier.get("verdict")
    if not isinstance(verdict, str) or not verdict:
        raise ValueError("classifier.verdict must be a non-empty string")

    # clock: either {"unmeasured": true} or a measured object.
    clock = bundle["clock"]
    if isinstance(clock, dict):
        if clock.get("unmeasured") is True:
            warnings.append("clock is unmeasured (no ping/pong round-trip recorded for this session)")
        elif "client_offset_ms" not in clock:
            warnings.append("clock object is present but missing client_offset_ms — may be a partial measurement")
    elif clock is not None:
        warnings.append(f"clock has unexpected type {type(clock).__name__}")

    # derived_windows keys.
    dw = bundle.get("derived_windows", {})
    for key in shape_cfg["derived_windows_keys"]:
        if key not in dw:
            warnings.append(f"derived_windows missing key '{key}' (expected by contract)")

    # series should be a dict.
    series = bundle.get("series", {})
    if not isinstance(series, dict):
        raise ValueError(f"series must be an object, got {type(series)}")

    return warnings


# --------------------------------------------------------------------------- #
# Run completeness validator                                                   #
# --------------------------------------------------------------------------- #

def validate_run_completeness(run: dict, cfg: dict) -> list[str]:
    """
    Validate a qdiag-sample run file meets the completeness bar.
    Returns warnings for soft failures, raises ValueError for hard failures.
    """
    warnings = []
    sampling = cfg["sampling"]

    meta = run.get("meta", {})
    samples = run.get("samples", [])
    n = len(samples)
    min_samples = sampling["min_samples"]
    requested_duration = meta.get("duration_s", sampling["duration_s"])

    if n < min_samples:
        raise ValueError(
            f"Run is degenerate: only {n} sample(s) collected, "
            f"minimum is {min_samples}. "
            f"Check that the session was alive and the bundle endpoint is reachable."
        )

    # Check actual time span.
    if n >= 2:
        first_ts = samples[0].get("ts_unix_s", 0)
        last_ts  = samples[-1].get("ts_unix_s", 0)
        span = last_ts - first_ts
        required_span = requested_duration * 0.8
        if span < required_span:
            warnings.append(
                f"Run span {span:.0f}s is below 80% of requested {requested_duration}s "
                f"(got {span:.0f}s, need {required_span:.0f}s) — results may be incomplete"
            )

    return warnings


# --------------------------------------------------------------------------- #
# Comparability validator                                                      #
# --------------------------------------------------------------------------- #

def validate_comparability(runs: list[dict], allow_cross: bool = False) -> list[str]:
    """
    Check that a set of run files are comparable (same app + profile + host).
    Returns warnings. Raises ValueError if incompatible and allow_cross is False.
    """
    warnings = []
    if len(runs) < 2:
        return warnings

    ref_meta = runs[0].get("meta", {})
    ref_app     = ref_meta.get("app", "?")
    ref_profile = ref_meta.get("profile", "?")
    ref_host    = ref_meta.get("host", "?")

    mismatches = []
    for i, run in enumerate(runs[1:], start=1):
        m = run.get("meta", {})
        if m.get("app")     != ref_app:
            mismatches.append(f"run[{i}] app '{m.get('app')}' != '{ref_app}'")
        if m.get("profile") != ref_profile:
            mismatches.append(f"run[{i}] profile '{m.get('profile')}' != '{ref_profile}'")
        if m.get("host")    != ref_host:
            mismatches.append(f"run[{i}] host '{m.get('host')}' != '{ref_host}'")

    if mismatches:
        msg = "Runs are not directly comparable:\n  " + "\n  ".join(mismatches)
        if allow_cross:
            warnings.append(msg + "\n  (--allow-cross passed; proceeding anyway)")
        else:
            raise ValueError(msg + "\n  Pass --allow-cross to compare anyway.")

    return warnings


# --------------------------------------------------------------------------- #
# IR experiment config validator (config.json "ir_experiment" block)          #
# --------------------------------------------------------------------------- #

def validate_experiment_config(cfg: dict, block: str = "ir_experiment") -> list[str]:
    """
    Validate a config.json experiment block (`ir_experiment` or `fec_experiment`)
    against the shared matrix schema (T1). The two blocks share one schema — rows
    with a whitelisted `env`, cols with `netem_args`, a `control_row`, and a
    `fixed_env` — so one validator covers both.

    Raises ValueError on hard schema failures. Returns warnings for soft issues.
    """
    warnings: list[str] = []
    if block not in cfg:
        raise ValueError(f"config.json missing '{block}' block")
    ir = cfg[block]

    for key in ("rows", "cols", "env_keys_whitelist", "fixed_env", "control_row"):
        if key not in ir:
            raise ValueError(f"{block} missing required key '{key}'")

    rows = ir["rows"]
    cols = ir["cols"]
    if not isinstance(rows, dict) or not rows:
        raise ValueError(f"{block}.rows must be a non-empty object")
    if not isinstance(cols, dict) or not cols:
        raise ValueError(f"{block}.cols must be a non-empty object")

    whitelist = set(ir["env_keys_whitelist"])
    if not whitelist:
        raise ValueError(f"{block}.env_keys_whitelist must be non-empty")

    for row_id, row in rows.items():
        if not isinstance(row, dict) or "env" not in row:
            raise ValueError(f"{block}.rows.{row_id} missing 'env'")
        env = row["env"]
        if not isinstance(env, dict) or not env:
            raise ValueError(f"{block}.rows.{row_id}.env must be a non-empty object")
        for env_key in env:
            if env_key not in whitelist:
                raise ValueError(
                    f"{block}.rows.{row_id}.env has key '{env_key}' "
                    f"not in env_keys_whitelist {sorted(whitelist)}"
                )
            if not isinstance(env[env_key], str):
                raise ValueError(
                    f"{block}.rows.{row_id}.env.{env_key} must be a string, "
                    f"got {type(env[env_key]).__name__}"
                )

    for col_id, col in cols.items():
        if not isinstance(col, dict) or "netem_args" not in col:
            raise ValueError(f"{block}.cols.{col_id} missing 'netem_args'")
        args = col["netem_args"]
        if args is not None and not isinstance(args, str):
            raise ValueError(
                f"{block}.cols.{col_id}.netem_args must be a string or null, "
                f"got {type(args).__name__}"
            )

    if ir["control_row"] not in rows:
        raise ValueError(
            f"{block}.control_row '{ir['control_row']}' is not a key in {block}.rows"
        )

    fixed_env = ir["fixed_env"]
    if not isinstance(fixed_env, dict):
        raise ValueError(f"{block}.fixed_env must be an object")
    for k, v in fixed_env.items():
        if not isinstance(v, str):
            warnings.append(f"{block}.fixed_env.{k} is not a string ({type(v).__name__})")

    return warnings


def validate_ir_experiment_config(cfg: dict) -> list[str]:
    """Back-compat wrapper: validate the `ir_experiment` block."""
    return validate_experiment_config(cfg, "ir_experiment")


def validate_fec_experiment_config(cfg: dict) -> list[str]:
    """Validate the `fec_experiment` block (FEC auto-arm matrix)."""
    return validate_experiment_config(cfg, "fec_experiment")


# --------------------------------------------------------------------------- #
# Percentile helper (mirrors classifier.go percentile, used in qdiag analysis) #
# --------------------------------------------------------------------------- #

def percentile(values: list[float], p: float) -> float:
    """Linear-interpolated p-th percentile (0..100). Returns NaN for empty."""
    import math
    if not values:
        return math.nan
    s = sorted(values)
    if len(s) == 1:
        return s[0]
    rank = (p / 100) * (len(s) - 1)
    lo   = int(rank)
    hi   = min(lo + 1, len(s) - 1)
    frac = rank - lo
    return s[lo] + frac * (s[hi] - s[lo])
