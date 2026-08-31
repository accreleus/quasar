# Security policy

## Supported versions

Quasar has no tagged releases yet. `main` is the only supported line, and it moves
fast. There is no long-term-support branch. If you are running an older commit,
update to the latest `main` before reporting a problem; it may already be fixed.

## Reporting a vulnerability

Please report security issues privately, not as a public GitHub issue. Use
[GitHub Security Advisories](https://github.com/accreleus/quasar/security/advisories/new)
for this repository to open a draft advisory. It reaches maintainers directly and
keeps the report out of public view until a fix is ready.

Include what you can:

- What you found and why it's a security issue (not just a bug).
- Steps to reproduce, or a proof of concept.
- The affected component (`control-plane`, `node-agent`, `web`, `deploy/`, or the
  `protocol/` contracts).
- Any suggested fix or mitigation, if you have one.

We'll acknowledge reports as promptly as we can and follow up with next steps.
There is no bug-bounty program at this time.

## Scope

This is a self-hosted platform aimed at individual operators, not a managed
multi-tenant service, so the threat model assumes an operator who controls their
own deployment. Issues in that model still matter: authentication/authorization
bypass, privilege escalation between users on a shared instance, remote code
execution, and anything that could compromise a host running the node agent.

Vendored/third-party components (`third_party/`, the pinned `gst-wayland-display`
and `inputtino` forks, see `docs/third-party-pins.md`) should generally be reported
upstream as well, but let us know if a Quasar-specific patch or configuration is
involved.
