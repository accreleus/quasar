#!/usr/bin/env python3
"""Build normalized documents from the committed fixtures, so the report
generator (_qimg_report.py) is testable with no host and no GPU. The
violating fixture is the real 2026-07-26 pre-fix image (6994MB, 33 contract
failures) -- the only specimen of the defect this whole skill exists to
catch.

Also exercises document-shape edge cases the real collector
(_qimg_collect.py) can legitimately produce:
  - numeric `disk` with possibly-null components (free_gb/images_gb/
    reclaimable_gb)
  - the host-level `running` live-check block: all-null (no agent
    container, or --fast), a mix of true/false/null, and outright false
    (the class of incident -- wrong image running, non-baked binary --
    this report exists to surface)
  - a host whose BASE section failed (so image_id/size_mb/created are
    null) while its role's contract still populated, because that role's
    own validate-image.sh call ran and parsed independently
  - a fully unreachable host (no disk/running/roles facts at all -- matches
    collect_host()'s early-return shape exactly)
  - `contract.size_max_mb` populated on two hosts (one under, one over its
    ceiling) so the size-ceiling bar's ratio/clamp/`over` class is actually
    exercised -- both real fixtures predate that field
  - `source_dirty` (tri-state) and a fuller provenance-label set (pins +
    built-at) on the two real-fixture hosts, with the `gwd` pin
    deliberately different between them -- exercises the dirty-flag badge,
    the provenance table, and pin-divergence drift detection

`--drift-only` builds a second, minimal document: every contract PASSES,
every host is reachable, live `running` matches -- but the same tag
resolves to two different image ids across two hosts. That isolates the
FIX-1 regression (drift must flip the exit code even when nothing else is
wrong) from the four-host fixture above, where `fixture-bad`'s contract
FAIL and `fixture-unreachable` already make the exit code 1 for other
reasons and so can't prove drift alone does anything.
"""
import json, os, sys

FIX = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                    "assets", "fixtures")

_UNSET = object()


def load(name):
    with open(os.path.join(FIX, name)) as f:
        return json.load(f)


def role_from(validate_json, role, tag, labels, created=None,
              image_id=_UNSET, size_mb=_UNSET, size_max_mb=_UNSET,
              source_dirty=None):
    """Build a roles[] entry from a validate-image.sh --json payload.
    image_id/size_mb/size_max_mb default to the payload's own values but
    can be forced to a real `None` (via an explicit keyword) to model the
    degraded-BASE case where per-image docker-inspect facts are
    unavailable, or forced to a real value to model a ceiling being
    configured (the fixtures' own validate-image.sh captures predate the
    size_max_mb field)."""
    return {
        "role": role, "tag": tag,
        "image_id": (validate_json.get("image_id", "sha256:unknown")
                      if image_id is _UNSET else image_id),
        "size_mb": (validate_json["size_mb"] if size_mb is _UNSET else size_mb),
        "created": created,
        "labels": labels,
        "source_dirty": source_dirty,
        "contract": {
            "verdict": validate_json["verdict"],
            "passed": validate_json["passed"],
            "failed": validate_json["failed"],
            "gpu_attached": validate_json.get("gpu_attached", False),
            "size_max_mb": (validate_json.get("size_max_mb")
                             if size_max_mb is _UNSET else size_max_mb),
            "assertions": validate_json.get("assertions", []),
        },
    }


