#!/usr/bin/env bash
# Offline contract test for check-release-compatibility.sh (the ADR 0008
# release-time check). Fixtures only: scripts/release/fixtures/compatibility/.
#
# The fixtures: candidate 0.5.0 (floor 0.4.0) whose actor renders control-plane
# 1..1, node-agent 1..2, recovery-actor 1..1; known releases 0.4.0 and 0.4.1
# (format 2) and 0.3.0 (format 1, ignored). The previous release's actor renders
# recovery-actor 1..1 unless a case names another previous-actor windows file.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
check="$repo/scripts/release/check-release-compatibility.sh"
fx="$repo/scripts/release/fixtures/compatibility"
ns="ghcr.io/accreleus/quasar"

work="$(mktemp -d /tmp/quasar-release-compatibility-test.XXXXXX)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/none"

fail() { echo "release compatibility test: $*" >&2; exit 1; }

run() { # run <manifest> <known dir> <recipes> <windows> [previous windows]
  "$check" --manifest "$fx/$1" --known "$2" --recipes "$fx/$3" --actor-windows "$fx/$4" \
    --previous-actor-windows "$fx/${5:-previous-actor-windows.json}"
}

pass() { # pass <expected fragment> <run args...>
  local expected=$1 output
  shift
  output=$(run "$@" 2>&1) || fail "unexpected refusal ($*): $output"
  [[ "$output" == *"$expected"* ]] || fail "missing '$expected': $output"
}

refuse() { # refuse <expected fragment> <run args...>
  local expected=$1 output
  shift
  if output=$(run "$@" 2>&1); then
    fail "unexpected PASS ($*): $output"
  fi
  [[ "$output" == *"$expected"* ]] || fail "missing refusal '$expected': $output"
}

test -x "$check" || fail "check-release-compatibility.sh is not executable"
"$check" --help >/dev/null || fail "--help must exit 0"

# The first format-2 release: nothing known, only (a) applies.
pass "PASS 0.5.0: 0 known format-2 release(s), previous none" \
  candidate.json "$work/none" recipes.json actor-windows.json

# Known releases; the format-1 one is ignored.
out=$(run candidate.json "$fx/known" recipes.json actor-windows.json 2>&1) \
  || fail "compatible release refused: $out"
[[ "$out" == *"ignoring v0.3.0.json (not format 2)"* ]] || fail "format-1 manifest not ignored: $out"
[[ "$out" == *"PASS 0.5.0: 2 known format-2 release(s), previous 0.4.1"* ]] || fail "$out"
pass "previous none" candidate.json "$fx/known-format1-only" recipes.json actor-windows.json

# (a) the candidate actor cannot render its own release's node agent.
refuse "(a) 0.5.0 node-agent image $ns/quasar-node-agent@sha256:bbbb" \
  candidate.json "$fx/known" recipes-candidate-agent-3.json actor-windows.json
refuse "revision 3 is outside the candidate recovery actor's node-agent window 1..2" \
  candidate.json "$work/none" recipes-candidate-agent-3.json actor-windows.json
refuse "(a) 0.5.0 control-plane image $ns/quasar-control-plane@sha256:aaaa" \
  candidate.json "$work/none" recipes.json actor-windows-no-control.json
refuse "the candidate recovery actor carries no control-plane recipe" \
  candidate.json "$work/none" recipes.json actor-windows-no-control.json

# (b) the floor covers 0.4.0, whose agent needs revision 1; the actor renders 2..2.
refuse "(b) the node-agent floor 0.4.0 covers release 0.4.0, whose node-agent image $ns/quasar-node-agent@sha256:2222" \
  candidate.json "$fx/known" recipes.json actor-windows-agent-2-only.json
out=$(run candidate.json "$fx/known" recipes.json actor-windows-agent-2-only.json 2>&1 || true)
[[ "$out" != *"release 0.4.1"* ]] || fail "0.4.1's agent (revision 2) is inside 2..2: $out"
# The same window passes when no known release is in reach of the floor.
pass "PASS" candidate.json "$work/none" recipes.json actor-windows-agent-2-only.json

