# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import unittest
import test_scheduler as fixtures
from server.core import Problem


class DeadlineTests(unittest.TestCase):
    setUp = fixtures.SchedulerTests.setUp
    tearDown = fixtures.SchedulerTests.tearDown
    node = fixtures.SchedulerTests.node
    job = fixtures.SchedulerTests.job
    def test_server_deadline_requests_stop_and_holds_reservation(self):
        job = self.job(timeoutSeconds=5)
        claim = self.store.claim('mini')
        self.job()
        self.now += 6
        renewal = self.store.renew('mini', job, claim['lease'])
        self.assertTrue(renewal['cancel'])
        self.assertEqual(renewal['reason'], 'job timeout')
        self.assertIsNone(self.store.claim('mini'))
        self.store.complete('mini', job, claim['lease'], True, {})
        state = next(j for j in self.store.snapshot()['jobs'] if j['id'] == job)
        self.assertEqual(state['state'], 'failed')
        self.assertIsNotNone(self.store.claim('mini'))

    def test_timeout_retry_uses_fresh_attempt_deadline(self):
        job = self.job(timeoutSeconds=5, retries=1)
        first = self.store.claim('mini')
        self.now += 6
        self.store.complete('mini', job, first['lease'], True, {})
        second = self.store.claim('mini')
        self.assertNotEqual(first['lease'], second['lease'])
        self.assertFalse(self.store.renew('mini', job, second['lease'])['cancel'])
        self.now += 6
        self.store.complete('mini', job, second['lease'], False, {})
        self.assertEqual(self.store.snapshot()['jobs'][0]['state'], 'failed')

    def test_timeout_grace_cannot_be_extended_forever(self):
        job = self.job(timeoutSeconds=5, retries=1)
        claim = self.store.claim('mini')
        start = self.now
        for elapsed in [6, 15, 24]:
            self.now = start + elapsed
            self.assertLessEqual(self.store.renew('mini', job, claim['lease'])['expires'], start + 25)
        self.now = start + 25
        self.assertEqual(self.store.snapshot()['jobs'][0]['state'], 'queued')
        with self.assertRaises(Problem):
            self.store.renew('mini', job, claim['lease'])

    def test_timeout_event_only_emitted_once(self):
        self.job(timeoutSeconds=5)
        self.store.claim('mini')
        self.now += 6
        self.store.snapshot(); self.store.snapshot()
        events = self.store.snapshot()['events']
        self.assertEqual(sum(e['kind'] == 'job.timeout' for e in events), 1)

    def test_log_batch_order_and_atomic_validation(self):
        job = self.job()
        claim = self.store.claim('mini')
        self.store.logs('mini', job, claim['lease'], ['one', 'two', 'three'])
        for bad in [[], ['ok', 1], ['x' * 8193], ['😀' * 2049], ['x'] * 101, ['x' * 8000] * 7]:
            with self.assertRaises(Problem):
                self.store.logs('mini', job, claim['lease'], bad)
        rows = self.store.db.execute('SELECT text FROM logs ORDER BY seq').fetchall()
        self.assertEqual([r['text'] for r in rows], ['one', 'two', 'three'])

    def test_log_batch_stale_attempt_rejected(self):
        job = self.job(retries=1)
        claim = self.store.claim('mini')
        self.now += 21
        self.store.claim('mini')
        with self.assertRaises(Problem):
            self.store.logs('mini', job, claim['lease'], ['stale'])
