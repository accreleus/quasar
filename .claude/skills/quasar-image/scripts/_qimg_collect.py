#!/usr/bin/env python3
"""Collect image facts from a Quasar host into the normalized document
_qimg_report.py and _qimg_diff.py consume. This is the ONLY module that
touches a host; keeping it separate is what makes reporting testable from
fixtures with no host and no GPU.

Round trips per host: exactly ONE (spec R4). A single remote bash script
gathers disk + image facts, the host's `deploy/.build-report.json` (if any),
the live "running" checks, and every present role's
`deploy/validate-image.sh --json` result in one SSH session, each chunk
tagged with a unique delimiter line so this module can split the combined
stdout locally. --fast skips the validate-image.sh calls (roles[].contract
stays null) and the live running checks (running stays present but
all-null) -- it does not skip the disk/image round trip or the build-report
read (both are cheap local reads, not live probes).
"""
import argparse, json, os, re, shlex, subprocess, sys, datetime

SKILL_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SHARED = os.path.join(os.path.dirname(SKILL_DIR), "_shared")

# Fixed (not per-run-random) nonce: cheap to grep for in a debug capture, and
# collision with real command output is not a realistic concern -- FAIL lines,
# JSON payloads and docker inspect output never contain "@@QIMG9f3d1c:".
_DELIM = "QIMG9f3d1c"


def cfg():
    with open(os.path.join(SKILL_DIR, "config.json")) as f:
        return json.load(f)


def json_quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def sh(host, command):
    """Run a command on `host` via the shared q_ssh helper. Returns (rc, stdout).

    `host` is shell-quoted (shlex.quote), never interpolated raw -- it can be
    attacker/typo-controlled CLI input (`--host`), and this string is handed
    to `bash -c`, so an unquoted host like `x"; echo INJECTED >&2; "` would
    otherwise break out of the intended single argument (FIX 3).

    Raises subprocess.TimeoutExpired if the remote command runs longer than
    10 minutes -- `ssh -o ConnectTimeout=15` (inside q_ssh) only bounds
    connection setup, not execution, and the default (non---fast) path runs
    containers on the remote host, so a genuine hang must not wedge the whole
    collection with no output at all (FIX 4). Callers must catch it."""
    wrapper = 'source "%s/lib.sh"; q_ssh %s %s' % (
        SHARED, shlex.quote(host), json_quote(command))
    p = subprocess.run(["bash", "-c", wrapper], capture_output=True, text=True,
                        timeout=600)
    return p.returncode, p.stdout


def q(host_shell_cmd):
    """Run a one-liner against _shared/lib.sh with no SSH (local helper lookup)."""
    p = subprocess.run(["bash", "-c", 'source "%s/lib.sh"; %s' % (SHARED, host_shell_cmd)],
                        capture_output=True, text=True)
    return p.stdout.strip()


def extract_json_object(text, required_keys=("verdict", "passed", "failed")):
    """Pull the first well-formed JSON object out of `text` that structurally
    looks like a validate-image.sh contract result (has every key in
    `required_keys`), tolerating arbitrary leading/trailing plain-text noise.
    `deploy/validate-image.sh --quiet` still prints FAIL lines *before* the
    --json payload (that's exactly the violating-image case this skill exists
    to catch) and an unconditional human summary line after it.

    A decodable `{...}` that lacks the required keys is NOT the contract
    result -- e.g. one of the payload's own nested `{"status":...}` assertion
    objects would parse cleanly on its own. Keep scanning for the next `{`
    rather than returning a structurally-wrong object. Returns None (never
    raises) if no valid object is found anywhere in `text`."""
    dec = json.JSONDecoder()
    i = text.find("{")
    while i != -1:
        try:
            obj, _end = dec.raw_decode(text, i)
        except ValueError:
            i = text.find("{", i + 1)
            continue
        if isinstance(obj, dict) and all(k in obj for k in required_keys):
            return obj
        i = text.find("{", i + 1)
    return None


_SIZE_RE = re.compile(r'([\d.]+)\s*([KMGT]?B)', re.IGNORECASE)
_UNIT_POWER = {"B": 0, "KB": 1, "MB": 2, "GB": 3, "TB": 4}


