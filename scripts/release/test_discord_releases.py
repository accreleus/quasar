import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from discord_releases import CHANNEL, SERVER, DeliveryError, message, private_file, send

WEBHOOK = 'https://discord.com/api/webhooks/123/fake_token'
DATA = {'version': 'v0.5.0', 'headline': 'Smoother sessions',
        'summary': 'A verified benefit from the release notes.',
        'highlights': ['A meaningful improvement.', 'An important fix.'],
        'release_notes_url': 'https://github.com/accreleus/quasar/releases/tag/v0.5.0'}
RELEASE = {'id': 9001, 'tag_name': 'v0.5.0', 'draft': False, 'prerelease': False,
           'published_at': '2026-10-12T01:00:00Z', 'html_url': DATA['release_notes_url']}


class FakeAPI:
    def __init__(self):
        self.calls = []
        self.release = dict(RELEASE)
        self.hook = {'channel_id': CHANNEL, 'guild_id': SERVER}
        self.fail_send = False
        self.ack = {'id': '123456', 'channel_id': CHANNEL}
        self.state = None

    def __call__(self, url, method='GET', data=None):
        self.calls.append((url, method, data))
        if '/releases/tags/' in url:
            return self.release
        if url == WEBHOOK:
            return self.hook
        if url == WEBHOOK + '?wait=true':
            receipts = list(self.state.glob('*.json'))
            assert len(receipts) == 1
            assert json.loads(receipts[0].read_text())['state'] == 'claimed'
            if self.fail_send:
                raise DeliveryError('uncertain')
            return self.ack
        raise AssertionError('Unexpected API call')


class Tests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.state = Path(self.tmp.name) / 'receipts'
        self.api = FakeAPI()
        self.api.state = self.state

    def post(self, data=DATA):
        return send(data, WEBHOOK, self.state, self.api)

    def test_preview_format_mentions_and_final_link(self):
        payload = message(DATA)
        self.assertIn('Smoother sessions', payload['content'])
        self.assertTrue(payload['content'].endswith(DATA['release_notes_url']))
        self.assertEqual(payload['allowed_mentions'], {'parse': []})
        self.assertFalse(self.state.exists())

    def test_prerelease_and_upgrade_note(self):
        payload = message(dict(DATA, prerelease=True, upgrade_note='Update before connecting.'))
        self.assertIn('Preview release', payload['content'])
        self.assertIn('**Upgrading:** Update before connecting.', payload['content'])

    def test_invalid_input_rejected(self):
        for fields in [{'version': '@everyone'}, {'release_notes_url': 'https://evil.example'},
                       {'highlights': []}, {'highlights': ['x'] * 4},
                       {'summary': 'x' * 501}, {'summary': '\ud800'},
                       {'headline': 'line\nbreak'}, {'prerelease': 'false'},
                       {'extra': 'unexpected'}]:
            with self.subTest(fields=fields), self.assertRaises(DeliveryError):
                message(dict(DATA, **fields))

    def test_missing_fields_rejected(self):
        with self.assertRaises(DeliveryError):
            message({'version': 'v0.5.0'})

    def test_send_exact_release_claim_before_post_and_no_github_writes(self):
        self.assertTrue(self.post().endswith('/123456'))
        self.assertEqual(len([c for c in self.api.calls if c[1] == 'POST']), 1)
        self.assertTrue(self.api.calls[0][0].endswith('/releases/tags/v0.5.0'))
        receipt = next(self.state.glob('*.json'))
        self.assertEqual(receipt.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.state.stat().st_mode & 0o777, 0o700)
        self.assertEqual(json.loads(receipt.read_text())['state'], 'sent')
        self.assertNotIn('fake_token', receipt.read_text())

    def test_duplicate_never_reposted(self):
        self.post()
        with self.assertRaises(DeliveryError):
            self.post()
        self.assertEqual(len([c for c in self.api.calls if c[1] == 'POST']), 1)

    def test_uncertain_delivery_never_replayed(self):
        self.api.fail_send = True
        with self.assertRaises(DeliveryError):
            self.post()
        self.api.fail_send = False
        with self.assertRaises(DeliveryError):
            self.post()
        self.assertEqual(len([c for c in self.api.calls if c[1] == 'POST']), 1)

    def test_wrong_acknowledgement_keeps_claim(self):
        self.api.ack['channel_id'] = 'wrong'
        with self.assertRaises(DeliveryError):
            self.post()
        self.assertTrue(any(self.state.glob('*.json')))

    def test_wrong_destination_never_claims_or_posts(self):
        self.api.hook['guild_id'] = 'wrong'
        with self.assertRaises(DeliveryError):
            self.post()
        self.assertFalse(self.state.exists())

    def test_unpublished_historical_mismatched_release_rejected(self):
        for fields in [{'draft': True}, {'published_at': None},
                       {'published_at': '2026-10-04T00:00:00Z'}, {'prerelease': True},
                       {'tag_name': 'v0.6.0'}, {'id': '../path'}, {'html_url': 'https://evil.example'}]:
            self.api.release = dict(RELEASE, **fields)
            with self.subTest(fields=fields), self.assertRaises(DeliveryError):
                self.post()
        self.assertFalse(self.state.exists())
        self.assertFalse(any(c[1] == 'POST' for c in self.api.calls))

    def test_persistence_failure_prevents_post(self):
        with patch('discord_releases.os.fsync', side_effect=OSError('disk failure')):
            with self.assertRaises(OSError):
                self.post()
        self.assertFalse(any(c[1] == 'POST' for c in self.api.calls))

    def test_insecure_or_symlinked_state_rejected(self):
        self.state.mkdir(mode=0o755)
        with self.assertRaises(DeliveryError):
            self.post()
        self.state.rmdir()
        self.state.symlink_to(self.tmp.name)
        with self.assertRaises(DeliveryError):
            self.post()

    def test_private_credential_read_without_exposure(self):
        path = Path(self.tmp.name) / 'webhook'
        path.write_text(WEBHOOK)
        path.chmod(0o600)
        self.assertEqual(private_file(path), WEBHOOK)
        path.chmod(0o644)
        with self.assertRaises(DeliveryError):
            private_file(path)
        link = Path(self.tmp.name) / 'link'
        link.symlink_to(path)
        with self.assertRaises(OSError):
            private_file(link)

    def test_invalid_webhook_file_rejected(self):
        path = Path(self.tmp.name) / 'webhook'
        for value in ['http://discord.com/api/webhooks/123/token', WEBHOOK + '?x=1',
                      'https://discord.com.evil/api/webhooks/123/token']:
            path.write_text(value)
            path.chmod(0o600)
            with self.assertRaises(DeliveryError):
                private_file(path)


if __name__ == '__main__':
    unittest.main()
