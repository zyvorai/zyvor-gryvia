from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import threading
import unittest

import checkpoint_status


class FakeAPI(BaseHTTPRequestHandler):
    cm = None
    conflicts = 0
    auth = []

    def log_message(self, *args):
        pass

    def _send(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        FakeAPI.auth.append(self.headers.get('Authorization'))
        if self.path != '/api/v1/namespaces/ns/configmaps/job-checkpoint-status':
            return self._send(404, {})
        self._send(200, FakeAPI.cm)

    def do_PUT(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if FakeAPI.conflicts:
            FakeAPI.conflicts -= 1
            FakeAPI.cm['data']['requestReason'] = 'written in between'
            FakeAPI.cm['metadata']['resourceVersion'] = str(int(FakeAPI.cm['metadata']['resourceVersion']) + 1)
            return self._send(409, {'reason': 'Conflict'})
        if body['metadata']['resourceVersion'] != FakeAPI.cm['metadata']['resourceVersion']:
            return self._send(409, {'reason': 'Conflict'})
        body['metadata']['resourceVersion'] = str(int(body['metadata']['resourceVersion']) + 1)
        FakeAPI.cm = body
        self._send(200, body)


class StatusTest(unittest.TestCase):
    def setUp(self):
        FakeAPI.cm = {'metadata': {'name': 'job-checkpoint-status', 'resourceVersion': '1'}, 'data': {}}
        FakeAPI.conflicts, FakeAPI.auth = 0, []
        self.server = HTTPServer(('127.0.0.1', 0), FakeAPI)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.t = [0.0]
        self.r = checkpoint_status.StatusReporter('job-checkpoint-status', 'ns', 'http://127.0.0.1:%d' % self.server.server_port,
                                                  token='tok', clock=lambda: self.t[0])

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def test_committed_and_progress(self):
        self.assertTrue(self.r.committed(40))
        self.assertEqual(FakeAPI.cm['data']['committedStep'], '40')
        self.assertEqual(FakeAPI.cm['data']['currentStep'], '40')
        self.assertIn('Bearer tok', FakeAPI.auth)
        self.t[0] = 5
        self.assertFalse(self.r.progress(45), 'throttled within every_seconds of the last write')
        self.t[0] = 11
        self.assertTrue(self.r.progress(47))
        self.assertEqual(FakeAPI.cm['data']['currentStep'], '47')
        self.assertEqual(FakeAPI.cm['data']['committedStep'], '40')

    def test_conflict_is_retried_and_keeps_other_keys(self):
        FakeAPI.conflicts = 1
        self.assertTrue(self.r.committed(10))
        self.assertEqual(FakeAPI.cm['data']['requestReason'], 'written in between')
        self.assertEqual(FakeAPI.cm['data']['committedStep'], '10')

    def test_requested_until_a_commit_answers(self):
        self.assertIsNone(self.r.requested())
        FakeAPI.cm['data'].update(requestedAt='2026-10-07T12:00:00Z', requestReason='GPU degraded', committedAt='2026-10-07T11:00:00Z')
        self.assertIsNone(self.r.requested(), 'polled at most every poll_seconds')
        self.t[0] = 10
        self.assertEqual(self.r.requested(), 'GPU degraded')
        self.r.committed(50, now=datetime(2026, 10, 7, 12, 5, tzinfo=timezone.utc))
        self.assertIsNone(self.r.requested(), 'a commit clears the cached request')
        self.t[0] = 20
        self.assertIsNone(self.r.requested(), 'committedAt is now later than requestedAt')

    def test_disabled_and_api_errors_never_raise(self):
        off = checkpoint_status.from_env({})
        self.assertFalse(off.enabled)
        self.assertFalse(off.committed(1))
        self.assertIsNone(off.requested())
        broken = checkpoint_status.StatusReporter('missing', 'ns', 'http://127.0.0.1:%d' % self.server.server_port)
        self.assertFalse(broken.committed(1))
        dead = checkpoint_status.StatusReporter('x', 'ns', 'http://127.0.0.1:1')
        self.assertFalse(dead.committed(1))
        self.assertIsNone(dead.requested())


if __name__ == '__main__':
    unittest.main()
