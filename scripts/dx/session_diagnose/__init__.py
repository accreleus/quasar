"""session_diagnose — the Quasar session-diagnosis analysis code.

It lives in the DX layer (scripts/dx/), not in a skill, because the DX layer is
universal and the skill is a wrapper. Nothing in here mints credentials, resolves
hosts, or reads deploy/.env: `scripts/dx/session.sh` fetches the bundle (auth via
scripts/dx/admin_token.sh) and hands this package a FILE.

Nothing in here owns the classifier's verdict vocabulary either. The control
plane owns it; a verdict string this code has never seen is DATA to pass through,
never a validation failure. A stale four-string copy of that enum here is what
turned a healthy `nominal` session into exit 2 on 2026-08-22.
"""
