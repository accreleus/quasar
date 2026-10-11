#!/usr/bin/env python3
"""Preview or manually post one structured announcement; never poll for releases."""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import urllib.error
import urllib.parse
import urllib.request

REPO = 'accreleus/quasar'
CHANNEL = '1545968515406630982'
SERVER = '1545968231179624468'
CUTOFF = '2026-10-11T04:03:05Z'


class DeliveryError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request(url, method='GET', data=None):
    req = urllib.request.Request(url, method=method,
        headers={'User-Agent': 'Quasar-release-announcer', 'Content-Type': 'application/json'},
        data=None if data is None else json.dumps(data).encode())
    try:
        with urllib.request.build_opener(NoRedirect()).open(req, timeout=30) as response:
            return json.load(response)
    except (urllib.error.HTTPError, OSError, ValueError):
        # Exception text, URL and remote body may contain the private webhook token.
        raise DeliveryError('Remote response could not be confirmed; no automatic retry') from None


def text(data, name, maximum):
    value = data.get(name)
    if (not isinstance(value, str) or not value.strip() or len(value) > maximum
            or any(ord(c) < 32 or 0xD800 <= ord(c) <= 0xDFFF for c in value)):
        raise DeliveryError('Invalid announcement field: ' + name)
    return value.strip()


def message(data):
    if not isinstance(data, dict):
        raise DeliveryError('Expected an announcement object')
    required = {'version', 'headline', 'summary', 'highlights', 'release_notes_url'}
    if not required <= data.keys() or data.keys() - required - {'upgrade_note', 'prerelease'}:
        raise DeliveryError('Unexpected or missing announcement fields')
    version = text(data, 'version', 100)
    if not re.fullmatch(r'[A-Za-z0-9._+-]{1,100}', version):
        raise DeliveryError('Invalid release version')
    url = text(data, 'release_notes_url', 250)
    if url != f'https://github.com/{REPO}/releases/tag/{version}':
        raise DeliveryError('Release URL must match the exact repository and version')
    highlights = data['highlights']
    if not isinstance(highlights, list) or not 1 <= len(highlights) <= 3:
        raise DeliveryError('Provide one to three highlights')
    bullets = [text({'highlight': value}, 'highlight', 200) for value in highlights]
    preview = data.get('prerelease', False)
    if not isinstance(preview, bool):
        raise DeliveryError('prerelease must be boolean')
    label = 'Preview release' if preview else 'Release'
    content = (f'**Quasar {version} — {label}: {text(data, "headline", 100)}**\n\n'
               + text(data, 'summary', 500) + '\n\n**What’s new**\n'
               + '\n'.join('- ' + value for value in bullets))
    if 'upgrade_note' in data:
        content += '\n\n**Upgrading:** ' + text(data, 'upgrade_note', 300)
    content += '\n\n**Full release notes:** ' + url
    if len(content) > 2000:
        raise DeliveryError('Announcement exceeds Discord message limit')
    return {'content': content, 'allowed_mentions': {'parse': []}}


def private_file(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor) as stream:
        info = os.fstat(stream.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o600):
            raise DeliveryError('Webhook file must be owner-owned, regular, and mode 0600')
        value = stream.read(8193).strip()
    if not re.fullmatch(r'https://discord\.com/api/webhooks/[0-9]+/[A-Za-z0-9_-]+', value):
        raise DeliveryError('Invalid webhook credential file')
    return value


def state_directory(path):
    path = Path(path)
    path.mkdir(parents=True, mode=0o700, exist_ok=True)
    info = path.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise DeliveryError('Receipt directory must be owner-owned and mode 0700')
    return path


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def send(data, webhook, state, api=request):
    payload = message(data)
    tag = data['version']
    release = api(f'https://api.github.com/repos/{REPO}/releases/tags/'
                  + urllib.parse.quote(tag, safe=''))
    published = release.get('published_at')
    if (release.get('tag_name') != tag or release.get('html_url') != data['release_notes_url']
            or release.get('draft', True) or not published or tag == 'v0.4.0'
            or dt.datetime.fromisoformat(published.replace('Z', '+00:00'))
            <= dt.datetime.fromisoformat(CUTOFF.replace('Z', '+00:00'))):
        raise DeliveryError('Release must match and be published after activation; no historical posts')
    if bool(release.get('prerelease')) != data.get('prerelease', False):
        raise DeliveryError('Announcement prerelease label does not match published release')
    release_id = release.get('id')
    if type(release_id) is not int or release_id <= 0:
        raise DeliveryError('Invalid published release identity')
    hook = api(webhook)
    if str(hook.get('channel_id')) != CHANNEL or str(hook.get('guild_id')) != SERVER:
        raise DeliveryError('Webhook destination does not match verified server/channel')
    directory = state_directory(state)
    receipt = directory / f'{release_id}-{CHANNEL}.json'
    # Exclusive creation is the local inter-process claim. Persist before POST.
    try:
        descriptor = os.open(receipt, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except FileExistsError:
        raise DeliveryError('Release already claimed; inspect Discord and receipt before reconciliation') from None
    record = {'release_id': release_id, 'channel_id': CHANNEL, 'state': 'claimed',
              'content_sha256': hashlib.sha256(payload['content'].encode()).hexdigest()}
    with os.fdopen(descriptor, 'w') as stream:
        json.dump(record, stream)
        stream.flush()
        os.fsync(stream.fileno())
    sync_directory(directory)
    result = api(webhook + '?wait=true', method='POST', data=payload)
    message_id = str(result.get('id', ''))
    if str(result.get('channel_id')) != CHANNEL or not re.fullmatch(r'[0-9]+', message_id):
        raise DeliveryError('Delivery acknowledgement uncertain; inspect Discord before reconciliation')
    # If this status write fails, the durable claim still prevents blind replay.
    record.update(state='sent', message_id=message_id)
    descriptor = os.open(receipt, os.O_WRONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, 'w') as stream:
        stream.seek(0)
        json.dump(record, stream)
        stream.truncate()
        stream.flush()
        os.fsync(stream.fileno())
    return f'https://discord.com/channels/{SERVER}/{CHANNEL}/{message_id}'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--announcement', type=Path, required=True)
    parser.add_argument('--send', action='store_true', help='Explicitly post; default is offline preview')
    parser.add_argument('--webhook-file', type=Path,
                        default=Path.home() / '.config/quasar/discord-webhook-url')
    parser.add_argument('--state-dir', type=Path,
                        default=Path.home() / '.local/state/quasar-discord-announcements')
    args = parser.parse_args()
    try:
        with args.announcement.open() as stream:
            data = json.load(stream)
        if args.send:
            print('Posted: ' + send(data, private_file(args.webhook_file), args.state_dir))
        else:
            print(message(data)['content'])
            print('\nPreview only; no credential read, network request, or receipt created.')
        return 0
    except (DeliveryError, OSError, ValueError, KeyError, TypeError):
        print('Announcement stopped. Check inputs, private configuration, and delivery receipt; '
              'do not blindly retry. No credentials or remote response bodies logged.', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
