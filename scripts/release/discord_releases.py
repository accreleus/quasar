#!/usr/bin/env python3
"""Future-only Discord delivery; deployments persist at-most-once send claims."""
import datetime as dt
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

REPO = 'accreleus/quasar'
CHANNEL = '1545968515406630982'
SERVER = '1545968231179624468'
ENVIRONMENT = 'discord-announcements'
TASK = 'discord-release-announcement'


class DeliveryError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request(url, method='GET', data=None, token=None):
    headers = {'User-Agent': 'Quasar-release-announcer', 'Content-Type': 'application/json'}
    if token:
        headers.update({'Authorization': 'Bearer ' + token,
                        'Accept': 'application/vnd.github+json',
                        'X-GitHub-Api-Version': '2022-11-28'})
    req = urllib.request.Request(url, method=method, headers=headers,
                                 data=None if data is None else json.dumps(data).encode())
    try:
        with urllib.request.build_opener(NoRedirect()).open(req, timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as exc:
        # Never log request URLs, response bodies, or exception text: webhook tokens
        # can occur in all three. No automatic POST retries after ambiguous results.
        raise DeliveryError('Remote API returned HTTP ' + str(exc.code)) from None
    except (OSError, ValueError):
        raise DeliveryError('Remote API response could not be confirmed') from None


def timestamp(value):
    parsed = dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        raise ValueError('Timestamp needs a timezone')
    return parsed


def eligible(release, cutoff):
    return (not release.get('draft', True) and bool(release.get('published_at'))
            and release.get('tag_name') != 'v0.4.0'
            and timestamp(release['published_at']) > timestamp(cutoff))


def message(release):
    url = release['html_url']
    if not url.startswith('https://github.com/' + REPO + '/releases/tag/'):
        raise DeliveryError('Unexpected release URL')
    tag = release['tag_name']
    if not re.fullmatch(r'[A-Za-z0-9._+-]{1,100}', tag):
        raise DeliveryError('Unexpected release tag')
    label = 'prerelease' if release.get('prerelease') else 'release'
    return {'content': f'Quasar {tag} {label} is available.\n\nFull release notes: {url}',
            'allowed_mentions': {'parse': []}}


class Announcer:
    def __init__(self, token, webhook, cutoff, api=request):
        timestamp(cutoff)  # Refuse missing/malformed activation dates.
        if not re.fullmatch(r'https://discord\.com/api/webhooks/[0-9]+/[A-Za-z0-9_-]+', webhook):
            raise DeliveryError('Expected a Discord webhook URL without query parameters')
        self.token, self.webhook, self.cutoff, self.api = token, webhook, cutoff, api

    def github(self, path, method='GET', data=None):
        return self.api('https://api.github.com/repos/' + REPO + path,
                        method=method, data=data, token=self.token)

    def pages(self, path):
        separator = '&' if '?' in path else '?'
        for page in range(1, 101):
            rows = self.github(f'{path}{separator}per_page=100&page={page}')
            yield from rows
            if len(rows) < 100:
                return
        raise DeliveryError('Pagination limit exceeded; refusing an incomplete ledger')

    def check_destination(self):
        hook = self.api(self.webhook)
        if str(hook.get('channel_id')) != CHANNEL or str(hook.get('guild_id')) != SERVER:
            raise DeliveryError('Webhook destination does not match verified channel/server')

    def deliver(self, release):
        if not eligible(release, self.cutoff):
            return 'historical-or-draft'
        payload = message(release)
        query = urllib.parse.urlencode({'environment': ENVIRONMENT, 'task': TASK})
        records = self.pages('/deployments?' + query)
        for record in records:
            data = record.get('payload') or {}
            if isinstance(data, str):
                data = json.loads(data)
            if data.get('release_id') == release['id'] and data.get('channel_id') == CHANNEL:
                # ANY claim blocks automatic replay, including interrupted/failed runs.
                return 'claimed'
        self.check_destination()
        claim = self.github('/deployments', 'POST', {
            'ref': release['tag_name'], 'task': TASK, 'environment': ENVIRONMENT,
            'auto_merge': False, 'required_contexts': [],
            'production_environment': False, 'transient_environment': True,
            'description': 'Discord release delivery claim',
            'payload': {'release_id': release['id'], 'channel_id': CHANNEL}})
        claim_id = claim['id']  # Must be confirmed before any Discord POST.
        result = self.api(self.webhook + '?wait=true', method='POST', data=payload)
        if str(result.get('channel_id')) != CHANNEL or not result.get('id'):
            raise DeliveryError('Discord delivery acknowledgement could not be confirmed')
        self.github(f'/deployments/{claim_id}/statuses', 'POST', {
            'state': 'success', 'auto_inactive': False,
            'description': 'Discord message ' + str(result['id']),
            'environment_url': f'https://discord.com/channels/{SERVER}/{CHANNEL}/{result["id"]}'})
        return 'sent'

    def run(self):
        releases = list(self.pages('/releases'))
        for release in sorted(releases, key=lambda item: item.get('published_at') or ''):
            outcome = self.deliver(release)
            # IDs only; no credentials or uncontrolled release text in logs.
            print('release_id=' + str(release['id']) + ' outcome=' + outcome)


if __name__ == '__main__':
    try:
        announcer = Announcer(os.environ['GITHUB_TOKEN'], os.environ['DISCORD_RELEASE_WEBHOOK_URL'],
                              os.environ['DISCORD_RELEASES_ENABLED_AFTER'])
        if sys.argv[1:] == ['--check-config']:
            announcer.check_destination()
            print('Webhook destination verified; no message sent or receipt created.')
        elif not sys.argv[1:]:
            announcer.run()
        else:
            raise DeliveryError('Unexpected command arguments')
    except (DeliveryError, ValueError, KeyError, TypeError):
        print('Release announcement stopped; inspect delivery claims before retrying. '
              'No credentials or remote response bodies were logged.', file=sys.stderr)
        sys.exit(1)
