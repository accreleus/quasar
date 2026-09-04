# PROTOTYPE — updater sidecar (#113). Throwaway. Never lands on `develop`.

Answers the apply-side questions in #113 before amendment 2 and the updater ticket are
written. The verdicts live as a comment on #104; this directory is the primary source.

What is here:

- `updater.py` — the sidecar. HTTP over a unix socket (`/run/quasar-updater/updater.sock`)
  in a volume shared with the agent and the control plane. `plan()` is the pure decision
  (namespace allowlist, digest form, known services, the two env-line rewrite, the two
  compose commands); the executor forks detached and writes `results/<id>.json`.
  It discovers its own compose project (name, config files, working dir) from its own
  container labels, so it needs no per-overlay configuration.
- `Dockerfile` — `docker:29-cli` (docker CLI + compose plugin) + python3 + curl.
- `docker-compose.updater-prototype.yml` — overlay adding the sidecar to the REAL stack.
- `mini-stack/` — isolated stand-in stack with the real service names, no host ports,
  for running the exercises beside a live `deploy` project. Stand-ins are the real
  control-plane image with a shell loop that prints updater results at boot.
- `ask.sh` — curl one-liners for talking to the sidecar from inside a container.

Run (on a host with docker + compose, in `mini-stack/`, after copying `env.example` to
`.env` and fixing `QUASAR_STACK_DIR`):

    docker compose -p updproto up -d --wait
    docker compose -p updproto exec quasar-control-plane sh /run/../ask.sh whoami   # or copy ask.sh in
