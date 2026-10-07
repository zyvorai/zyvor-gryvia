import hashlib
import json
import tempfile
from pathlib import Path
import unittest

from checkpoint_contract import bind_contract, verify_committed
from coordinated_checkpoint import save_global

DIGEST = 'a' * 64


class ContractTest(unittest.TestCase):
    def test_verify_and_corruption(self):
        with tempfile.TemporaryDirectory() as tmp:
            bind_contract(tmp, DIGEST, DIGEST)
            save_global(tmp, b'weights', 10, 0, 1)
            out = verify_committed(tmp, DIGEST, DIGEST)
            self.assertTrue(out['integrityVerified'])
            self.assertFalse(out['durableStorage'])
            self.assertFalse(out['optimizerReshardable'])
            (Path(tmp) / 'steps/10/rank-0.bin').write_bytes(b'corrupt')
            with self.assertRaisesRegex(ValueError, 'checksum'):
                verify_committed(tmp, DIGEST, DIGEST)

    def test_cannot_rebind_or_claim_existing_checkpoint(self):
        with tempfile.TemporaryDirectory() as tmp:
            bind_contract(tmp, DIGEST, DIGEST)
            with self.assertRaises(ValueError):
                bind_contract(tmp, 'b' * 64, DIGEST)
        with tempfile.TemporaryDirectory() as tmp:
            save_global(tmp, b'weights', 10, 0, 1)
            with self.assertRaises(ValueError):
                bind_contract(tmp, DIGEST, DIGEST)

    def test_verify_every_rank(self):
        with tempfile.TemporaryDirectory() as tmp:
            bind_contract(tmp, DIGEST, DIGEST)
            save_global(tmp, b'weights', 10, 0, 1)
            directory = Path(tmp) / 'steps/10'
            (directory / 'rank-1.bin').write_bytes(b'rank1')
            commit = json.loads((directory / 'COMMIT').read_text())
            commit['world_size'] = 2
            commit['ranks'].append({'rank': 1, 'bytes': 5, 'sha256': hashlib.sha256(b'rank1').hexdigest()})
            (directory / 'COMMIT').write_text(json.dumps(commit))
            self.assertEqual(verify_committed(tmp, DIGEST, DIGEST)['worldSize'], 2)
            (directory / 'rank-1.bin').write_bytes(b'bad')
            with self.assertRaises(ValueError):
                verify_committed(tmp, DIGEST, DIGEST)


if __name__ == '__main__':
    unittest.main()
