#!/usr/bin/env bash
source scripts/verify/common.sh
bash scripts/verify/quick.sh
bash scripts/verify/db.sh
bash scripts/verify/agent.sh
bash scripts/verify/security.sh