def parse_size_gb(text):
    """Parse a docker-style human size string ('4.077GB (33%)', '12.3GB',
    '151.6GB') into a float number of GB. Returns None if `text` is missing,
    not a string, or the regex doesn't match -- never raises."""
    if not isinstance(text, str):
        return None
    m = _SIZE_RE.search(text)
    if not m:
        return None
    try:
        value = float(m.group(1))
    except ValueError:
        return None
    power = _UNIT_POWER.get(m.group(2).upper())
    if power is None:
        return None
    return value * (1024 ** (power - 3))


def _round_or_none(x):
    return round(x, 2) if isinstance(x, (int, float)) else None


def compute_disk(raw):
    """Transform the raw `docker system df --format json` row (+ a separate
    `df -Pk` free-space read) into the documented
    {"free_gb", "images_gb", "reclaimable_gb"} shape. Any missing or
    unparseable component becomes null; this never raises -- a host with a
    weird/absent docker or df output still produces a valid document."""
    row = raw.get("disk")
    images_gb = parse_size_gb(row.get("Size")) if isinstance(row, dict) else None
    reclaimable_gb = parse_size_gb(row.get("Reclaimable")) if isinstance(row, dict) else None

    free_kb = raw.get("disk_free_kb")
    free_gb = None
    try:
        if free_kb is not None:
            free_gb = float(free_kb) / (1024 * 1024)
    except (TypeError, ValueError):
        free_gb = None

    return {"free_gb": _round_or_none(free_gb),
            "images_gb": _round_or_none(images_gb),
            "reclaimable_gb": _round_or_none(reclaimable_gb)}


def _null_running():
    return {"container": None, "image_id": None, "matches_latest": None,
            "agent_binary": None, "baked": None,
            "pulse_image": None, "pulse_has_daemon": None}


def parse_running(text):
    """Parse the RUNNING section body (our own clean JSON emission -- no
    validate-image.sh noise to strip here) into the documented `running`
    shape. Degrades to all-null on anything unexpected; never raises. A host
    with no agent container running is a normal, expected outcome (the
    section is a literal `null`), not an error."""
    t = (text or "").strip()
    if not t or t == "null":
        return _null_running()
    try:
        obj = json.loads(t)
    except ValueError:
        return _null_running()
    if not isinstance(obj, dict):
        return _null_running()
    out = _null_running()
    for k in out:
        if k in obj:
            out[k] = obj[k]
    return out


def get_contract(section_text):
    """Populate roles[].contract from a role's validate-image.sh --quiet
    --json /dev/stdout output, tolerating the FAIL-line noise the script
    prints before (and the summary line it prints after) the JSON payload.
    `size_max_mb` (Task 2 Step 6) must pass through untouched -- it is the
    report's only legitimate source for the size-ceiling bar; the ceiling
    must never be re-derived by parsing the human-readable "size" assertion
    detail string."""
    obj = extract_json_object(section_text or "")
    if obj is None:
        return None
    return {k: obj.get(k) for k in
            ("verdict", "passed", "failed", "gpu_attached", "size_max_mb", "assertions")}


def parse_build_report(text):
    """Parse the BUILDREPORT section (a `cat` of the host's
    `deploy/.build-report.json`, or the literal `null` when the file is
    absent) into a dict, or None. An image built before this tooling existed
    has no receipt at all -- that is normal, not an error -- so this degrades
    to None on anything that isn't a well-formed JSON object; it never
    raises."""
    t = (text or "").strip()
    if not t or t == "null":
        return None
    try:
        obj = json.loads(t)
    except ValueError:
        return None
    return obj if isinstance(obj, dict) else None


