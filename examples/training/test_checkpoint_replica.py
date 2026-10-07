import io
import json
from pathlib import Path
import tempfile
import unittest

import checkpoint_replica as cr
import coordinated_checkpoint as cc


class FakeS3:
    def __init__(self):
        self.objects = {}

    def upload_file(self, path, bucket, key):
        self.objects[(bucket, key)] = Path(path).read_bytes()

    def put_object(self, Bucket, Key, Body):
        self.objects[(Bucket, Key)] = Body

    def get_object(self, Bucket, Key):
        if (Bucket, Key) not in self.objects:
            raise KeyError('NoSuchKey')
        return {'Body': io.BytesIO(self.objects[(Bucket, Key)])}

    def download_file(self, bucket, key, path):
        Path(path).write_bytes(self.objects[(bucket, key)])

    def get_paginator(self, name):
        s3 = self

        class Pages:
            def paginate(self, Bucket, Prefix):
                return [{'Contents': [{'Key': k} for (b, k) in sorted(s3.objects) if b == Bucket and k.startswith(Prefix)]}]
        return Pages()


def commit(root, step, payload):
    cc.save_global(root, payload, step, 0, 1)


class ReplicaTest(unittest.TestCase):
    def test_file_replica_round_trip(self):
        with tempfile.TemporaryDirectory() as src, tempfile.TemporaryDirectory() as rep, tempfile.TemporaryDirectory() as dst:
            target = 'file://' + rep
            self.assertIsNone(cr.replicate(src, target))
            for step in (5, 10, 15):
                commit(src, step, b'state-%d' % step)
                self.assertEqual(cr.replicate(src, target), step)
            self.assertEqual(cc.latest_committed_step(rep), 15)
            self.assertEqual(sorted(p.name for p in (Path(rep) / 'steps').iterdir()), ['10', '15'], 'keeps the newest 2')
            Path(src, 'steps/20').mkdir()  # uncommitted: never replicated
            self.assertEqual(cr.replicate(src, target), 15)

            self.assertEqual(cr.restore(target, dst), 15)
            self.assertEqual(cc.load_global(dst, 0, 1), (b'state-15', 15))
            self.assertIsNone(cr.restore(target, dst), 'a root with a committed step is never overwritten')

    def test_s3_replica_round_trip(self):
        s3 = FakeS3()
        with tempfile.TemporaryDirectory() as src, tempfile.TemporaryDirectory() as dst:
            self.assertIsNone(cr.restore('s3://bkt/jobs/a', dst, s3_client=s3), 'empty replica')
            commit(src, 7, b'weights')
            self.assertEqual(cr.replicate(src, 's3://bkt/jobs/a', s3_client=s3), 7)
            self.assertEqual(json.loads(s3.objects[('bkt', 'jobs/a/COMMITTED')]), {'step': 7})
            self.assertIn(('bkt', 'jobs/a/steps/7/COMMIT'), s3.objects)
            self.assertEqual(cr.restore('s3://bkt/jobs/a', dst, s3_client=s3), 7)
            self.assertEqual(cc.load_global(dst, 0, 1), (b'weights', 7))

    def test_rejects_bad_targets_and_incomplete_replicas(self):
        with tempfile.TemporaryDirectory() as src, tempfile.TemporaryDirectory() as rep, tempfile.TemporaryDirectory() as dst:
            commit(src, 1, b'x')
            for bad in ('http://x/y', 'file://host/path', 's3:///nobucket', '/plain/path'):
                with self.assertRaises(ValueError):
                    cr.replicate(src, bad)
            cr.replicate(src, 'file://' + rep)
            (Path(rep) / 'steps/1/COMMIT').unlink()
            with self.assertRaisesRegex(ValueError, 'no COMMIT'):
                cr.restore('file://' + rep, dst)


if __name__ == '__main__':
    unittest.main()
