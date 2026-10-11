# Discord release announcements

`Discord releases` is invoked manually by an operator or release agent to post a concise
version and release-notes link for one explicitly selected published
Quasar releases, including labeled prereleases. It never mentions users or roles and
never cross-publishes to servers following the announcement channel.

## Configuration

- Create a channel-scoped Discord incoming webhook for the intended announcements channel.
- Store its private URL directly in the GitHub repository Actions secret
  `DISCORD_RELEASE_WEBHOOK_URL`. Never put the URL in source, issue text, or logs.
- Set repository Actions variable `DISCORD_RELEASES_ENABLED_AFTER` to the UTC activation
  time (`YYYY-MM-DDTHH:MM:SSZ`). Without this variable, the posting job is disabled.
- The script verifies the webhook's server and channel against its explicit destination
  constants. A mismatch stops delivery.

There are no scheduled checks or automatic published-release triggers. Releases published at or before the
activation timestamp, drafts, and the previously announced v0.4.0 are excluded.

A manual workflow run first performs `--check-config`: a read-only webhook GET that verifies
credentials and destination without sending a message or creating a receipt. The default
`verify_only=true` stops there. Explicit posting fetches only the requested `release_tag`;
it does not scan or post other releases.

The release agent should invoke the workflow on `main`, keeping the webhook URL in GitHub:

```sh
# Read-only verification; no message is sent.
gh workflow run discord-releases.yml --repo accreleus/quasar --ref main \
  -f release_tag=vX.Y.Z -f verify_only=true

# Post exactly the selected published release when instructed to announce it.
gh workflow run discord-releases.yml --repo accreleus/quasar --ref main \
  -f release_tag=vX.Y.Z -f verify_only=false
```

Replace `vX.Y.Z` with the published release's actual tag. No separate announcement-text
template/input is implemented yet; the current post remains the version and release-notes link.

## Delivery receipts and failure recovery

The workflow uses `contents: read` and `deployments: write`. It creates delivery receipts
with task `discord-release-announcement` in environment `discord-announcements`, containing
the release/channel IDs, repository, activation cutoff, task/environment, and an HMAC-SHA256
signature authenticated by the existing private webhook credential. The credential itself
is never stored in receipts. Forged, unsigned, altered, and malformed claims are ignored;
deployment-write access alone cannot generate a valid signature. These records do not
deploy Quasar, merge refs, or modify
release notes. They must be retained to preserve duplicate-delivery protection.

A single concurrency group serializes runs. A receipt is claimed before the Discord POST;
an authenticated existing claim blocks automatic replay, even if a run failed or was interrupted.
Successful acknowledgements add a status with the Discord message link. No POST is retried
automatically after an uncertain result. This favors avoiding duplicates over automatic
recovery: a crash after the claim but before sending can leave an unsent announcement.

For an interrupted or failed run, inspect the matching receipt and Discord channel before
reconciliation. Never blindly delete receipts or repeat a POST. Do not run parallel script
copies outside the workflow's concurrency group. Disable delivery by removing the activation
variable or disabling the workflow; revoke the Discord webhook if its URL is compromised.

Webhook rotation changes the receipt authentication key. Before replacing the webhook secret,
disable the workflow and reconcile any pending announcements. Reset the activation cutoff to
the current UTC time before re-enabling it, so older signed receipts cannot cause replay.
Do not rotate the credential while delivery is running. Repository actors who can change
workflow code/access its secrets or delete genuine receipts remain trusted; signatures protect
against forged claim insertion, not those broader privileges.

## Tests

Run `python3 -m unittest discover -s scripts/release -p test_discord_releases.py -v` and
`actionlint .github/workflows/discord-releases.yml .github/workflows/ci.yml`.
The unit tests mock all external APIs; they send no Discord messages and create no receipts.