# (b) the previous release's control plane could not be restored.
refuse "(b) restoring the previous release 0.4.1's control plane $ns/quasar-control-plane@sha256:4444" \
  candidate.json "$fx/known" recipes-candidate-control-2.json actor-windows-control-2-only.json
pass "PASS" candidate.json "$work/none" recipes-candidate-control-2.json actor-windows-control-2-only.json

# (c) a floor above the previous release strands what that release left current.
refuse "(c) the node-agent floor 0.5.0-0 orders above the previous release 0.4.1" \
  candidate-floor-above-previous.json "$fx/known" recipes.json actor-windows.json
pass "PASS" candidate-floor-above-previous.json "$work/none" recipes.json actor-windows.json

# (d) the previous release's actor cannot render the candidate actor it hands over to.
refuse "(d) 0.5.0 recovery-actor image $ns/quasar-recovery@sha256:cccc" \
  candidate.json "$fx/known" recipes-candidate-actor-2.json actor-windows-actor-2.json
refuse "revision 2 is outside the previous release 0.4.1's recovery actor's recovery-actor window 1..1" \
  candidate.json "$fx/known" recipes-candidate-actor-2.json actor-windows-actor-2.json
pass "PASS" candidate.json "$fx/known" recipes-candidate-actor-2.json actor-windows-actor-2.json \
  actor-windows-actor-2.json
# With no earlier format-2 release there is no hand-over to check.
pass "PASS" candidate.json "$work/none" recipes-candidate-actor-2.json actor-windows-actor-2.json
# A previous release makes its actor's windows a required input.
out=$("$check" --manifest "$fx/candidate.json" --known "$fx/known" --recipes "$fx/recipes.json" \
  --actor-windows "$fx/actor-windows.json" 2>&1) && fail "a missing previous-actor windows file passed: $out"
[[ "$out" == *"(d) --previous-actor-windows is required"* ]] || fail "$out"

# Inputs.
refuse "no recipe revision for $ns/quasar-node-agent@sha256:5555" \
  candidate.json "$fx/known" recipes-missing-known-agent.json actor-windows.json
refuse "has recipe revision '1', not a positive integer" \
  candidate.json "$work/none" recipes-string-revision.json actor-windows.json
refuse "node-agent must be {\"from\": N, \"to\": M} with 1 <= N <= M" \
  candidate.json "$work/none" recipes.json actor-windows-malformed.json
refuse "unknown role 'seed'" candidate.json "$work/none" recipes.json actor-windows-malformed.json
printf 'quasar-recovery 0.5.0\n' > "$work/not-json.json"
out=$("$check" --manifest "$fx/candidate.json" --known "$work/none" --recipes "$fx/recipes.json" \
  --actor-windows "$work/not-json.json" 2>&1) && fail "unreadable actor windows passed: $out"
[[ "$out" == *"--actor-windows $work/not-json.json is unreadable"* ]] || fail "$out"
refuse "has the candidate's own version 0.5.0" \
  candidate.json "$fx/known-same-version" recipes.json actor-windows.json
refuse "is not a valid format-2 manifest" \
  known/v0.3.0.json "$work/none" recipes.json actor-windows.json
mkdir -p "$work/dup"
cp "$fx/known/v0.4.0.json" "$work/dup/a.json"
cp "$fx/known/v0.4.0.json" "$work/dup/b.json"
refuse "repeats version 0.4.0" candidate.json "$work/dup" recipes.json actor-windows.json
refuse "is not a directory" candidate.json "$work/absent" recipes.json actor-windows.json

# Every reason is reported, not just the first.
out=$(run candidate-floor-above-previous.json "$fx/known" recipes-candidate-agent-3.json \
  actor-windows.json 2>&1 || true)
[[ "$out" == *"(a) "* && "$out" == *"(c) "* && "$out" == *"REFUSED (2 reason(s))"* ]] \
  || fail "check stopped at the first reason: $out"

rc=0
"$check" --manifest "$fx/candidate.json" >/dev/null 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "a missing flag must exit 2, got $rc"

echo "Release compatibility check: PASS"
