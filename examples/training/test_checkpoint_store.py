# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import json
from pathlib import Path
import tempfile
import unittest
from checkpoint_store import save, load

class CheckpointTests(unittest.TestCase):
    def test_commit_corruption_and_uncommitted_blob(self):
        with tempfile.TemporaryDirectory() as root:
            first = save(root, b'old model', 10)
            Path(root, '0', 'uncommitted.bin').write_bytes(b'interrupted save')
            self.assertEqual(load(root)[0], b'old model')
            self.assertRaises(ValueError, load, root, world_size=2)
            Path(root, '0', first['blob']).write_bytes(b'corrupted')
            self.assertRaises(ValueError, load, root)
    def test_identity_and_traversal(self):
        with tempfile.TemporaryDirectory() as root:
            save(root, b'model', 1)
            pointer = Path(root, '0', 'latest.json')
            data = json.loads(pointer.read_text()); data['blob'] = '../secret.bin'
            pointer.write_text(json.dumps(data))
            self.assertRaises(ValueError, load, root)
    def test_pytorch_optimizer_roundtrip(self):
        try:
            import torch
            from pytorch_recovery import checkpoint, resume
        except ImportError:
            self.skipTest('install CPU PyTorch to verify framework recovery')
        with tempfile.TemporaryDirectory() as root:
            model = torch.nn.Linear(2, 1)
            opt = torch.optim.SGD(model.parameters(), lr=.1, momentum=.9)
            for _ in range(5):
                opt.zero_grad(); model(torch.ones(1, 2)).sum().backward(); opt.step()
            checkpoint(root, model, opt, 5)
            expected = {k: v.clone() for k, v in model.state_dict().items()}
            second = torch.nn.Linear(2, 1); newopt = torch.optim.SGD(second.parameters(), lr=.1, momentum=.9)
            self.assertEqual(resume(root, second, newopt), 5)
            for key, value in second.state_dict().items():
                self.assertTrue(torch.equal(value, expected[key]))
            self.assertEqual(len(newopt.state), len(opt.state))

if __name__ == '__main__': unittest.main()
