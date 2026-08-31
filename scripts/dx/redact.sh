#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/redact.sh — deterministic stream filter that masks secret VALUES.
#
#   some-command | scripts/dx/redact.sh > safe.txt
#   scripts/dx/redact.sh < input.txt
#
# Everything a diagnostic bundle captures goes through this. It is a pure
# stdin→stdout filter with no side effects, so scripts/dx/tests/run.sh can hold
# it to a golden fixture.
#
# Pattern classes handled (see tests/fixtures/redact-*.txt):
#   1. key=value / key: value where the KEY matches (case-insensitively)
#      password | passwd | secret | token | key | bearer | authorization
#   2. URL credentials — scheme://user:PASS@host  (postgres://, https://, …)
#   3. PEM blocks — the body between BEGIN/END is replaced wholesale
#   4. JWT-looking strings — eyJ<b64>.<b64>.<b64>
#   5. `Bearer <token>` / `Authorization: Bearer <token>` headers
#
# Deliberately conservative in one direction only: it may over-mask (a key
# named "monkey" contains "key"). It must never under-mask.

set -euo pipefail

exec awk '
function is_sensitive(k,   i, n, words) {
  n = split("password passwd secret token key bearer authorization credential", words, " ")
  for (i = 1; i <= n; i++) if (index(k, words[i]) > 0) return 1
  return 0
}

# scheme://user:PASS@  ->  scheme://user:***REDACTED***@
function mask_url_creds(line,   out, rest, m, p) {
  out = ""; rest = line
  while (match(rest, /:\/\/[^:@ \t\/"]+:[^@ \t\/"]+@/)) {
    m = substr(rest, RSTART, RLENGTH)
    p = index(substr(m, 4), ":")            # the ":" separating user from pass
    out = out substr(rest, 1, RSTART - 1) substr(m, 1, 3 + p) "***REDACTED***@"
    rest = substr(rest, RSTART + RLENGTH)
  }
  return out rest
}

# KEY=value and KEY: value, whitespace after the separator preserved.
function mask_kv(line,   out, rest, seg, pre, i, c, sep, key, ws, val, ate_bearer) {
  out = ""; rest = line
  # The unquoted-value class excludes a double quote on purpose: without that,
  # `-H "authorization: X"` would swallow the CLOSING quote and the filter would
  # not be idempotent (re-redacting redacted output would keep eating quotes).
  while (match(rest, /[A-Za-z_][A-Za-z0-9_.-]*[ \t]*[=:][ \t]*("[^"]*"|'"'"'[^'"'"']*'"'"'|[^ \t,;"]+)/)) {
    seg = substr(rest, RSTART, RLENGTH)
    pre = substr(rest, 1, RSTART - 1)
    sep = 0
    for (i = 1; i <= length(seg); i++) {
      c = substr(seg, i, 1)
      if (c == "=" || c == ":") { sep = i; break }
    }
    if (sep > 0) {
      key = substr(seg, 1, sep - 1)
      sub(/[ \t]+$/, "", key)
      ws = ""
      i = sep + 1
      while (i <= length(seg) && (substr(seg, i, 1) == " " || substr(seg, i, 1) == "\t")) {
        ws = ws substr(seg, i, 1); i++
      }
      val = substr(seg, i)
      if (is_sensitive(tolower(key)) && val != "") {
        seg = substr(seg, 1, sep) ws "***REDACTED***"
        # "Authorization: Bearer <token>" — the value token is only the scheme
        # word, so swallow the token that follows it too.
        if (tolower(val) == "bearer") ate_bearer = 1
      }
    }
    out = out pre seg
    rest = substr(rest, RSTART + RLENGTH)
    if (ate_bearer) {
      if (match(rest, /^[ \t]+[A-Za-z0-9._~+\/=-]+/)) rest = substr(rest, RLENGTH + 1)
      ate_bearer = 0
    }
  }
  return out rest
}

BEGIN { in_pem = 0 }

{
  line = $0

  # --- 3. PEM blocks -------------------------------------------------------
  if (in_pem) {
    if (line ~ /-----END [A-Z0-9 ]+-----/) { in_pem = 0; print line }
    next
  }
  if (line ~ /-----BEGIN [A-Z0-9 ]+-----/) {
    print line
    print "***REDACTED***"
    in_pem = 1
    next
  }

  # --- 4. JWTs -------------------------------------------------------------
  gsub(/eyJ[A-Za-z0-9_=-]+\.[A-Za-z0-9_=-]+\.[A-Za-z0-9_=.-]+/, "***REDACTED***", line)

  # --- 1. key=value / key: value -------------------------------------------
  # Runs before the Bearer rule so "Authorization: Bearer x" is masked once,
  # by the key, rather than twice.
  line = mask_kv(line)

  # --- 2. URL credentials --------------------------------------------------
  line = mask_url_creds(line)

  # --- 5. bare Bearer tokens (no key= in front of them) --------------------
  gsub(/[Bb][Ee][Aa][Rr][Ee][Rr][ \t]+[A-Za-z0-9._~+\/=-]+/, "Bearer ***REDACTED***", line)

  print line
}
'
