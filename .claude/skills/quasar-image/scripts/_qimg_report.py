#!/usr/bin/env python3
"""Render the normalized document (schema 1, produced by _qimg_collect.py)
as a self-contained HTML fleet report. Pure: no host access, no subprocess
-- that boundary is what makes it testable from fixtures with no host and
no GPU.

Exit code: 1 when any role's contract verdict is FAIL, a host is
unreachable, a reachable host's live `running` check shows a real
mismatch (running image doesn't match :latest / agent binary isn't the
baked one / pulse image has no pulseaudio daemon), OR fleet drift is
detected (tag->image-id divergence, size divergence for the same tag, or
pin divergence across hosts) -- else 0.
"""
import argparse, datetime, html, json, os, re, sys

SKILL_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TMPL = os.path.join(SKILL_DIR, "assets", "report.html.tmpl")

# Provenance pin labels (§4 of the design doc). Shared between the
# provenance table (side-by-side per host) and fleet-drift's pin-divergence
# check, so the two features can never disagree about which labels exist.
PIN_LABELS = [
    ("org.quasar.pins.gwd", "gwd"),
    ("org.quasar.pins.gst", "gst"),
    ("org.quasar.pins.plugins-rs", "plugins-rs"),
    ("org.quasar.pins.interpipe", "interpipe"),
    ("org.quasar.pins.base", "base"),
]


def esc(x):
    return html.escape(str(x)) if x is not None else ""


def _ceiling(role):
    """Ceiling comes from validate-image.sh's size_max_mb field (Task 2 Step
    6, roles[].contract.size_max_mb) -- never parsed out of a human-readable
    assertion string. Missing/None on documents predating that field, or
    under --fast (contract is None): both render as "no ceiling recorded"
    rather than crashing or dividing by zero."""
    return (role.get("contract") or {}).get("size_max_mb") or 0


def size_bar(size_mb, ceiling):
    if size_mb is None:
        return "<span class='muted'>size unknown</span>"
    if not ceiling:
        return "%d MB <span class='muted'>(no ceiling recorded)</span>" % size_mb
    pct = max(4, min(100, int(100 * size_mb / ceiling)))
    over = " over" if size_mb > ceiling else ""
    return "<span class='bar%s' style='width:%dpx'></span>%d/%d MB" % (
        over, pct, size_mb, ceiling)


def short_id(image_id):
    """First 12 hex chars of the image id, stripped of a leading 'sha256:'.
    None (not a fabricated string) when the id itself is null/empty, so
    callers can distinguish "no id" from "id present" and render each
    distinguishably -- never silently coerce null into a fake short id."""
    if not image_id:
        return None
    s = image_id[7:] if image_id.startswith("sha256:") else image_id
    return s[:12]


def _parse_ts(s):
    if not isinstance(s, str):
        return None
    for fmt in ("%Y-%m-%dT%H:%M:%SZ", "%Y-%m-%dT%H:%M:%S.%fZ"):
        try:
            return datetime.datetime.strptime(s, fmt)
        except ValueError:
            continue
    return None


