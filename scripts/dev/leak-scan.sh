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
  -h | --help)
    sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  *)
    echo "usage: $0 [--staged]" >&2
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

# The schema template's placeholders are RFC 5737 addresses, but keep it named
# so a future placeholder choice cannot trip the guard.
ALLOWLIST=(
  ':(exclude).claude/skills/_shared/hosts.example.json'
)

ALTERNATION="$(
  IFS='|'
  echo "${PATTERNS[*]}"
)"

grep_args=(--line-number --extended-regexp --no-color -I -e "$ALTERNATION")
[ "$MODE" = "staged" ] && grep_args=(--cached "${grep_args[@]}")

set +e
HITS="$(git grep "${grep_args[@]}" -- . "${INTERNAL_BOUND[@]}" "${ALLOWLIST[@]}" "$SELF" 2>/dev/null)"
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
