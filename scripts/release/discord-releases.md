# Manual Discord release announcements

Release agents invoke `quasar-release-announcement` to draft a structured announcement,
preview it locally, then call `scripts/release/discord_releases.py` directly when sending
is authorized. There is no posting workflow, schedule, release trigger or GitHub secret
handoff at runtime. CI runs only mocked script tests.

## Inputs and preview

Write a temporary JSON file outside the checkout with `version`, `headline`, `summary`,
`highlights` (one to three), and `release_notes_url`. Optional inputs are `prerelease`
(boolean) and `upgrade_note`. The skill describes field limits and writing guidance.

```sh
python3 scripts/release/discord_releases.py --announcement /tmp/quasar-announcement.json
```

The default is an offline preview: no secret read, network call or receipt. No announcement
text is committed or added to the GitHub release body. The output includes a headline,
summary, highlights, optional upgrade note, and final full-release-notes link.

## Private setup and posting

On the designated posting machine, the operator securely places the existing Discord
webhook URL in `~/.config/quasar/discord-webhook-url`, owned by the operator with mode 0600.
Do not place it in this repository, a skill file, a command argument, shell history, chat
or logs. The GitHub Actions secret cannot be read back; transfer the existing URL directly
from Discord using a private local editor or secret-management interface. Never have an
agent print or inspect its value. No new webhook needs to be created.

```sh
python3 scripts/release/discord_releases.py --announcement /tmp/quasar-announcement.json --send
```

Optional `--webhook-file` selects an owner-owned 0600 regular file; symlinks are refused.
The script reads the public release by exact tag, verifies publication/URL/prerelease,
and checks the webhook guild/channel before posting. Historical releases before the
original activation time (2026-10-11T04:03:05Z), drafts and v0.4.0 are refused. Pings are
always disabled. It does not cross-publish to servers following the announcement channel.

## Receipts and recovery

Use one posting machine/operator account and persistent receipt directory. The default is
`~/.local/state/quasar-discord-announcements`, mode 0700. `--state-dir` is available for a
single centrally chosen location, not as a retry bypass. Different machines/accounts do
not share duplicate protection. Preserve and securely migrate this directory when changing
the posting machine. Local processes share atomic exclusive claim creation.

Before any Discord POST, the script writes and fsyncs a mode-0600 receipt keyed by release
ID and channel. A claim remains even after a crash/timeout, preventing automatic replay.
A confirmed response adds the message ID and returns its Discord link. It never trusts
GitHub deployment records, creates deployments or needs GitHub write access, eliminating
the forged-deployment-claim issue. Webhook rotation does not invalidate local receipts.

If sending or acknowledgement fails, inspect Discord and the local receipt before manual
reconciliation. Do not blindly retry, delete claims, change accounts/directories or use a
second machine. A crash after claiming but before posting can leave an unsent announcement;
this favors avoiding duplicates over automatic recovery. No live test message is authorized
by setup or preview alone. Revoking the webhook stops all direct posting.

## Tests

`python3 -m unittest discover -s scripts/release -p test_discord_releases.py -v`

All external calls are mocked; tests send no Discord messages. Test receipts are temporary.