def apply_build_report(out_roles, section_text):
    """Populate roles[].source_dirty (Task 4 review gap) from the host's
    build-report receipt, matched by role name -- a receipt only describes
    the generation it was written for, and `build-images.sh` can be run for
    a subset of roles, so a report naming only "nv" must never bleed its
    source_dirty onto "runtime". When BOTH the receipt and the collected
    role carry an image_id and they disagree, the receipt is stale (the tag
    has since been rebuilt or retagged some other way) and is left
    unapplied; when either side's image_id is unknown (e.g. a degraded-BASE
    role, Task 4's "per-image docker-inspect facts unavailable" case) there
    is nothing to cross-check, so the role-name match alone is trusted --
    consistent with how a degraded BASE section already trusts that role's
    own independently-parsed contract. Mutates `out_roles` in place; each
    entry must already carry a `source_dirty` key (default None) so a
    missing/unmatched receipt is a no-op, not a KeyError."""
    report = parse_build_report(section_text)
    if report is None:
        return
    dirty = report.get("source_dirty")
    if not isinstance(dirty, bool):
        return
    by_role = {r.get("role"): r for r in report.get("roles", []) if isinstance(r, dict)}
    for role_entry in out_roles:
        receipt = by_role.get(role_entry.get("role"))
        if receipt is None:
            continue
        receipt_id = receipt.get("image_id")
        collected_id = role_entry.get("image_id")
        if receipt_id and collected_id and receipt_id != collected_id:
            continue
        role_entry["source_dirty"] = dirty


