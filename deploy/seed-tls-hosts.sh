#!/usr/bin/env bash
# seed-tls-hosts.sh — put this host's LAN address into QUASAR_TLS_HOSTS.
#
# WHY THIS EXISTS
# The default deploy is batteries-included TLS (QUASAR_TLS=auto): the
# control-plane generates a self-signed cert on first boot and persists it under
# QUASAR_TLS_DIR. It bakes in localhost, the loopback IPs, QUASAR_PUBLIC_HOST,
# anything in QUASAR_TLS_HOSTS, and its own non-loopback interface addresses.
#
# That last set sounds like it covers LAN access. It does not: the control-plane
# runs in a container, so the only address it can enumerate is its own docker
# bridge address (172.x.y.z). The HOST's LAN IP, the one an operator actually
# types into a browser, is in a namespace it cannot see. The generated cert
# therefore has no SAN matching https://<host-lan-ip>:8443, and the browser
# fails hostname validation (ERR_CERT_COMMON_NAME_INVALID) forever: trusting the
# cert does not help, because the failure is name mismatch, not trust.
#
# So the default deploy needs QUASAR_TLS_HOSTS set to be usable over the LAN.
# This script sets it from the host's primary LAN address so the operator does
# not have to know any of the above.
#
# Usage (on the deployment host, from the repo root):
#   deploy/seed-tls-hosts.sh [env-file]      # default env-file: deploy/.env
# deploy/redeploy.sh calls this automatically. Run it by hand before the first
# `docker compose up -d` if you bring the stack up that way.
#
# IDEMPOTENT BY DESIGN: an existing uncommented QUASAR_TLS_HOSTS is never
# rewritten (the operator's value wins, and a rewrite could silently drop a name
# they added). It only ever appends when there is no value at all.
#
# DNS-NAME HINT: an operator with a DNS name (no-proxy deploy) can export
# QUASAR_TLS_HOSTS in the shell environment before running this script (or
# before deploy/redeploy.sh, which calls it automatically) and that value is
# seeded verbatim instead of the detected LAN IP/hostname, e.g.:
#   QUASAR_TLS_HOSTS=play.example.com deploy/seed-tls-hosts.sh
# This is only consulted when deploy/.env has no QUASAR_TLS_HOSTS of its own —
# same append-only-when-empty rule as everything else here.
set -euo pipefail

ENV_FILE="${1:-deploy/.env}"
ENV_HINT="${QUASAR_TLS_HOSTS:-}"

# An uncommented assignment with a non-empty value. The commented-out example
# line shipped in .env.example is deliberately NOT a value.
tls_hosts_value() {
  [ -f "$ENV_FILE" ] || return 0
  sed -nE 's/^[[:space:]]*QUASAR_TLS_HOSTS[[:space:]]*=[[:space:]]*([^[:space:]#]+).*/\1/p' \
    "$ENV_FILE" | tail -1
}

# Primary LAN address = the source address the kernel would use for off-link
# traffic. `ip route get` asks the routing table directly, so it picks the right
# interface on a multi-homed host instead of guessing from `ip addr` order.
# 1.1.1.1 is only a routing lookup target; nothing is sent to it.
detect_lan_ip() {
  local ip=""
  if command -v ip >/dev/null 2>&1; then
    ip="$(ip -4 route get 1.1.1.1 2>/dev/null |
      sed -nE 's/.*[[:space:]]src[[:space:]]+([0-9.]+).*/\1/p' | head -1)"
  fi
  # Fallback for hosts without iproute2 (or no default route to look up).
  if [ -z "$ip" ] && command -v hostname >/dev/null 2>&1; then
    ip="$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9.]+$' | head -1 || true)"
  fi
  case "$ip" in
    "" | 127.* | 169.254.*) return 0 ;; # loopback / link-local are useless as a LAN SAN
  esac
  printf '%s' "$ip"
}

existing="$(tls_hosts_value)"
LAN_IP="$(detect_lan_ip)"

if [ -n "$existing" ]; then
  # Do not touch it — but do say so if the value looks like it will not cover
  # this host, since that is the exact silent failure this script exists for.
  if [ -n "$LAN_IP" ] && ! printf '%s' "$existing" | tr ',' '\n' | grep -qx "$LAN_IP"; then
    echo "note: QUASAR_TLS_HOSTS is already set to '$existing' in $ENV_FILE and is left as-is."
    echo "note: it does not list this host's LAN IP ($LAN_IP); https://$LAN_IP:<tls-port> will"
    echo "note: fail hostname validation unless one of the listed names resolves for clients."
  else
    echo "QUASAR_TLS_HOSTS already set in $ENV_FILE — left untouched"
  fi
  exit 0
fi

if [ -n "$ENV_HINT" ]; then
  # Operator told us explicitly (e.g. a DNS name) — trust it verbatim, no LAN
  # detection needed.
  HOSTS="$ENV_HINT"
elif [ -z "$LAN_IP" ]; then
  echo "!! Could not determine this host's primary LAN address, so QUASAR_TLS_HOSTS" >&2
  echo "!! was NOT seeded. Set it by hand in $ENV_FILE to the IP/hostname you will" >&2
  echo "!! use in the browser, e.g. QUASAR_TLS_HOSTS=192.0.2.10,play.lan — otherwise" >&2
  echo "!! the self-signed cert has no SAN for it and the browser rejects the name." >&2
  exit 0 # advisory, not fatal: the stack still works over https://localhost
else
  # The short hostname is a cheap extra SAN: harmless if it does not resolve for
  # clients, and it makes https://<hostname>:<port> work where it does (mDNS,
  # local DNS, hosts file).
  HOSTS="$LAN_IP"
  SHORT_HOST="$(hostname -s 2>/dev/null || true)"
  case "$SHORT_HOST" in
    "" | localhost) ;;
    *) HOSTS="$HOSTS,$SHORT_HOST" ;;
  esac
fi

umask 077
touch "$ENV_FILE"
{
  echo ""
  echo "# QUASAR_TLS_HOSTS — SAN hostnames/IPs for the self-signed HTTPS cert (#376)."
  echo "# Seeded by deploy/seed-tls-hosts.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ) because none was set."
  echo "# The control-plane runs in a container, so it can only enumerate its own"
  echo "# docker bridge address, never this host's LAN IP. Without this line the"
  echo "# generated cert has no SAN for https://$LAN_IP:<tls-port> and the browser"
  echo "# rejects the NAME (ERR_CERT_COMMON_NAME_INVALID), which trusting the cert"
  echo "# does not fix. Add any other name clients use (VPN IP, DNS name) here."
  echo "#"
  echo "# CHANGING THIS DOES NOT RE-ISSUE AN EXISTING CERT: the pair is generated"
  echo "# once and reused for ~10 years so an accepted browser exception survives"
  echo "# restarts. To pick up new names, delete cert.pem + key.pem from the TLS"
  echo "# volume and recreate the control-plane container. That mints a NEW cert"
  echo "# with a new fingerprint, so every client that trusted the old one must"
  echo "# trust the new one again."
  echo "QUASAR_TLS_HOSTS=$HOSTS"
} >>"$ENV_FILE"
chmod 600 "$ENV_FILE"
echo "seeded QUASAR_TLS_HOSTS=$HOSTS into $ENV_FILE"
