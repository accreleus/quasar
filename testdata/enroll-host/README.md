# Served enrollment script pins (#359)

The control plane serves `deploy/enroll-host.sh` at `/enroll-host.sh` with two lines filled
in from its configuration (`QUASAR_ENROLL_SEED_IMAGE`, `QUASAR_ENROLL_AGENT_IMAGE`,
`docs/configuration.md`): the seed image the one-line command starts and the node-agent image
it installs. The console's Add host dialog reads the same two lines back from the served
script to write its Dockge / Arcane stack, so both paths start the same seed.

`pins.json` is the pair and the exact lines a render must produce. Both sides test against it:

- `control-plane/internal/enrollscript` renders the real `deploy/enroll-host.sh` with these
  images and asserts the rendered script carries exactly `lines`;
- `web/src/lib/addHost.test.ts` puts `lines` into the real script and asserts the console
  reads these images back and writes them into the stack.
