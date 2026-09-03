#!/usr/bin/env bash
# leak-scan.sh — refuse to publish operator infrastructure fingerprints.
#
# WHY THIS EXISTS. This repository is mirrored to a public repo. Anything that
# stays public must not carry the operator's home-lab fingerprints: LAN
# addresses paired with host roles, ssh key names and paths, absolute
# /Users/<someone> paths, or a personal DNS name. Each of those is a small,
# durable, permanently-archived hint about a private network. A scrub is a
# one-time act; this script is what stops the next commit re-adding one.
#
# WHAT IT SCANS. Git-tracked content only, never the working tree, so
# deliberately-untracked operator config (.claude/skills/_shared/hosts.json,
# .mcp.json) is invisible to it by construction rather than by allowlist.
#
#   scripts/dev/leak-scan.sh            # the tracked tree at HEAD's index
#   scripts/dev/leak-scan.sh --staged   # staged content only (pre-push/pre-commit)
#   scripts/dev/leak-scan.sh --issues   # GitHub issue titles/bodies/comments
#
# THE TRACKER IS THE OTHER PUBLIC SURFACE. The repo is not the only thing that
# gets published: an issue body is just as public and just as permanently
# archived, and on 2026-09-03 nine issues were found carrying real hostnames, a
# LAN IP, an absolute /home/<user>/ path and the operator's dev domain — several
# filed by an agent working in a DIFFERENT repo that had no such guard. The tree
# modes cannot see any of that, so `--issues` runs the SAME patterns over the
# tracker. It needs `gh` authenticated; it reads, never writes.
#
# Exit: 0 clean, 1 fingerprints found (each printed as path:line:match), 2 usage.
#
# ADDING A PATTERN is cheap and encouraged. Removing one, or widening an
# exclusion to make a run green, is the failure mode this file guards against:
# fix the file instead. Documentation addresses (RFC 5737 192.0.2.0/24,
# 198.51.100.0/24, 203.0.113.0/24) and example.com/.invalid names are the
# sanctioned stand-ins — they are reserved for exactly this and can never route
# to anything real.

set -euo pipefail

MODE="tree"
case "${1:-}" in
  "") ;;
  --staged) MODE="staged" ;;
  --issues) MODE="issues" ;;
  -h | --help)
    sed -n '2,34p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  *)
    echo "usage: $0 [--staged|--issues]" >&2
    exit 2
    ;;
esac

# --- the fingerprint patterns ------------------------------------------------
#
# Deliberately specific. A blanket "any RFC1918 address" rule would fire on the
# coturn deny-range examples in deploy/README.md, which are correct and must
# stay literal; the operator's own /24 is what must never appear.
PATTERNS=(
  # The operator LAN. Any host in it, with or without a port.
  '10\.1\.1\.[0-9]{1,3}'
  # Absolute home paths from any developer's machine (macOS and Linux shapes).
  '/Users/[A-Za-z0-9._-]+/'
  '/home/[A-Za-z0-9._-]+/(code|src|dev|projects)/'
  # The operator's personal domain, in any subdomain.
  '[A-Za-z0-9.-]*techanvil\.net'
  # ssh private-key names that identify a specific box or account.
  'unraid_root'
  'id_ed25519_loopback'
  # A key path pinned to a named developer rather than resolved from config.
  # [~] not ~ — a literal tilde in a regex, never a shell home expansion.
  '[~]/\.ssh/[A-Za-z0-9._-]*(unraid|tower|hermes|devbox)'
)

# --- exclusions --------------------------------------------------------------
#
# PHASE 1 ONLY. These directories are bound for the PRIVATE internal repo and
# never reach the public mirror, so their historical run logs, evidence bundles
# and kickoff prompts are not scrubbed. When those trees leave this repo in
# Phase 2, DELETE this list rather than letting it rot into a general amnesty.
INTERNAL_BOUND=(
  ':(exclude)docs/completed/**'
  ':(exclude)docs/design/**'
  ':(exclude)docs/reports/**'
  ':(exclude)docs/research/**'
  ':(exclude)docs/superpowers/**'
  ':(exclude)docs/tech-debt/**'
  ':(exclude)docs/phase6/**'
  ':(exclude)docs/phase7/**'
  ':(exclude)docs/phase8/**'
  ':(exclude)docs/phase9/**'
)

# This script names every pattern it hunts for, so it always matches itself.
SELF=':(exclude)scripts/dev/leak-scan.sh'

# Same reason, one level out: the negative-test corpus for --issues has to CONTAIN
# fingerprints or it proves nothing. Named as one exact file, never a directory —
# a wildcard here would silently amnesty every future fixture. Its contents are
# invented (someone/192.0.2-style stand-ins are useless for a detection test); it
# describes no real host.
ISSUES_TEST_CORPUS=':(exclude)scripts/dx/tests/fixtures/leak-issues-dirty.json'

# The schema template's placeholders are RFC 5737 addresses, but keep it named
# so a future placeholder choice cannot trip the guard.
ALLOWLIST=(
  ':(exclude).claude/skills/_shared/hosts.example.json'
)

ALTERNATION="$(
  IFS='|'
  echo "${PATTERNS[*]}"
)"

