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

# --- the generic patterns ----------------------------------------------------
#
# Shapes that identify ANYONE's machine, with no operator-specific value in
# them. A blanket "any RFC1918 address" rule would fire on the coturn deny-range
# examples in deploy/README.md, which are correct and must stay literal, so an
# operator's own address range belongs in the operator patterns loaded below.
PATTERNS=(
  # Absolute home paths from any developer's machine (macOS and Linux shapes).
  '/Users/[A-Za-z0-9._-]+/'
  '/home/[A-Za-z0-9._-]+/(code|src|dev|projects)/'
)

# --- the operator's own patterns ----------------------------------------------
#
# The literal values that identify ONE operator's network — an address range, a
# domain, key and host names — are deliberately NOT in this file. A public
# repository that lists them has published the very inventory it is guarding.
# They are loaded at run time; only the generic shapes above live here.
#
#   LEAK_SCAN_OPERATOR_PATTERNS  newline-separated patterns. CI sets it from the
#                                repository secret of the same name.
#   LEAK_SCAN_PATTERNS_FILE      a file in the same format. Defaults to
#                                .claude/skills/_shared/leak-patterns.local in the
#                                MAIN checkout (untracked, beside hosts.json), so
#                                a worktree finds the same file.
#
# Format: one extended regex per line. `tree:` (or no prefix) applies to every
# mode; `issues:` applies to the issue tracker only — bare host names, which
# tracked prose may still carry. Blank lines and `#` comments are ignored.
#
# With neither source the scan runs the generic shapes only and says so on
# stderr. LEAK_SCAN_REQUIRE_OPERATOR_PATTERNS=1 turns that into exit 2; CI sets it
# wherever the secret is available, so a missing or broken secret can never read
# as a clean run. Pattern TEXT is never printed: in CI it is a secret.
OPERATOR_TREE_PATTERNS=()
OPERATOR_ISSUE_PATTERNS=()
operator_raw=""
operator_source=""
if [ -n "${LEAK_SCAN_OPERATOR_PATTERNS:-}" ]; then
  operator_raw="$LEAK_SCAN_OPERATOR_PATTERNS"
  operator_source="LEAK_SCAN_OPERATOR_PATTERNS"
else
  if [ -z "${LEAK_SCAN_PATTERNS_FILE:-}" ]; then
    common="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)"
    if [ -n "$common" ]; then
      LEAK_SCAN_PATTERNS_FILE="$(dirname "$common")/.claude/skills/_shared/leak-patterns.local"
    else
      LEAK_SCAN_PATTERNS_FILE="$(cd "$(dirname "$0")/../.." && pwd)/.claude/skills/_shared/leak-patterns.local"
    fi
  fi
  if [ -r "$LEAK_SCAN_PATTERNS_FILE" ]; then
    operator_raw="$(cat "$LEAK_SCAN_PATTERNS_FILE")"
    operator_source="$LEAK_SCAN_PATTERNS_FILE"
  fi
fi

line_no=0
while IFS= read -r line || [ -n "$line" ]; do
  line_no=$((line_no + 1))
  line="${line%$'\r'}"
  case "$line" in '' | '#'*) continue ;; esac
  case "$line" in
    issues:*) pat="${line#issues:}" kind=issues ;;
    tree:*) pat="${line#tree:}" kind=tree ;;
    *) pat="$line" kind=tree ;;
  esac
  [ -n "$pat" ] || continue
  # Validate before use, and report only the line number: the text may be secret.
  set +e
  printf '' | grep -E -e "$pat" >/dev/null 2>&1
  prc=$?
  set -e
  if [ "$prc" -gt 1 ]; then
    echo "leak-scan: operator pattern on line $line_no of $operator_source is not a valid extended regex." >&2
    exit 2
  fi
  if [ "$kind" = issues ]; then
    OPERATOR_ISSUE_PATTERNS+=("$pat")
  else
    OPERATOR_TREE_PATTERNS+=("$pat")
  fi