# ── The one remote script, run once per host ────────────────────────────────
#
# Emits, in order, three kinds of delimited section so this module can split
# stdout locally without a second round trip:
#   BASE           disk facts + per-tag `docker image inspect` results
#   RUNNING        the live agent-container checks (or `null`)
#   ROLE:<role>    one per configured role, the raw validate-image.sh output
#                  (FAIL-line noise + JSON + summary), or empty if the role's
#                  image tag isn't present on the host, or SKIP_LIVE=1
#
# `jstr` centralizes JSON string-quoting for the RUNNING section via `jq`
# (already a hard dependency of deploy/validate-image.sh, so assuming it's on
# PATH here is not a new requirement).
_REMOTE_TMPL = r'''
set -u
cd "$DIR" 2>/dev/null || { echo '{"error":"repo dir not found"}'; exit 0; }

jstr() { if [ -z "${1:-}" ]; then printf 'null'; else jq -Rn --arg v "$1" '$v'; fi; }

echo '@@%(delim)s:BASE@@'
printf '{'
_dfjson="$(docker system df --format json 2>/dev/null | head -1)"
printf '"disk":%%s' "${_dfjson:-null}"
printf ','
printf '"disk_free_kb":'
_docker_root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)"
_free_kb="$(df -Pk "$_docker_root" 2>/dev/null | awk 'NR==2 {print $4}')"
if [ -n "${_free_kb:-}" ]; then printf '%%s' "$_free_kb"; else printf 'null'; fi
printf ',"images":['
first=1
for tag in $TAGS; do
  docker image inspect "$tag" >/dev/null 2>&1 || continue
  [ $first -eq 1 ] || printf ','
  first=0
  # tr -d '\n' (FIX 1b): `docker image inspect --format` appends its own
  # trailing newline per invocation, so N present tags emit an N-line BASE
  # body whose last line alone is just the array's closing `]}` -- fold each
  # per-image object back onto one line so BASE stays single-line regardless
  # of how many tags are present (the python side also parses this
  # defensively via extract_json_object, but the wire shape should not
  # depend on that).
  docker image inspect --format \
    '{"tag":"{{.RepoTags}}","id":"{{.Id}}","size":{{.Size}},"created":"{{.Created}}","labels":{{json .Config.Labels}}}' \
    "$tag" | tr -d '\n'
done
printf ']}'
echo

echo '@@%(delim)s:BUILDREPORT@@'
if [ -f "deploy/.build-report.json" ]; then
  cat "deploy/.build-report.json"
else
  printf 'null'
fi
echo

echo '@@%(delim)s:RUNNING@@'
if [ "$SKIP_LIVE" = "1" ]; then
  printf 'null'
else
  CONTAINER="$(docker ps --filter "name=$AGENT_FILTER" --format '{{.Names}}' 2>/dev/null | head -1)"
  if [ -z "${CONTAINER:-}" ]; then
    printf 'null'
  else
    IMAGE_ID="$(docker inspect --format '{{.Image}}' "$CONTAINER" 2>/dev/null || true)"
    IMAGE_REF="$(docker inspect --format '{{.Config.Image}}' "$CONTAINER" 2>/dev/null || true)"
    # FIX 10a: bash longest-suffix-match strips from the FIRST colon onward
    # -- wrong when IMAGE_REF carries a registry host:port (e.g.
    # "registry.example.com:5000/quasar-nv:latest" would wrongly yield
    # "registry.example.com"). Shortest-suffix-match strips only the
    # trailing ":<tag>", correctly leaving any registry port intact.
    REPO="${IMAGE_REF%%:*}"
    LATEST_ID=""
    if [ -n "$REPO" ]; then
      LATEST_ID="$(docker image inspect --format '{{.Id}}' "${REPO}:latest" 2>/dev/null || true)"
    fi
    if [ -n "$LATEST_ID" ] && [ -n "${IMAGE_ID:-}" ]; then
      if [ "$IMAGE_ID" = "$LATEST_ID" ]; then MATCHES=true; else MATCHES=false; fi
    else
      MATCHES=null
    fi

    # PID 1 in the container is docker-init (tini), never the agent -- find the
    # agent process by (full cmdline, not truncated 15-char `comm`) pattern.
    AGENT_PID="$(docker exec "$CONTAINER" sh -c 'pgrep -f quasar-node-agent 2>/dev/null | head -1' 2>/dev/null || true)"
    AGENT_BIN=""
    if [ -n "${AGENT_PID:-}" ]; then
      AGENT_BIN="$(docker exec "$CONTAINER" sh -c "readlink -f /proc/$AGENT_PID/exe 2>/dev/null" 2>/dev/null || true)"
    fi
    if [ -n "$AGENT_BIN" ]; then
      if [ "$AGENT_BIN" = "/usr/local/bin/quasar-node-agent" ]; then BAKED=true; else BAKED=false; fi
    else
      BAKED=null
    fi

    PULSE_IMAGE="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$CONTAINER" 2>/dev/null | sed -n 's/^QUASAR_PULSE_IMAGE=//p' | head -1)"
    PULSE_IMAGE="${PULSE_IMAGE:-$NV_LATEST_TAG}"
    if [ -n "${PULSE_IMAGE:-}" ] && docker image inspect "$PULSE_IMAGE" >/dev/null 2>&1; then
      if docker run --rm --entrypoint sh "$PULSE_IMAGE" -c 'command -v pulseaudio' >/dev/null 2>&1; then
        PULSE_HAS_DAEMON=true
      else
        PULSE_HAS_DAEMON=false
      fi
    else
      PULSE_HAS_DAEMON=null
    fi

    printf '{"container":%%s,"image_id":%%s,"matches_latest":%%s,"agent_binary":%%s,"baked":%%s,"pulse_image":%%s,"pulse_has_daemon":%%s}' \
      "$(jstr "$CONTAINER")" "$(jstr "${IMAGE_ID:-}")" "$MATCHES" "$(jstr "$AGENT_BIN")" "$BAKED" "$(jstr "${PULSE_IMAGE:-}")" "$PULSE_HAS_DAEMON"
  fi
fi
echo

for pair in $ROLE_TAG_PAIRS; do
  role="${pair%%%%:*}"
  tag="${pair#*:}"
  echo "@@%(delim)s:ROLE:$role@@"
  if [ "$SKIP_LIVE" != "1" ] && docker image inspect "$tag" >/dev/null 2>&1; then
    deploy/validate-image.sh --role "$role" --image "$tag" --quiet --json /dev/stdout "$GPU_FLAG" 2>/dev/null || true
  fi
  echo
done
echo '@@%(delim)s:END@@'
'''
REMOTE = _REMOTE_TMPL % {"delim": _DELIM}

_MARKER_RE = re.compile(r'@@' + re.escape(_DELIM) + r':([A-Z]+)(?::([^@]*))?@@\n?')


