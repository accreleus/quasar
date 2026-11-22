#!/usr/bin/env bash
# Send one local user's UDP to <ip>:<port> to the relay instead, with a nat OUTPUT REDIRECT (to 127.0.0.1:<relay-port>).
# Scoped by uid so the relay's own upstream packets (another user) and every other process are untouched; conntrack
# rewrites the replies back, so the client still sees the real address. Needs sudo and a nat table (no netem modules).
#
#   redirect.sh up   <ip> <port> <relay-port> <client-user>
#   redirect.sh down <ip> <port> <relay-port> <client-user>
#   redirect.sh status
set -euo pipefail
cmd=${1:-}
rule() { echo OUTPUT -p udp -d "$1" --dport "$2" -m owner --uid-owner "$(id -u "$4")" -m comment --comment udprelay -j REDIRECT --to-ports "$3"; }
case $cmd in
  up)     # shellcheck disable=SC2046
          sudo -n iptables -t nat -C $(rule "${@:2}") 2>/dev/null || sudo -n iptables -t nat -A $(rule "${@:2}") ;;
  down)   # shellcheck disable=SC2046
          while sudo -n iptables -t nat -C $(rule "${@:2}") 2>/dev/null; do sudo -n iptables -t nat -D $(rule "${@:2}"); done ;;
  status) sudo -n iptables -t nat -S OUTPUT | grep udprelay || echo "no udprelay rules" ;;
  *)      sed -n '2,10p' "$0"; exit 2 ;;
esac