done <<<"$operator_raw"

# --- the bench server, from qbench's own config --------------------------------
#
# The quasar-bench server's address is operator-local too: qbench keeps it in
# ${XDG_CONFIG_HOME:-~/.config}/qbench/url, outside every repository, and the
# repo's scripts only ever read BENCH_URL or that file. When the file exists at
# scan time, its host — and, for a dotted name of three or more labels, the
# parent domain — join the operator patterns, so a commit or an issue that pastes
# the real bench address fails. Loopback is skipped (test fixtures use it). The
# value is never printed. LEAK_SCAN_BENCH_URL_FILE points elsewhere (tests).
bench_url_file="${LEAK_SCAN_BENCH_URL_FILE:-${XDG_CONFIG_HOME:-$HOME/.config}/qbench/url}"
if [ -r "$bench_url_file" ]; then
  bench_host="$(sed -n '1{s#^[A-Za-z][A-Za-z0-9+.-]*://##;s#^[^@/]*@##;s#[:/?].*$##;p;}' "$bench_url_file" | tr -d '[:space:]' | tr 'A-Z' 'a-z')"
  case "$bench_host" in
    '' | localhost | 127.* | '[::1]') ;;
    *)
      bench_names=("$bench_host")
      if ! [[ "$bench_host" =~ ^[0-9.]+$ ]]; then
        IFS=. read -r -a bench_labels <<<"$bench_host"
        if [ "${#bench_labels[@]}" -ge 3 ]; then
          bench_names+=("${bench_host#*.}")
        fi
      fi
      for bench_name in "${bench_names[@]}"; do
        OPERATOR_TREE_PATTERNS+=("$(printf '%s' "$bench_name" | sed 's/[][\.*^$()+?{}|]/\\&/g')")
      done
      ;;
  esac
fi

if [ $((${#OPERATOR_TREE_PATTERNS[@]} + ${#OPERATOR_ISSUE_PATTERNS[@]})) -eq 0 ]; then
  if [ "${LEAK_SCAN_REQUIRE_OPERATOR_PATTERNS:-0}" = 1 ]; then
    echo "leak-scan: operator patterns are required (LEAK_SCAN_REQUIRE_OPERATOR_PATTERNS=1) but none were loaded — refusing to report a generic-only scan as clean." >&2
    exit 2
  fi
  echo "leak-scan: note — no operator patterns loaded; running the generic checks only." >&2
fi

# --- exclusions --------------------------------------------------------------
#
# This script describes the shapes it hunts for, so it can match itself.
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

ALL_TREE_PATTERNS=("${PATTERNS[@]}" "${OPERATOR_TREE_PATTERNS[@]}")
ALL_ISSUE_PATTERNS=("${ALL_TREE_PATTERNS[@]}" "${OPERATOR_ISSUE_PATTERNS[@]}")
ALTERNATION="$(
  IFS='|'
  echo "${ALL_TREE_PATTERNS[*]}"
)"
ISSUE_ALTERNATION="$(
  IFS='|'
  echo "${ALL_ISSUE_PATTERNS[*]}"
)"

# --- issue-tracker mode -------------------------------------------------------
#
# Same patterns, other public surface — PLUS the operator's issue-only patterns
# (bare host names), because an issue body has no code-comment excuse for one.
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
  HITS="$(printf '%s' "$FLAT" | grep --extended-regexp --color=never -e "$ISSUE_ALTERNATION")"
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
HITS="$(git grep "${grep_args[@]}" -- . "${ALLOWLIST[@]}" "$SELF" \
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
  personal domain -> an env var with no default (the bench server: BENCH_URL,
                     else qbench's own ~/.config/qbench/url)
  ssh key / alias -> a lookup in .claude/skills/_shared/hosts.json (untracked)

Real addresses and keys belong in .claude/skills/_shared/hosts.json, which is
gitignored. hosts.example.json documents its schema.
EOF
exit 1
