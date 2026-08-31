#!/usr/bin/env bash
source scripts/verify/common.sh
go version
node --version
npm --version
rustc --version
cargo --version
rustfmt --version
cargo clippy --version
gst-launch-1.0 --version | head -n 2
psql --version
printf 'build date: %s\n' "$(cat /etc/quasar-devtools-build-date 2>/dev/null || echo label-only)"