def build_doc():
    good = load("validate-nv-passing.json")
    bad = load("validate-nv-violating.json")

    return {"schema": 1, "generated_at": "2026-07-27T00:00:00Z", "hosts": [
        # Healthy host: clean contract, live agent matches :latest, baked
        # binary, pulse daemon present. Everything green. size_max_mb=2300
        # -- 1786MB is comfortably UNDER ceiling (the size bar's normal,
        # non-"over" path).
        {"name": "fixture-good", "gpu": "nvidia", "dir": "/srv/quasar",
         "reachable": True, "error": None,
         "disk": {"free_gb": 33.0, "images_gb": 12.3, "reclaimable_gb": 4.08},
         "running": {"container": "quasar-node-agent",
                      "image_id": "sha256:47d971decfe9",
                      "matches_latest": True,
                      "agent_binary": "/usr/local/bin/quasar-node-agent",
                      "baked": True,
                      "pulse_image": "quasar-nv:latest",
                      "pulse_has_daemon": True},
         "roles": [role_from(good, "nv", "quasar-nv:latest",
                              {"org.quasar.pins.gwd": "b202563",
                               "org.quasar.pins.gst": "1.28.4",
                               "org.quasar.pins.base":
                                   "ghcr.io/example/quasar-base:develop",
                               "org.quasar.source.commit": "2f17889",
                               "org.quasar.built.at": "2026-07-26T16:00:00Z"},
                              created="2026-07-26T16:08:00Z",
                              size_max_mb=2300, source_dirty=False)]},

        # The specimen: the real 6994MB / 33-failure pre-fix image. Its live
        # agent is running a stale, non-baked binary that doesn't match
        # :latest -- exactly the class of incident this report exists to
        # catch. Pulse-daemon presence is unknown here (null) so the
        # false/null mix both render. size_max_mb=2300 -- 6994MB is well
        # OVER ceiling (the size bar's `over` path). `gwd` pin deliberately
        # differs from fixture-good's (43d4c25 vs b202563) so pin-divergence
        # drift detection has a real specimen. source_dirty=True models the
        # actual incident: built from an uncommitted tree.
        {"name": "fixture-bad", "gpu": "nvidia", "dir": "/srv/quasar",
         "reachable": True, "error": None,
         "disk": {"free_gb": 28.0, "images_gb": None, "reclaimable_gb": None},
         "running": {"container": "quasar-node-agent",
                      "image_id": "sha256:ddc39415aea5",
                      "matches_latest": False,
                      "agent_binary": "/workspace/target/debug/quasar-node-agent",
                      "baked": False,
                      "pulse_image": "quasar-nv:latest",
                      "pulse_has_daemon": None},
         "roles": [role_from(bad, "nv", "quasar-nv:latest",
                              {"org.quasar.pins.gwd": "43d4c25",
                               "org.quasar.pins.gst": "1.28.4",
                               "org.quasar.pins.base":
                                   "ghcr.io/example/quasar-base:develop",
                               "org.quasar.source.commit": "1e92ea2",
                               "org.quasar.built.at": "2026-07-26T13:10:07Z"},
                              created="2026-07-26T16:08:00Z",
                              size_max_mb=2300, source_dirty=True)]},

        # Degraded BASE section: the host's own validate-image.sh call for
        # this role still ran and parsed (contract populated with real
        # verdict/counts), but per-image docker-inspect facts never came
        # back -- image_id/size_mb/created are null. Must not crash.
        # `running` is the ordinary "no agent container" all-null shape.
        # No size_max_mb/source_dirty override -- both stay null/unset,
        # exercising "unknown" rendering for the new columns too.
        {"name": "fixture-degraded", "gpu": "nvidia", "dir": "/srv/quasar",
         "reachable": True,
         "error": "unparseable BASE section: Expecting value: line 1 column 1 (char 0)",
         "disk": {"free_gb": None, "images_gb": None, "reclaimable_gb": None},
         "running": {"container": None, "image_id": None, "matches_latest": None,
                      "agent_binary": None, "baked": None,
                      "pulse_image": None, "pulse_has_daemon": None},
         "roles": [role_from(bad, "nv", "quasar-nv:latest", {},
                              created=None, image_id=None, size_mb=None)]},

        # Fully unreachable host -- collect_host()'s early-return shape has
        # no "gpu"/"disk"/"running" keys at all, only name/reachable/error/
        # roles.
        {"name": "fixture-unreachable", "reachable": False,
         "error": "ssh failed (rc=255)", "roles": []},
    ]}


def build_drift_only_doc():
    """Minimal two-host document: both contracts PASS, both hosts
    reachable, both `running` blocks match cleanly -- the ONLY problem is
    that the same tag resolves to two different image ids. Isolates the
    FIX-1 regression (a fleet where every contract passes and every host
    is reachable, but the same tag resolves to different image ids across
    hosts, must still exit 1)."""
    good = load("validate-nv-passing.json")

    def clean_host(name, image_id):
        return {
            "name": name, "gpu": "nvidia", "dir": "/srv/quasar",
            "reachable": True, "error": None,
            "disk": {"free_gb": 33.0, "images_gb": 12.3, "reclaimable_gb": 4.08},
            "running": {"container": "quasar-node-agent",
                         "image_id": image_id,
                         "matches_latest": True,
                         "agent_binary": "/usr/local/bin/quasar-node-agent",
                         "baked": True,
                         "pulse_image": "quasar-nv:latest",
                         "pulse_has_daemon": True},
            "roles": [role_from(good, "nv", "quasar-nv:latest",
                                 {"org.quasar.pins.gwd": "b202563",
                                  "org.quasar.source.commit": "2f17889"},
                                 created="2026-07-26T16:08:00Z",
                                 image_id=image_id, source_dirty=False)],
        }

    return {"schema": 1, "generated_at": "2026-07-27T00:00:00Z", "hosts": [
        clean_host("drift-host-a", "sha256:aaaaaaaaaaaabbbbccccdddd"),
        clean_host("drift-host-b", "sha256:bbbbbbbbbbbbccccddddeeee"),
    ]}


def main():
    doc = build_drift_only_doc() if "--drift-only" in sys.argv[1:] else build_doc()
    json.dump(doc, sys.stdout, indent=2)


if __name__ == "__main__":
    main()
