#!/usr/bin/env bash
# shellcheck shell=bash
#
# deploy/lib/agent-readiness.sh — what a node-agent's log says about the host.
#
# PURE. Sourced, never run: no docker, no clock, no network, no output but the
# answer. Every function takes text and echoes a word, which is what lets
# scripts/dx/tests/run.sh pin the whole classification without a stack.
#
# It exists because the alternative is a deploy script that reports health it
# never observed. `redeploy.sh` used to set `readiness=ok` the moment the log
# came back non-empty and only then look for a verdict, so a log with no verdict
# in it — or one carrying a verdict the classifier did not know — summarised as
# `result=OK` (#177). The three states are therefore ok, failed, and UNVERIFIED,
# and unverified is the default.
#
# Token contract: node-agent/src/{readiness,agent}.rs.

# readiness_filter_re is the grep -E pattern that finds candidate verdict lines
# in a log. It MUST match every line readiness_cause classifies as something
# other than `unverified`: a verdict the filter drops never reaches the
# classifier, which is exactly how the mid-provision verdict went unnoticed.
# scripts/dx/tests/run.sh pins the two against each other, so they cannot drift
# apart silently again.
readiness_filter_re() {
  printf '%s' 'boot-render-node-missing|boot-render-node-retry-deferred|boot-render-node-retries-spent|boot-render-node-unopenable|boot-dri-modes-stale-cdi|boot-host-render-node-missing|readiness-checks-failed|host readiness: no failures;|host readiness: all checks passed or skipped'
}

# AGENT_START_RE marks one node-agent process start. main.rs logs it once, very
# early, before anything else this file classifies.
#
# It is a HARD STATE BOUNDARY for everything below, because docker keeps a
# container's log across restarts and the agent restarts itself routinely (the
# #98 retry, the driver-volume provision, a config reload). Without it, a verdict
# from a process that is no longer running can be combined with one from the
# process that is — an old healthy probe outvoting the current process's degraded
# plan, or an old all-clear paired with a current codec result to declare both
# verdicts in. Only the LATEST process lifetime counts.
AGENT_START_RE='quasar node-agent .* starting [(]node_name='

# readiness_line <log> — the last candidate verdict of the CURRENT process, or
# empty. Never the first: the #98 boot race exits on purpose and heals on the
# retry, so a healed host still carries the failing line from the boot before.
readiness_line() {
  awk -v re="$(readiness_filter_re)" -v start="$AGENT_START_RE" '
    $0 ~ start { verdict = ""; next }
    $0 ~ re    { verdict = $0 }
    END { if (verdict != "") print verdict }
  ' <<<"$1"
}

# readiness_cause <verdict-line> — one line in, one cause word out. An empty or
# unrecognised line is `unverified`, which is the whole point: absence of
# evidence is not evidence of health.
readiness_cause() {
  case "$1" in
  *'host readiness: all checks passed or skipped'*) echo passed ;;
  *'host readiness: no failures;'*) echo provisioning ;;
  *boot-render-node-missing*) echo render-node-missing ;;
  *boot-render-node-retry-deferred*) echo retry-deferred ;;
  *boot-dri-modes-stale-cdi*) echo stale-cdi ;;
  *boot-render-node-unopenable*) echo render-node-unopenable ;;
  *boot-render-node-retries-spent* | *boot-host-render-node-missing*) echo sanity-failed ;;
  *readiness-checks-failed*) echo checks-failed ;;
  *) echo unverified ;;
  esac
}

# readiness_state <cause> — the value the machine-readable summary reports.
readiness_state() {
  case "$1" in
  passed) echo ok ;;
  provisioning) echo PROVISIONING ;;
  render-node-missing | retry-deferred) echo RETRYING ;;
  stale-cdi | render-node-unopenable | sanity-failed | checks-failed) echo FAILED ;;
  *) echo unverified ;;
  esac
}

