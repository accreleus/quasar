import copy
import json
import unittest
from discord_releases import Announcer, CHANNEL, SERVER, DeliveryError, eligible, message

CUTOFF = '2026-10-11T00:00:00Z'
WEBHOOK = 'https://discord.com/api/webhooks/123/test_token'
RELEASE = {'id': 9001, 'draft': False, 'published_at': '2026-10-12T01:00:00Z',
           'tag_name': 'v0.5.0', 'prerelease': False,
           'html_url': 'https://github.com/accreleus/quasar/releases/tag/v0.5.0'}


class FakeAPI:
    def __init__(self):
        self.calls, self.records = [], []
        self.hook = {'channel_id': CHANNEL, 'guild_id': SERVER}
        self.fail_send = False
        self.fail_status = False
        self.fail_claim = False

    def __call__(self, url, method='GET', data=None, token=None):
        self.calls.append((url, method, data, token))
        if '/releases/tags/' in url:
            return dict(RELEASE)
        if '/deployments?' in url:
            return self.records
        if url == WEBHOOK:
            return self.hook
        if url.endswith('/deployments') and method == 'POST':
            if self.fail_claim:
                raise DeliveryError('claim failed')
            self.records.append({'payload': data['payload']})
            return {'id': 51}
        if url == WEBHOOK + '?wait=true':
            if self.fail_send:
                raise DeliveryError('unknown delivery')
            return {'id': '123456', 'channel_id': CHANNEL}
        if url.endswith('/statuses'):
            if self.fail_status:
                raise DeliveryError('unknown status')
            return {'id': 1}
        raise AssertionError('Unexpected API call')


