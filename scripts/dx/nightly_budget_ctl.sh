#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/nightly_budget_ctl.sh — install/run/status for the nightly
# glass-to-glass budget cron (scripts/dx/nightly_budget.sh).
#
#   make nightly-budget-install HOST=devbox   # idempotent: writes the crontab line
#   make nightly-budget-run     HOST=devbox   # trigger one run now, foreground
#   make nightly-budget-status  HOST=devbox   # tail today's log + LAST_REGRESSION
#
# HOST=local runs directly (no ssh); any other HOST must be TYPED explicitly
# for install/run (mutating — writes a crontab line / launches a live bench
# run), matching every other remote-mutating dx verb. `status` is read-only
# and does not require an explicit HOST.
#
# The crontab line always points at the CHECKED-OUT tree's own
# scripts/dx/nightly_budget.sh — installing it does not fetch or redeploy
# anything, and neither does the cron job itself (see nightly_budget.sh's own
# header). Re-running install after a redeploy simply repoints the same line
# at whatever is on disk now (nothing to repoint — the line is a fixed path).
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CMD="${1:-}"
case "$CMD" in
  install|run|status) shift ;;
  *) printf 'usage: nightly_budget_ctl.sh {install|run|status}\n' >&2; exit 2 ;;
esac

TARGET="nightly-budget-$CMD"
dx_require_host_scope "$TARGET"

CRON_LINE_MARK="scripts/dx/nightly_budget.sh"
CRON_SCHEDULE="30 3 * * *"

remote_dir_or_local() {
  if [ "$DX_HOST" = local ]; then printf '%s\n' "$DX_ROOT"; else printf '%s\n' "$DX_REMOTE_DIR"; fi
}

run_here_or_remote() { # run_here_or_remote <shell-snippet>
  if [ "$DX_HOST" = local ]; then bash -c "$1"; else dx_ssh_remote "$1"; fi
}

case "$CMD" in
  install)
    DIR="$(remote_dir_or_local)"
    dx_info "installing crontab line on $DX_HOST: $CRON_SCHEDULE cd $DIR && $CRON_LINE_MARK"
    if run_here_or_remote "cd '$DIR' && \
        ( crontab -l 2>/dev/null | grep -vF '$CRON_LINE_MARK'; \
          echo '$CRON_SCHEDULE cd $DIR && bash $CRON_LINE_MARK >/dev/null 2>&1' \
        ) | crontab -"; then
      dx_pass install "crontab line written (idempotent — reinstalling replaces, never duplicates)"
    else
      dx_fail install "crontab write failed on $DX_HOST"
    fi
    run_here_or_remote "crontab -l 2>/dev/null | grep -F '$CRON_LINE_MARK' || true"
    dx_result "$TARGET"
    ;;
  run)
    DIR="$(remote_dir_or_local)"
    dx_info "triggering one run on $DX_HOST (foreground — a real cell takes several minutes)"
    run_here_or_remote "cd '$DIR' && bash $CRON_LINE_MARK"
    dx_result "$TARGET"
    ;;
  status)
    DIR="$(remote_dir_or_local)"
    run_here_or_remote "LOGDIR=\"\${NIGHTLY_LOG_DIR:-\$HOME/quasar-nightly}\"; \
      TODAY=\"\$LOGDIR/\$(date -u +%F).log\"; \
      if [ -f \"\$TODAY\" ]; then echo \"== \$TODAY (last 40 lines) ==\"; tail -n 40 \"\$TODAY\"; \
      else echo \"no log for today yet: \$TODAY\"; fi; \
      echo; \
      if [ -f \"\$LOGDIR/LAST_REGRESSION\" ]; then echo '== LAST_REGRESSION =='; cat \"\$LOGDIR/LAST_REGRESSION\"; \
      else echo '(no LAST_REGRESSION on file)'; fi; \
      echo; crontab -l 2>/dev/null | grep -F '$CRON_LINE_MARK' || echo '(no crontab line installed)'"
    dx_result "$TARGET"
    ;;
esac