def age_text(created, generated_at):
    """Age is (document generated_at - image created), both taken from the
    document itself -- never wall-clock time. That keeps the renderer pure
    and its output fully reproducible from a fixed input document, and it
    degrades to "unknown" rather than crashing on a null/unparseable
    timestamp on either side (degraded-BASE hosts have created=None)."""
    c, g = _parse_ts(created), _parse_ts(generated_at)
    if c is None or g is None:
        return "<span class='muted'>age unknown</span>"
    secs = (g - c).total_seconds()
    if secs < 0:
        return "<span class='muted'>age unknown</span>"
    days = int(secs // 86400)
    if days >= 1:
        return "%dd" % days
    hours = int(secs // 3600)
    if hours >= 1:
        return "%dh" % hours
    return "%dm" % int(secs // 60)


def dirty_badge(value):
    """Tri-state like `badge()`, but polarity is inverted (True == dirty ==
    bad) so it can't reuse `badge()`'s ok/bad mapping directly. A bare False
    (clean) must read as visually distinct from an unresolved None
    (unknown provenance) -- same null-safety rule as the `running` block."""
    if value is True:
        return "<span class='tag-bad'>dirty</span>"
    if value is False:
        return "<span class='tag-ok'>clean</span>"
    return "<span class='tag-unknown'>unknown</span>"


def badge(value, ok_label, bad_label, unknown_label="unknown"):
    """`value` is a tri-state live-check result: True/False/None (unknown).
    A bare False must read as visually distinct from an unresolved None --
    that's the whole point of rendering the `running` block."""
    if value is True:
        return "<span class='tag-ok'>%s</span>" % esc(ok_label)
    if value is False:
        return "<span class='tag-bad'>%s</span>" % esc(bad_label)
    return "<span class='tag-unknown'>%s</span>" % esc(unknown_label)


def running_is_bad(running):
    """True only on a real, confirmed mismatch (some field is literally
    False) -- never on unknown (None), which is a normal, expected outcome
    (no agent container running, or --fast was used)."""
    if not running:
        return False
    return any(v is False for v in running.values())


def render_running(running):
    if not running or all(v is None for v in (running or {}).values()):
        return ("<p class='muted'>No running agent container detected "
                "(or checked with --fast).</p>")
    items = [
        "<li>Container: <code>%s</code></li>" % esc(running.get("container")),
        "<li>Running image <code>%s</code> — %s</li>" % (
            esc(running.get("image_id")),
            badge(running.get("matches_latest"),
                  "matches :latest", "DOES NOT MATCH :latest")),
        "<li>Agent binary <code>%s</code> — %s</li>" % (
            esc(running.get("agent_binary")),
            badge(running.get("baked"),
                  "baked into image", "WORKSPACE BUILD (not baked)")),
        "<li>Pulse image <code>%s</code> — %s</li>" % (
            esc(running.get("pulse_image")),
            badge(running.get("pulse_has_daemon"),
                  "has pulseaudio daemon", "NO pulseaudio daemon")),
    ]
    return "<ul class='running'>%s</ul>" % "".join(items)


def render_role_row(r, generated_at):
    c = r.get("contract") or {}
    verdict = c.get("verdict", "not run")
    cls = "fail" if verdict == "FAIL" else ""
    passed, failed = c.get("passed"), c.get("failed")
    if passed is None and failed is None:
        contract_text = esc(verdict)
    else:
        contract_text = "%s — %s passed, %s failed" % (
            esc(verdict),
            esc(passed if passed is not None else "?"),
            esc(failed if failed is not None else "?"))
    fails = "".join(
        "<div class='fail'>%s — %s</div>" % (esc(a.get("id")), esc(a.get("detail")))
        for a in (c.get("assertions") or []) if a.get("status") == "FAIL")

    labels = r.get("labels") or {}
    commit = labels.get("org.quasar.source.commit", "no label")
    commit_cell = "<code>%s</code> %s" % (esc(commit), dirty_badge(r.get("source_dirty")))

    sid = short_id(r.get("image_id"))
    id_cell = "<code>%s</code>" % esc(sid) if sid else "<span class='muted'>unknown</span>"

    row = (
        "<tr><td><code>%s</code></td><td><code>%s</code></td><td>%s</td><td>%s</td>"
        "<td>%s</td><td class='%s'>%s</td><td>%s</td></tr>"
        % (esc(r.get("role")), esc(r.get("tag")), id_cell,
           size_bar(r.get("size_mb"), _ceiling(r)),
           age_text(r.get("created"), generated_at),
           cls, contract_text, commit_cell))
    if fails:
        row += "<tr><td colspan='7'>%s</td></tr>" % fails
    return row


def render_host(h, generated_at):
    if not h.get("reachable"):
        return "<h2>%s</h2><p class='fail'>unreachable: %s</p>" % (
            esc(h.get("name")), esc(h.get("error")))

    disk = h.get("disk") or {}
    disk_text = "free %sGB, images %sGB, reclaimable %sGB" % (
        esc(disk.get("free_gb", "?") if disk.get("free_gb") is not None else "?"),
        esc(disk.get("images_gb", "?") if disk.get("images_gb") is not None else "?"),
        esc(disk.get("reclaimable_gb", "?") if disk.get("reclaimable_gb") is not None else "?"))

    error_note = ("<p class='fail'>degraded: %s</p>" % esc(h["error"])
                  if h.get("error") else "")
    rows = "".join(render_role_row(r, generated_at) for r in h.get("roles", []))
    return (
        "<h2>%s <span class='muted'>(%s, %s)</span></h2>"
        "%s%s"
        "<table><tr><th>role</th><th>tag</th><th>image id</th><th>size</th>"
        "<th>age</th><th>contract</th><th>source commit</th></tr>%s</table>"
        % (esc(h.get("name")), esc(h.get("gpu", "?")), disk_text,
           error_note, render_running(h.get("running")), rows))


def render_provenance(doc):
    """Resolved pins per host, side by side (§8) -- "which gwd commit is
    each host running" answerable at a glance without hunting through each
    host's own card. Sourced entirely from image labels already present in
    the document; unreachable hosts contribute no rows (they carry no
    label facts) and a missing/empty label renders as an explicit
    "unknown" cell rather than a blank one."""
    rows = []
    for h in doc["hosts"]:
        if not h.get("reachable"):
            continue
        for r in h.get("roles", []):
            labels = r.get("labels") or {}
            pin_cells = "".join(
                "<td>%s</td>" % (esc(labels[key]) if labels.get(key)
                                  else "<span class='muted'>unknown</span>")
                for key, _ in PIN_LABELS)
            commit = labels.get("org.quasar.source.commit")
            built_at = labels.get("org.quasar.built.at")
            rows.append(
                "<tr><td>%s</td><td><code>%s</code></td>%s"
                "<td>%s</td><td>%s</td></tr>" % (
                    esc(h.get("name")), esc(r.get("tag")), pin_cells,
                    ("<code>%s</code>" % esc(commit)) if commit
                    else "<span class='muted'>unknown</span>",
                    esc(built_at) if built_at
                    else "<span class='muted'>unknown</span>"))
    if not rows:
        return "<h2>Provenance</h2><p class='muted'>No reachable hosts.</p>"
    header = ("<tr><th>host</th><th>tag</th>%s"
               "<th>source commit</th><th>built at</th></tr>"
               % "".join("<th>%s</th>" % esc(label) for _, label in PIN_LABELS))
    return "<h2>Provenance</h2><table>%s%s</table>" % (header, "".join(rows))


def render_drift(doc):
    """Fleet drift: same tag resolving to different image ids across hosts
    (the 2026-07-26 incident), size divergence for the same tag, and pin
    divergence across hosts (§8). Returns (html, found) -- `found` is the
    single boolean the caller folds into the exit-code decision, so drift
    can never again silently render red while the process exits 0."""
    by_tag = {}
    for h in doc["hosts"]:
        if not h.get("reachable"):
            continue
        for r in h.get("roles", []):
            by_tag.setdefault(r.get("tag"), []).append(
                (h.get("name"), r.get("image_id"), r.get("size_mb"), r.get("labels") or {}))

    lines = []
    found = False

    for tag, entries in sorted(by_tag.items()):
        ids = {e[1] for e in entries if e[1] is not None}
        if len(entries) > 1 and len(ids) > 1:
            found = True
            lines.append(
                "<li><code>%s</code> resolves to %d different images: %s</li>" % (
                    esc(tag), len(ids),
                    esc(", ".join(
                        "%s=%s" % (n, short_id(i) or "unknown")
                        for n, i, _, _ in entries))))

    for tag, entries in sorted(by_tag.items()):
        sizes = {e[2] for e in entries if e[2] is not None}
        if len(entries) > 1 and len(sizes) > 1:
            found = True
            lines.append(
                "<li><code>%s</code> size diverges across hosts: %s</li>" % (
                    esc(tag),
                    esc(", ".join(
                        "%s=%s" % (n, ("%d MB" % sz) if sz is not None else "unknown")
                        for n, _, sz, _ in entries))))

    for key, short_label in PIN_LABELS:
        values = {}
        for tag, entries in by_tag.items():
            for n, _iid, _sz, labels in entries:
                v = labels.get(key)
                if v:
                    values.setdefault(v, set()).add(n)
        if len(values) > 1:
            found = True
            lines.append(
                "<li>pin <code>%s</code> diverges across hosts: %s</li>" % (
                    esc(short_label),
                    esc(", ".join(
                        "%s=%s" % (v, ",".join(sorted(hosts)))
                        for v, hosts in sorted(values.items())))))

    if not lines:
        return "<h2>Fleet drift</h2><p class='muted'>None detected.</p>", False
    return "<h2>Fleet drift</h2><ul>%s</ul>" % "".join(lines), found


_SLOT_RE = re.compile(r"\{\{(\w+)\}\}")


def render_template(tmpl_text, slot):
    """Single-pass substitution keyed off {{name}}. re.sub scans the
    template exactly once, left to right, and never rescans replacement
    text for further matches -- unlike a loop of sequential str.replace()
    calls, where a later replace's search re-scans content an earlier
    replace already inserted. That matters here because host_sections
    (rendered first, in the old code) can carry a remote-supplied string
    (an image label, an assertion detail) that literally contains
    "{{drift_section}}"; with str.replace() the later drift_section
    substitution would corrupt it. Raises if the template references a
    key SLOT doesn't have, so a missing SLOT entry fails loudly instead of
    leaving a literal {{placeholder}} in the output."""
    def repl(m):
        key = m.group(1)
        if key not in slot:
            raise KeyError("unbound template placeholder: {{%s}}" % key)
        return slot[key]
    return _SLOT_RE.sub(repl, tmpl_text)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="src", required=True)
    ap.add_argument("--out", dest="dst", required=True)
    args = ap.parse_args()
    with open(args.src) as f:
        doc = json.load(f)

    failed = any((r.get("contract") or {}).get("verdict") == "FAIL"
                 for h in doc["hosts"] for r in h.get("roles", []))
    unreachable = any(not h.get("reachable") for h in doc["hosts"])
    running_bad = any(running_is_bad(h.get("running"))
                       for h in doc["hosts"] if h.get("reachable"))
    drift_html, drift_found = render_drift(doc)
    bad = failed or unreachable or running_bad or drift_found

    reasons = []
    if failed:
        reasons.append("contract violations")
    if unreachable:
        reasons.append("unreachable hosts")
    if running_bad:
        reasons.append("live agent mismatches")
    if drift_found:
        reasons.append("fleet drift")

    generated_at = doc.get("generated_at", "")
    SLOT = {
        "verdict_class": "bad" if bad else "ok",
        "verdict_text": ("Attention needed: %s — see below" % ", ".join(reasons)
                         if bad else "All audited images satisfy their contract"),
        "generated_at": esc(generated_at),
        "host_sections": "".join(render_host(h, generated_at) for h in doc["hosts"]),
        "provenance_section": render_provenance(doc),
        "drift_section": drift_html,
    }
    with open(TMPL) as f:
        tmpl_text = f.read()
    out = render_template(tmpl_text, SLOT)
    with open(args.dst, "w") as f:
        f.write(out)
    print("report → %s" % args.dst, file=sys.stderr)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