class Tests(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI()
        self.sender = Announcer('fake-github-token', WEBHOOK, CUTOFF, self.api)

    def test_configuration_check_read_only(self):
        self.sender.check_destination()
        self.assertEqual(len(self.api.calls), 1)
        self.assertEqual(self.api.calls[0][0], WEBHOOK)
        self.assertEqual(self.api.calls[0][1], 'GET')
        self.assertIsNone(self.api.calls[0][3])

    def test_configuration_wrong_destination(self):
        self.api.hook['channel_id'] = 'wrong'
        with self.assertRaises(DeliveryError):
            self.sender.check_destination()
        self.assertFalse(any(call[1] == 'POST' for call in self.api.calls))

    def test_future_release(self):
        self.assertTrue(eligible(RELEASE, CUTOFF))

    def test_historical_and_draft_never_touch_api(self):
        for fields in [{'draft': True}, {'published_at': None},
                       {'published_at': CUTOFF}, {'published_at': '2026-10-10T00:00:00Z'},
                       {'tag_name': 'v0.4.0'}]:
            release = dict(RELEASE, **fields)
            self.assertEqual(self.sender.deliver(release), 'historical-or-draft')
        self.assertEqual(self.api.calls, [])

    def test_prerelease_labeled(self):
        self.assertIn('prerelease', message(dict(RELEASE, prerelease=True))['content'])

    def test_mentions_disabled(self):
        self.assertEqual(message(RELEASE)['allowed_mentions'], {'parse': []})

    def test_untrusted_tag_rejected(self):
        with self.assertRaises(DeliveryError):
            message(dict(RELEASE, tag_name='@everyone'))

    def test_untrusted_url_rejected(self):
        with self.assertRaises(DeliveryError):
            message(dict(RELEASE, html_url='https://example.com'))

    def test_invalid_configuration(self):
        for hook in ['http://discord.com/api/webhooks/123/token', WEBHOOK + '?x=1',
                     'https://discord.com.evil/api/webhooks/123/token']:
            with self.assertRaises(DeliveryError):
                Announcer('fake', hook, CUTOFF)
        with self.assertRaises(ValueError):
            Announcer('fake', WEBHOOK, '2026-10-11')

    def test_success_claims_before_sending(self):
        self.assertEqual(self.sender.deliver(copy.deepcopy(RELEASE)), 'sent')
        posts = [call for call in self.api.calls if call[1] == 'POST']
        self.assertTrue(posts[0][0].endswith('/deployments'))
        self.assertEqual(posts[1][0], WEBHOOK + '?wait=true')
        self.assertFalse(posts[0][2]['auto_merge'])
        self.assertIsNone(posts[1][3])  # GitHub token never sent to Discord.
        self.assertEqual(self.sender.deliver(RELEASE), 'claimed')
        self.assertEqual(len([c for c in self.api.calls if c[0] == WEBHOOK + '?wait=true']), 1)

    def test_wrong_destination_no_claim_or_post(self):
        for field in ['channel_id', 'guild_id']:
            self.api.hook[field] = 'wrong'
            with self.assertRaises(DeliveryError):
                self.sender.deliver(RELEASE)
            self.assertFalse(any(call[1] == 'POST' for call in self.api.calls))

    def test_claim_failure_no_discord_post(self):
        self.api.fail_claim = True
        with self.assertRaises(DeliveryError):
            self.sender.deliver(RELEASE)
        self.assertFalse(any(c[0] == WEBHOOK + '?wait=true' for c in self.api.calls))

    def test_ambiguous_send_never_replayed(self):
        self.api.fail_send = True
        with self.assertRaises(DeliveryError):
            self.sender.deliver(RELEASE)
        self.assertEqual(self.sender.deliver(RELEASE), 'claimed')
        self.assertEqual(len([c for c in self.api.calls if c[0] == WEBHOOK + '?wait=true']), 1)

    def test_status_failure_never_replayed(self):
        self.api.fail_status = True
        with self.assertRaises(DeliveryError):
            self.sender.deliver(RELEASE)
        self.assertEqual(self.sender.deliver(RELEASE), 'claimed')

    def test_forged_claims_cannot_suppress_delivery(self):
        signed = self.sender.signed_claim(RELEASE['id'])
        self.api.records = [
            {'payload': {'release_id': RELEASE['id'], 'channel_id': CHANNEL}},
            {'payload': dict(signed, signature='0' * 64)},
            {'payload': 'not json'}, {'payload': []},
            {'payload': dict(signed, signature='\u2603')},
            {'payload': dict(signed, signature='\ud800')},
        ]
        self.assertEqual(self.sender.deliver(RELEASE), 'sent')

    def test_signed_claim_bound_to_all_fields(self):
        signed = self.sender.signed_claim(RELEASE['id'])
        for key in ['version', 'repository', 'task', 'environment',
                    'release_id', 'channel_id', 'cutoff']:
            with self.subTest(field=key):
                self.assertFalse(self.sender.authentic_claim(
                    {'payload': dict(signed, **{key: 'tampered'})}, RELEASE['id']))
        self.assertFalse(self.sender.authentic_claim(
            {'payload': dict(signed, extra='injected')}, RELEASE['id']))

    def test_other_webhook_cannot_forge_claim(self):
        attacker = Announcer('fake', WEBHOOK + '_different', CUTOFF, self.api)
        self.api.records = [{'payload': attacker.signed_claim(RELEASE['id'])}]
        self.assertEqual(self.sender.deliver(RELEASE), 'sent')

    def test_authenticated_json_claim_blocks_replay(self):
        self.api.records = [{'payload': json.dumps(self.sender.signed_claim(RELEASE['id']))}]
        self.assertEqual(self.sender.deliver(RELEASE), 'claimed')
        self.assertFalse(any(call[1] == 'POST' for call in self.api.calls))

    def test_receipt_contains_no_credentials(self):
        self.sender.deliver(RELEASE)
        receipt = json.dumps(self.api.records)
        self.assertNotIn(WEBHOOK, receipt)
        self.assertNotIn('test_token', receipt)
        self.assertNotIn('fake-github-token', receipt)

    def test_manual_invocation_fetches_only_requested_release(self):
        self.sender.run(RELEASE['tag_name'])
        gets = [c[0] for c in self.api.calls if '/releases' in c[0]]
        self.assertEqual(gets, ['https://api.github.com/repos/accreleus/quasar/releases/tags/v0.5.0'])

    def test_manual_invocation_rejects_unsafe_tag(self):
        with self.assertRaises(DeliveryError):
            self.sender.run('v0.5.0/../../other')
        self.assertEqual(self.api.calls, [])

    def test_manual_invocation_rejects_wrong_release_response(self):
        with self.assertRaises(DeliveryError):
            self.sender.run('v0.6.0')
        self.assertFalse(any(c[1] == 'POST' for c in self.api.calls))


if __name__ == '__main__':
    unittest.main()
