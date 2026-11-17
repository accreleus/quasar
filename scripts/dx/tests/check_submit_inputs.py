#!/usr/bin/env python3
"""Trust-boundary assertions for bench_submit.py (#1407); called by scripts/dx/tests/run.sh.

    check_submit_inputs.py <fixture run dir>

A run directory's conditions.json, caps-negotiated.json and verdict.json are REFUSED when they
carry a value the bench must not be sent. Prints "OK", or a `; `-joined list of problems.
"""
import copy
import json
import os
import shutil
import sys
import tempfile

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
import bench_submit as bs  # noqa: E402

src = sys.argv[1]
problems = []


def refused(label, fn):
    try:
        fn()
    except bs.UntrustedInput:
        return
    problems.append("%s: accepted, want UntrustedInput" % label)


def rundir(**docs):  # caps= / verdict= replace caps-negotiated.json / verdict.json
    d = tempfile.mkdtemp()
    for n in os.listdir(src):
        shutil.copy(os.path.join(src, n), d)
    for name, doc in docs.items():
        fname = {"caps": "caps-negotiated.json", "verdict": "verdict.json"}[name]
        with open(os.path.join(d, fname), "w") as fh:
            json.dump(doc, fh)
    return d


def conditions(d, path=None):
    session = json.load(open(os.path.join(d, "session.json")))
    return bs.build_conditions(d, session, [], {}, d, path)


# The unmodified fixture is accepted: the check must not break a good run.
try:
    conditions(rundir())
except bs.UntrustedInput as exc:
    problems.append("good fixture refused: %s" % exc)

# conditions.json: every shape the issue names, each its own refusal.
bad_cond = {
    "negotiated string too long": {"negotiated": {"codec": "h" * 600}},
    "bidi override in a value": {"effective": {"abr_mode": "smooth‮"}},
    "line separator in a value": {"effective": {"abr_mode": "a b"}},
    "control character in a value": {"effective": {"abr_mode": "a\nb"}},
    "key with a space": {"effective": {"abr mode": "smooth"}},
    "nested too deep": {"effective": {"a": {"b": {"c": {"d": {"e": 1}}}}}},
    "too many items": {"effective": {"k%d" % i: i for i in range(300)}},
    "not finite": {"effective": {"x": float("inf")}},
}
for label, doc in bad_cond.items():
    d = rundir()
    p = os.path.join(d, "cond.json")
    with open(p, "w") as fh:
        json.dump(doc, fh)
    refused("conditions.json " + label, lambda d=d, p=p: conditions(d, p))

# caps-negotiated.json values become tags and negotiated conditions.
for key in ("codec", "profile", "encoder_factory"):
    for bad in ("h264\n", "a b", "x" * 200, "h264‮", 7):
        caps = {"events": [{"payload": {key: bad}}]}
        d = rundir(caps=caps)
        refused("caps %s=%r" % (key, bad), lambda d=d: bs.read_caps_negotiated(d))

# verdict.json: the verdict is a token, the evidence becomes validity_reason.
refused("verdict with a space", lambda d=rundir(verdict={"verdict": "very bad"}): bs.read_verdict(d))
refused("evidence with a separator", lambda d=rundir(verdict={"verdict": "nominal", "evidence": ["a b"]}): bs.read_verdict(d))
refused("evidence too long", lambda d=rundir(verdict={"verdict": "nominal", "evidence": ["x" * 600]}): bs.read_verdict(d))
refused("evidence not a list", lambda d=rundir(verdict={"verdict": "nominal", "evidence": "nope"}): bs.read_verdict(d))

# suite and scenario names: no leading `-`, no spaces, bounded.
for ok in ("1080p60-h264-clean", "a", "x.y_z-1", "stream-hold/transport=webrtc/codec=h264/profile=1080p60"):
    try:
        bs.check_name(ok, "n")
    except bs.UntrustedInput:
        problems.append("name %r refused" % ok)
for bad in ("", "-rf", "--x", ".hidden", "a b", "a\nb", "x" * 201):
    refused("name %r" % bad, lambda bad=bad: bs.check_name(bad, "n"))

# End to end: main() refuses a bad scenario name before reading or posting anything.
try:
    bs.main(["--dir", src, "--suite", "s", "--scenario=-x", "--dry-run"])
    problems.append("main accepted --scenario -x")
except SystemExit as exc:
    if exc.code in (0, None):
        problems.append("main exit code %r for --scenario -x" % (exc.code,))

print("OK" if not problems else "; ".join(problems))
