#!/usr/bin/env python3
"""
_ir_env.py — pure .env mutation helper for the IR-period experiment (T4).

apply_env() is a pure function: given the current .env text and a dict of
KEY=VALUE updates, it returns the new text with those keys idempotently set
(existing `KEY=...` lines are replaced in place; unknown keys are appended).
All unrelated lines, comments, and blank lines are preserved byte-for-byte.

Used two ways:
  1. Imported by qdiag-ir-experiment (local) and scripts/validate (unit test).
  2. Executed standalone over SSH (same trick as _qdiag_sample_runner.py —
     `ssh ... "python3 /dev/stdin <path> KEY=VAL [KEY=VAL ...]" < _ir_env.py`)
     to mutate deploy/.env on the target host without shipping a remote-only script.

CLI:
  python3 _ir_env.py <path> KEY=VALUE [KEY=VALUE ...]     # apply + write back
  python3 _ir_env.py --restore <path> <original-text-file>  # write back original text
  python3 _ir_env.py --print <path> KEY [KEY ...]         # print current values (for Gate A)
"""

import sys
from pathlib import Path


def apply_env(text: str, updates: dict) -> str:
    """
    Return `text` with each key in `updates` idempotently set to its value.

    - A line `KEY=...` (not a comment) for a key in `updates` is replaced with
      `KEY=value`.
    - Keys in `updates` not found as an existing line are appended at the end.
    - All other lines (comments, blanks, unrelated vars) pass through unchanged.
    - Calling apply_env(apply_env(text, u), u) == apply_env(text, u) (idempotent).
    """
    updates = dict(updates)
    trailing_newline = text.endswith("\n")
    lines = text.split("\n")
    if trailing_newline and lines and lines[-1] == "":
        lines = lines[:-1]

    out = []
    seen = set()
    for line in lines:
        stripped = line.strip()
        matched_key = None
        if stripped and not stripped.startswith("#") and "=" in stripped:
            key = stripped.split("=", 1)[0].strip()
            if key in updates:
                matched_key = key
        if matched_key is not None:
            out.append(f"{matched_key}={updates[matched_key]}")
            seen.add(matched_key)
        else:
            out.append(line)

    for key, val in updates.items():
        if key not in seen:
            out.append(f"{key}={val}")

    result = "\n".join(out)
    if trailing_newline:
        result += "\n"
    return result


def read_env(text: str, keys: list) -> dict:
    """Read the current value of each key in `keys` from `text` (None if absent)."""
    values = {k: None for k in keys}
    for line in text.split("\n"):
        stripped = line.strip()
        if not stripped or stripped.startswith("#") or "=" not in stripped:
            continue
        key, _, val = stripped.partition("=")
        key = key.strip()
        if key in values:
            values[key] = val.strip().strip('"').strip("'")
    return values


def _main(argv: list) -> int:
    if not argv:
        print(__doc__, file=sys.stderr)
        return 2

    if argv[0] == "--restore":
        path, orig_path = argv[1], argv[2]
        Path(path).write_text(Path(orig_path).read_text())
        print(f"restored {path} from {orig_path}")
        return 0

    if argv[0] == "--print":
        path = argv[1]
        keys = argv[2:]
        text = Path(path).read_text()
        values = read_env(text, keys)
        for k in keys:
            print(f"{k}={values[k]}")
        return 0

    path = argv[0]
    updates = {}
    for pair in argv[1:]:
        if "=" not in pair:
            print(f"bad KEY=VALUE arg: {pair}", file=sys.stderr)
            return 2
        k, _, v = pair.partition("=")
        updates[k] = v

    p = Path(path)
    before = p.read_text() if p.exists() else ""
    after = apply_env(before, updates)
    p.write_text(after)
    for k, v in updates.items():
        print(f"set {k}={v}")
    return 0


if __name__ == "__main__":
    sys.exit(_main(sys.argv[1:]))