# readiness_severity <cause> — what the cause does to the aggregate result:
# `ok` changes nothing, `warn` degrades it, `fail` fails it.
#
# `checks-failed` is warn, not fail, and deliberately: a readiness check that
# fails means "sessions may fail in ways that look unrelated", not "this deploy
# did not happen". The boot-sanity family is fail because every session on the
# host will fail. `unverified` is warn — the deploy may be perfect; nobody knows.
readiness_severity() {
  case "$1" in
  passed) echo ok ;;
  render-node-missing | stale-cdi | render-node-unopenable | sanity-failed) echo fail ;;
  *) echo warn ;;
  esac
}

# codec_cause <log> — what the LATEST codec probe found.
#
# In log order, because docker keeps a container's log across restarts and the
# agent re-probes on every start: a first boot's `pending` must not outlive the
# healthy probe that followed it, and an older healthy probe must not outvote a
# newer degraded one. The agent emits the vulkan codec-plan line (if any) and
# THEN `codec support probed for`, so a plan line is a candidate the probe line
# commits.
#
# `codec support probed for` is the positive signal because it is emitted for
# EVERY encoder. The vulkan codec-plan line is absent by design on a VA or
# openh264 host, so treating its absence as health was the same false claim the
# readiness half made.
codec_cause() {
  awk -v start="$AGENT_START_RE" '
    $0 ~ start { state = ""; candidate = ""; next }
    /vulkan-codec-plan-degraded/              { candidate = "degraded"; next }
    /vulkan-codec-plan-pending-driver-volume/ { candidate = "pending";  next }
    /codec support probed for/                { state = (candidate != "" ? candidate : "ok"); candidate = ""; next }
    END {
      if (state != "") print state
      else if (candidate != "") print candidate
      else print "unverified"
    }
  ' <<<"$1"
}

# agent_verdicts_seen <log> — whether BOTH halves have been observed yet. What
# the poll below waits on: a healthy agent logs its readiness verdict and its
# codec probe at different moments, so a read that lands between them would
# otherwise stop the poll and report a perfectly healthy host as half-unverified.
agent_verdicts_seen() {
  [ -n "$(readiness_line "$1")" ] && [ "$(codec_cause "$1")" != unverified ]
}

# poll_agent_log <reader> <tries> [sleeper] — read the agent's log until both
# verdicts are in, or the tries run out. Echoes the last log read.
#
# The reader and the sleeper are injected so this is testable without a stack
# and without waiting: redeploy.sh passes its docker-compose reader and a real
# `sleep 2`, the suite passes a scripted reader and a no-op. The bound is the
# caller's; nothing here waits forever.
poll_agent_log() {
  local reader="$1" tries="$2" sleeper="${3:-__agent_sleep_2}" log="" i
  for ((i = 0; i < tries; i++)); do
    log="$("$reader")"
    if [ -n "$log" ] && agent_verdicts_seen "$log"; then
      break
    fi
    [ "$i" -lt "$((tries - 1))" ] && "$sleeper"
  done
  printf '%s' "$log"
}

__agent_sleep_2() { sleep 2; }

# codec_severity <codec-cause> — `pending` is the expected first-boot state and
# self-clears on the agent's own restart, so it is a note rather than a warning.
codec_severity() {
  case "$1" in
  ok | pending) echo ok ;;
  *) echo warn ;;
  esac
}

# agent_summary <log> — the whole agent-derived contribution to the deploy
# summary, as `key=value` lines. This is the function the tests assert against,
# so a regression that restores a false OK is caught by behaviour rather than by
# reading the script's source.
#
#   readiness=<ok|RETRYING|PROVISIONING|FAILED|unverified>
#   codecs=<ok|degraded|pending|unverified>
#   severity=<ok|warn|fail>      the worst of the two
agent_summary() {
  local log="$1" line cause rsev csev sev
  line="$(readiness_line "$log")"
  cause="$(readiness_cause "$line")"
  rsev="$(readiness_severity "$cause")"
  csev="$(codec_severity "$(codec_cause "$log")")"
  sev=ok
  case "$rsev:$csev" in
  *fail*) sev=fail ;;
  *warn*) sev=warn ;;
  esac
  printf 'readiness=%s\n' "$(readiness_state "$cause")"
  printf 'codecs=%s\n' "$(codec_cause "$log")"
  printf 'severity=%s\n' "$sev"
}
