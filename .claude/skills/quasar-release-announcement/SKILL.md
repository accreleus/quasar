---
name: quasar-release-announcement
description: Draft, preview, and manually post a Quasar release announcement to the Accreleus Discord announcements channel after the GitHub release is published.
---

Prepare an engaging announcement from the actual published release notes. Describe user benefits, not a commit list or unsupported performance claims. Aim for 100–150 words; small releases can be shorter. Use the current repository's `scripts/release/discord_releases.py` and read its [operator reference](../../../scripts/release/discord-releases.md) for setup and recovery.

Create a temporary JSON file outside the repository with these inputs:
- `version`: exact published tag, e.g. `v0.5.0`.
- `headline`: short plain-text headline, up to 100 characters.
- `summary`: one or two sentences explaining the main benefit, up to 500 characters.
- `highlights`: one to three plain-text benefit-focused bullets, up to 200 characters each.
- `release_notes_url`: exact `https://github.com/accreleus/quasar/releases/tag/{version}`.
- `prerelease`: boolean, matching GitHub; default false. Preview releases are labeled.
- `upgrade_note`: optional important upgrade/compatibility instruction, up to 300 characters.

Preview from the repository root:

```sh
python3 scripts/release/discord_releases.py --announcement /tmp/quasar-announcement.json
```

Always show the complete rendered preview in the conversation and ask the user to approve posting that exact announcement to the Accreleus Discord announcements channel. General approval to create/publish a release or use this skill does not cover posting. Wait for explicit approval of the displayed text; silence is not approval. Once that exact preview is approved, send it without asking a second time. If the version, destination, or wording changes after approval, show the revised complete preview and obtain fresh approval before posting.

```sh
python3 scripts/release/discord_releases.py --announcement /tmp/quasar-announcement.json --send
```

The script reads the private webhook locally, verifies the published release and Discord destination, and disables all pings. Never read/print the webhook file, paste credentials into chat, include them in command arguments, or commit announcement drafts. Use the designated posting machine, operator account, and persistent receipt directory; other machines/accounts do not share duplicate protection.

Report the returned Discord message link after success. An existing claim, timeout, error, or uncertain acknowledgement is a stopping point: inspect Discord and the local receipt before reconciliation. Never switch state directories, delete claims, or retry to work around a failure. The prior v0.4.0 announcement and releases published before activation are excluded. The script does not cross-publish to follower servers.