def split_sections(out):
    """Split the combined one-ssh-round-trip stdout into named sections
    (BASE, RUNNING, ROLE:<name>). Returns {} if no markers are present at all
    (e.g. the remote script's own early exit on "repo dir not found" prints
    its error JSON before ever reaching the first marker) -- callers fall
    back to treating the whole blob as a single JSON error document."""
    parts = _MARKER_RE.split(out)
    sections = {}
    n_matches = (len(parts) - 1) // 3
    for i in range(n_matches):
        kind = parts[1 + 3 * i]
        sub = parts[2 + 3 * i]
        body = parts[3 + 3 * i]
        key = kind if sub is None else "%s:%s" % (kind, sub)
        sections[key] = body
    return sections


def die(msg):
    sys.stderr.write("_qimg_collect: %s\n" % msg)
    sys.exit(2)


def collect_host(name, roles, all_roles, agent_filter, fast):
    # `name` is shell-quoted (FIX 3) -- it comes straight from --host on the
    # CLI, so an unquoted embed here is the same injection class as `sh()`'s
    # host argument, just one hop earlier (a local lib.sh lookup rather than
    # a remote command).
    gpu = q('q_host_cfg %s gpu' % shlex.quote(name))
    hdir = q('q_host_cfg %s dir' % shlex.quote(name))
    if not hdir:
        return {"name": name, "reachable": False, "error": "unknown host", "roles": []}

    tags = " ".join(v["tag"] + ":latest" for v in roles.values())
    role_tag_pairs = " ".join("%s:%s:latest" % (r, v["tag"]) for r, v in roles.items())
    gpu_flag = "--no-gpu" if (not gpu or gpu == "none") else "--gpu"
    # The pulse-image fallback must resolve from config.json's FULL role map,
    # never a literal in this script -- `roles` here may be --role-filtered
    # (e.g. --role runtime excludes "nv"), so look the nv tag up in
    # `all_roles` (the unfiltered map from cfg()) instead. If a config has no
    # "nv" role at all, fall back to "" (empty, not a hardcoded tag name) --
    # the remote script already treats an empty NV_LATEST_TAG as "no fallback".
    nv_meta = all_roles.get("nv") or {}
    nv_tag = (nv_meta["tag"] + ":latest") if nv_meta.get("tag") else ""

    env_prefix = 'DIR=%s TAGS=%s ROLE_TAG_PAIRS=%s AGENT_FILTER=%s NV_LATEST_TAG=%s GPU_FLAG=%s SKIP_LIVE=%s bash -s' % (
        json_quote(hdir), json_quote(tags), json_quote(role_tag_pairs),
        json_quote(agent_filter or ""), json_quote(nv_tag), gpu_flag,
        "1" if fast else "0")
    try:
        rc, out = sh(name, env_prefix + " <<'EOS'\n" + REMOTE + "\nEOS")
    except subprocess.TimeoutExpired:
        # FIX 4: ConnectTimeout only bounds connection setup; a wedged remote
        # command (e.g. a hung `docker run` during the default/non---fast
        # path) must surface as a normal per-host failure, not hang the whole
        # fleet collection with no output at all.
        return {"name": name, "reachable": False, "error": "timed out", "roles": []}
    if rc != 0:
        return {"name": name, "reachable": False,
                "error": "ssh failed (rc=%d)" % rc, "roles": []}

    sections = split_sections(out)
    if "BASE" not in sections:
        # No markers at all -- either the remote script's own early-exit error
        # JSON (e.g. "repo dir not found", which exits before RUNNING/ROLE ever
        # run -- nothing was collected), or genuinely unparseable output.
        # This is a real "could not use this host" failure, unlike a malformed
        # BASE section below (where RUNNING/ROLE markers ARE present).
        try:
            raw = json.loads(out.strip().splitlines()[-1]) if out.strip() else {}
        except Exception as e:
            return {"name": name, "reachable": False,
                    "error": "unparseable host output: %s" % e, "roles": []}
        return {"name": name, "reachable": False,
                "error": raw.get("error") or "no output from host", "roles": []}

    # A malformed/erroring BASE section is NOT a host-unreachable failure --
    # we already reached the host and (per the markers found) ran RUNNING and
    # the per-role validate-image.sh calls too. Degrade the base facts (disk
    # -> null, image metadata unavailable) and record the problem in `error`,
    # but keep reporting whatever RUNNING/ROLE sections did parse rather than
    # discarding a good session's worth of data over one bad section.
    # FIX 1a: BASE's body is NOT one line -- each per-tag `docker image
    # inspect --format ...` call in the remote loop emits its own trailing
    # newline, so a host with >1 present tag produces a multi-line body whose
    # LAST line is just the closing `]}` of the images array. The old
    # `.splitlines()[-1]` grabbed that closing fragment alone and always
    # failed to parse, silently degrading every real collection to
    # base_error + an empty `by_tag` (roles: image_id/size_mb/created/labels
    # all null) -- see also the `tr -d '\n'` fix on the remote-script side
    # below, which removes the embedded newlines at the source; this parser
    # fix stands on its own regardless (a decoder that tolerates embedded
    # newlines/noise, same as `get_contract` already does for ROLE sections).
    base_error = None
    raw = {}
    try:
        parsed = extract_json_object(sections["BASE"], required_keys=("images",))
        if parsed is None:
            raise ValueError("no JSON object with an 'images' key found")
        raw = parsed
        if isinstance(raw, dict) and raw.get("error"):
            base_error = raw["error"]
            raw = {}
    except Exception as e:
        base_error = "unparseable BASE section: %s" % e
        raw = {}

    by_tag = {}
    if not base_error:
        for img in raw.get("images", []):
            for t in img.get("tag", "").strip("[]").split():
                by_tag[t] = img

    out_roles = []
    for role, meta in roles.items():
        tag = meta["tag"] + ":latest"
        img = by_tag.get(tag)
        role_section = sections.get("ROLE:%s" % role, "")
        if img:
            contract = None if fast else get_contract(role_section)
            out_roles.append({
                "role": role,
                "tag": tag,
                "image_id": img["id"],
                "size_mb": int(img["size"]) // 1024 // 1024,
                "created": img.get("created"),
                "labels": img.get("labels") or {},
                "contract": contract,
                "source_dirty": None,
            })
        elif base_error and role_section.strip():
            # BASE failed, so per-image facts (id/size/created/labels) are
            # unavailable -- but this role's own validate-image.sh section
            # still ran and parsed on the host. Report the degraded entry
            # (contract present, image metadata null) instead of dropping it.
            contract = None if fast else get_contract(role_section)
            out_roles.append({
                "role": role,
                "tag": tag,
                "image_id": None,
                "size_mb": None,
                "created": None,
                "labels": {},
                "contract": contract,
                "source_dirty": None,
            })
        # else: image tag not present on this host (the normal case) -- skip,
        # same as before.

    apply_build_report(out_roles, sections.get("BUILDREPORT", ""))

    return {"name": name, "gpu": gpu, "dir": hdir, "reachable": True,
            "error": base_error, "disk": compute_disk(raw), "roles": out_roles,
            "running": parse_running(sections.get("RUNNING", ""))}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", action="append", default=[])
    ap.add_argument("--role", action="append", default=[])
    ap.add_argument("--all", action="store_true")
    ap.add_argument("--fast", action="store_true",
                     help="skip deploy/validate-image.sh and the live running "
                          "checks; roles[].contract stays null and running "
                          "stays present but all-null")
    args = ap.parse_args()

    c = cfg()
    all_roles = c["roles"]
    roles = all_roles
    if args.role:
        # FIX 5: an unknown/typo'd --role must fail loudly. Filtering with a
        # bad name silently produced an empty role map -- every host still
        # came back "reachable" with zero roles and this script still
        # exited 0, a silent wrong answer in a tool that exists to catch
        # exactly this class of mistake.
        unknown = sorted(set(args.role) - set(all_roles))
        if unknown:
            die("unknown role(s): %s (known: %s)" %
                (", ".join(unknown), ", ".join(sorted(all_roles))))
        roles = {r: v for r, v in all_roles.items() if r in args.role}

    hosts = args.host
    if args.all or not hosts:
        hosts = q("q_hosts").split()

    doc = {"schema": 1,
           "generated_at": datetime.datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ"),
           "hosts": [collect_host(h, roles, all_roles, c["agent_container_filter"], args.fast)
                     for h in hosts]}
    json.dump(doc, sys.stdout, indent=2)
    print()


if __name__ == "__main__":
    main()