# --- issue-tracker mode -------------------------------------------------------
#
# Same patterns, other public surface. Hostnames alone are deliberately NOT
# patterns here either (see the note above the list): what leaks is an address,
# a key name, a home path or the personal domain.
if [ "$MODE" = issues ]; then
  if [ -z "${LEAK_SCAN_ISSUES_JSON:-}" ]; then
    command -v gh >/dev/null 2>&1 || {
      echo "leak-scan: --issues needs the gh CLI on PATH." >&2
      exit 2
    }
    gh auth status >/dev/null 2>&1 || {
      echo "leak-scan: --issues needs gh authenticated (gh auth login)." >&2
      exit 2
    }
  fi

  # One API call. Every issue, open and closed, with its comments. A failure to
  # fetch must never read as 'clean' — that is the whole point of the guard.
  #
  # LEAK_SCAN_ISSUES_JSON is the TEST SEAM: a file holding the same payload shape,
  # so the detection itself is verifiable offline instead of only against whatever
  # the live tracker happens to contain today (which, right after a scrub, is
  # exactly the payload that proves nothing).
  if [ -n "${LEAK_SCAN_ISSUES_JSON:-}" ]; then
    ISSUES_JSON="$(cat "$LEAK_SCAN_ISSUES_JSON")" || {
      echo "leak-scan: --issues could not read $LEAK_SCAN_ISSUES_JSON" >&2
      exit 2
    }
  else
    ISSUES_JSON="$(gh issue list --state all --limit "${LEAK_SCAN_ISSUE_LIMIT:-500}" \
      --json number,title,body,comments 2>/dev/null)" || {
      echo "leak-scan: --issues could not read the tracker (gh issue list failed)." >&2
      exit 2
    }
  fi

  # Flatten to one grep-able line per source line, labelled the way the tree
  # modes label a file: <location>:<line>:<text>.
  FLAT="$(printf '%s' "$ISSUES_JSON" | jq -r '
    .[] as $i
    | ( [ {f: "title", t: ($i.title // "")}, {f: "body", t: ($i.body // "")} ]
        + ( ($i.comments // [])
            | to_entries
            | map({f: "comment[\(.key + 1)]", t: (.value.body // "")}) ) )[]
    | . as $part
    | ($part.t | split("\n") | to_entries[])
    | "issue#\($i.number) \($part.f):\(.key + 1):\(.value)"
  ')" || {
    echo "leak-scan: --issues could not parse the tracker payload (is jq installed?)." >&2
    exit 2
  }

  set +e
  HITS="$(printf '%s' "$FLAT" | grep --extended-regexp --color=never -e "$ALTERNATION")"
  rc=$?
  set -e
  [ "$rc" -gt 1 ] && {
    echo "leak-scan: grep failed (rc=$rc) while scanning issues" >&2
    exit 2
  }

  if [ -z "$HITS" ]; then
    echo "leak-scan: clean (issues) — no operator fingerprints in the tracker."
    exit 0
  fi

  echo "leak-scan: FINGERPRINTS FOUND in the issue tracker." >&2
  echo >&2
  echo "$HITS" >&2
  echo >&2
  cat >&2 <<'EOF'
An issue body is as public and as permanently archived as a commit. Edit the
issue or comment; the same stand-ins apply as for the tree:

  LAN address     -> a role name (gpu-test / aux-infra / deploy-only), or an
                     RFC 5737 documentation address (192.0.2.x)
  absolute path   -> a repo-relative path, or /path/to/quasar
  personal domain -> a description ("the reporter's dev origin")

NOTE: GitHub keeps an edit history, so editing reduces but does not erase the
disclosure. Deleting the comment is the only full removal.

Beware `gh api -f body=@file`: -f writes the LITERAL string "@file" and silently
blanks the issue. Use `jq -Rs '{body: .}' < file | gh api <path> -X PATCH --input -`
and read one object back afterwards.
EOF
  exit 1
fi

grep_args=(--line-number --extended-regexp --no-color -I -e "$ALTERNATION")
[ "$MODE" = "staged" ] && grep_args=(--cached "${grep_args[@]}")

set +e
HITS="$(git grep "${grep_args[@]}" -- . "${INTERNAL_BOUND[@]}" "${ALLOWLIST[@]}" "$SELF" \
  "$ISSUES_TEST_CORPUS" 2>/dev/null)"
rc=$?
set -e

# git grep: 0 = matched, 1 = no match, >1 = real error.
if [ "$rc" -gt 1 ]; then
  echo "leak-scan: git grep failed (rc=$rc) — is this a git repository?" >&2
  exit 2
fi

if [ -z "$HITS" ]; then
  echo "leak-scan: clean (${MODE}) — no operator fingerprints in tracked content."
  exit 0
fi

echo "leak-scan: FINGERPRINTS FOUND in tracked content (${MODE})." >&2
echo >&2
echo "$HITS" >&2
echo >&2
cat >&2 <<'EOF'
This repository is mirrored publicly. Each hit above leaks a detail of a private
network. Fix the file — do not weaken this script:

  LAN address     -> a role name, or an RFC 5737 documentation address
                     (192.0.2.x / 198.51.100.x / 203.0.113.x), or <your-host-ip>
  absolute path   -> a repo-relative path
  personal domain -> an env var (BENCH_URL / QUASAR_BENCH_URL) with no default
  ssh key / alias -> a lookup in .claude/skills/_shared/hosts.json (untracked)

Real addresses and keys belong in .claude/skills/_shared/hosts.json, which is
gitignored. hosts.example.json documents its schema.
EOF
exit 1
