# Discord release announcements

`Discord releases` posts a concise version and release-notes link for future published
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

Published-release events trigger prompt checks. An hourly scheduled check catches releases
created by the Images workflow's `GITHUB_TOKEN`, which do not start separate release-event
workflows. Scheduled runs can be delayed by GitHub. Releases published at or before the
activation timestamp, drafts, and the previously announced v0.4.0 are excluded.

A manual workflow run first performs `--check-config`: a read-only webhook GET that verifies
credentials and destination without sending a message or creating a receipt. The subsequent
scan sends only eligible future releases. Check the activation timestamp and published
releases before using a manual run as a no-post verification.

## Delivery receipts and failure recovery

The workflow uses `contents: read` and `deployments: write`. It creates delivery receipts
with task `discord-release-announcement` in environment `discord-announcements`, containing
only the release ID and channel ID. These records do not deploy Quasar, merge refs, or modify
release notes. They must be retained to preserve duplicate-delivery protection.

A single concurrency group serializes runs. A receipt is claimed before the Discord POST;
any existing claim blocks automatic replay, even if a run failed or was interrupted.
Successful acknowledgements add a status with the Discord message link. No POST is retried
automatically after an uncertain result. This favors avoiding duplicates over automatic
recovery: a crash after the claim but before sending can leave an unsent announcement.

For an interrupted or failed run, inspect the matching receipt and Discord channel before
reconciliation. Never blindly delete receipts or repeat a POST. Do not run parallel script
copies outside the workflow's concurrency group. Disable delivery by removing the activation
variable or disabling the workflow; revoke the Discord webhook if its URL is compromised.

## Tests

Run `python3 -m unittest discover -s scripts/release -p test_discord_releases.py -v` and
`actionlint .github/workflows/discord-releases.yml .github/workflows/ci.yml`.
The unit tests mock all external APIs; they send no Discord messages and create no receipts.
