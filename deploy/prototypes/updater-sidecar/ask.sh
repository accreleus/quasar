#!/bin/sh
# PROTOTYPE (#113) — talk to the updater from inside a container that shares its volume.
#   ask.sh whoami | results | result <id> | apply <id> <control-ref> <agent-ref> <svc,...> [dry]
S=${UPDATER_SOCK:-/run/quasar-updater/updater.sock}
case "$1" in
  whoami)  curl -s --unix-socket "$S" http://u/whoami ;;
  results) curl -s --unix-socket "$S" http://u/results ;;
  result)  curl -s --unix-socket "$S" "http://u/result/$2" ;;
  apply)   id=$2; c=$3; a=$4; svcs=$(echo "$5" | sed 's/,/","/g'); dry=${6:+, "dry_run": true}
           curl -s --unix-socket "$S" -X POST http://u/apply -d "{\"id\":\"$id\",\"images\":{\"control\":\"$c\",\"agent\":\"$a\"},\"services\":[\"$svcs\"]$dry}" ;;
  *) echo "usage: $0 whoami|results|result <id>|apply <id> <control-ref> <agent-ref> <svc,..> [dry]" >&2; exit 2 ;;
esac
echo
