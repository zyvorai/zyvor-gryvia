"""Multi-process CPU (gloo) tests for dcp_checkpoint: save at world size 4, load at 2 and 3, with DDP
(replicated) and FSDP2 (fully_shard on a CPU device mesh, so parameters and Adam moments really are resharded).

Needs torch (CPU). Each phase spawns real processes that join one gloo process group through a file store.
"""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

import torch
import torch.distributed as dist
import torch.multiprocessing as mp
from torch.distributed.checkpoint.state_dict import StateDictOptions, get_state_dict
from torch.distributed.device_mesh import init_device_mesh
from torch.distributed.fsdp import fully_shard
from torch.nn.parallel import DistributedDataParallel as DDP

sys.path.insert(0, str(Path(__file__).resolve().parent))
import checkpoint_contract  # noqa: E402
import coordinated_checkpoint as cc  # noqa: E402
import dcp_checkpoint  # noqa: E402

DIGEST = 'a' * 64


def build(seed, kind, world):
    torch.manual_seed(seed)
    model = torch.nn.Sequential(torch.nn.Linear(6, 12), torch.nn.ReLU(), torch.nn.Linear(12, 3))
    if kind == 'ddp':
        return DDP(model), model
    mesh = init_device_mesh('cpu', (world,))
    for layer in (model[0], model[2]):
        fully_shard(layer, mesh=mesh)
    fully_shard(model, mesh=mesh)
    return model, model


def train(ddp, opt, steps, rank, world, first=0):
    for s in range(first, first + steps):
        g = torch.Generator().manual_seed(1000 + s)
        x, y = torch.randn(12, 6, generator=g), torch.randn(12, 3, generator=g)
        xs, ys = x.chunk(world)[rank], y.chunk(world)[rank]
        opt.zero_grad()
        torch.nn.functional.mse_loss(ddp(xs), ys).backward()
        opt.step()


def full_state(ddp, opt):
    """Every tensor in full on every rank (gathered from the shards for FSDP)."""
    model_sd, optim_sd = get_state_dict(ddp, opt, options=StateDictOptions(full_state_dict=True))
    return {'model': model_sd, 'optim': optim_sd}


def flatten(obj, prefix=''):
    if isinstance(obj, dict):
        out = {}
        for k in sorted(obj, key=str):
            out.update(flatten(obj[k], '%s/%s' % (prefix, k)))
        return out
    if isinstance(obj, (list, tuple)):
        out = {}
        for i, v in enumerate(obj):
            out.update(flatten(v, '%s/%d' % (prefix, i)))
        return out
    return {prefix: obj}


def same(a, b):
    fa, fb = flatten(a), flatten(b)
    if fa.keys() != fb.keys():
        return 'keys differ: %s' % sorted(set(fa) ^ set(fb))
    for k in fa:
        x, y = fa[k], fb[k]
        if isinstance(x, torch.Tensor):
            if not isinstance(y, torch.Tensor) or not torch.equal(x, y):
                return 'tensor %s differs' % k
        elif x != y:
            return 'value %s differs: %r != %r' % (k, x, y)
    return None


def worker(rank, world, phase, root, store, kind):
    dist.init_process_group('gloo', init_method='file://' + store, rank=rank, world_size=world)
    try:
        if phase == 'save':
            ddp, model = build(0, kind, world)
            opt = torch.optim.AdamW(model.parameters(), lr=0.01)
            train(ddp, opt, 4, rank, world)
            ref = full_state(ddp, opt)
            dcp_checkpoint.save(root, 4, ddp, opt)
            if rank == 0:
                torch.save(ref, os.path.join(root, 'reference.pt'))
        else:
            ddp, model = build(99, kind, world)  # different init: everything must come from the checkpoint
            opt = torch.optim.AdamW(model.parameters(), lr=0.5)
            found = dcp_checkpoint.load(root, ddp, opt)
            got = full_state(ddp, opt)
            ref = torch.load(os.path.join(root, 'reference.pt'), weights_only=False)
            diff = same(ref, got)
            # every rank must also hold the same weights after one more synchronized step
            train(ddp, opt, 1, rank, world, first=4)
            weights = full_state(ddp, opt)['model']
            checksum = torch.stack([t.sum() for t in weights.values()]).sum().reshape(1)
            gathered = [torch.zeros(1) for _ in range(world)]
            dist.all_gather(gathered, checksum)
            consistent = all(torch.equal(g, gathered[0]) for g in gathered)
            Path(root, 'result-%d-%d.json' % (world, rank)).write_text(json.dumps(
                {'found': list(found) if found else None, 'diff': diff, 'consistent': consistent,
                 'adamSteps': [float(s['step']) for s in opt.state_dict()['state'].values()]}))
    finally:
        dist.destroy_process_group()


def spawn(world, phase, root, kind='ddp'):
    fd, store = tempfile.mkstemp(prefix='gloo-store-')
    os.close(fd)
    os.unlink(store)
    mp.spawn(worker, args=(world, phase, root, store, kind), nprocs=world, join=True)


class DCPReshardTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.root = cls.tmp.name
        checkpoint_contract.bind_contract(cls.root, DIGEST, DIGEST)
        spawn(4, 'save', cls.root)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def test_commit_record(self):
        self.assertEqual(cc.latest_committed_step(self.root), 4)
        record = dcp_checkpoint.read_commit(self.root, 4)
        self.assertEqual((record['format'], record['world_size']), ('dcp', 4))
        self.assertIn('.metadata', [f['name'] for f in record['files']])

    def test_contract_reports_reshardable(self):
        out = checkpoint_contract.verify_committed(self.root, DIGEST, DIGEST)
        self.assertTrue(out['optimizerReshardable'])
        self.assertEqual((out['format'], out['worldSize'], out['step']), ('dcp', 4, 4))

    def test_load_at_smaller_world_sizes(self):
        for world in (2, 3):
            spawn(world, 'load', self.root)
            for rank in range(world):
                r = json.loads(Path(self.root, 'result-%d-%d.json' % (world, rank)).read_text())
                self.assertEqual(r['found'], [4, 4], 'world %d rank %d' % (world, rank))
                self.assertIsNone(r['diff'], 'world %d rank %d: %s' % (world, rank, r['diff']))
                self.assertTrue(r['consistent'], 'world %d rank %d diverged after resume' % (world, rank))
                self.assertEqual(r['adamSteps'], [5.0] * 4, 'optimizer step counters not restored')

    def test_fsdp_reshards_parameters_and_optimizer_state(self):
        with tempfile.TemporaryDirectory() as tmp:
            spawn(4, 'save', tmp, 'fsdp')
            for world in (2, 3):
                spawn(world, 'load', tmp, 'fsdp')
                for rank in range(world):
                    r = json.loads(Path(tmp, 'result-%d-%d.json' % (world, rank)).read_text())
                    self.assertEqual(r['found'], [4, 4])
                    self.assertIsNone(r['diff'], 'FSDP world %d rank %d: %s' % (world, rank, r['diff']))
                    self.assertTrue(r['consistent'])
                    self.assertEqual(r['adamSteps'], [5.0] * 4)

    def test_uncommitted_step_is_ignored_and_corruption_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            spawn(4, 'save', tmp)
            Path(tmp, 'steps/8/dcp').mkdir(parents=True)
            Path(tmp, 'steps/8/dcp/.metadata').write_bytes(b'half-written')
            spawn(2, 'load', tmp)
            self.assertEqual(json.loads(Path(tmp, 'result-2-0.json').read_text())['found'], [4, 4])
            self.assertEqual(cc.discard_uncommitted(tmp), [8])

            data = next(p for p in Path(tmp, 'steps/4/dcp').iterdir() if p.suffix == '.distcp')
            blob = bytearray(data.read_bytes())
            blob[-1] ^= 0xFF
            data.write_bytes(bytes(blob))
            with self.assertRaisesRegex(ValueError, 'checksum'):
                checkpoint_contract.verify_dcp_files(Path(tmp, 'steps/4/dcp'), dcp_checkpoint.read_commit(tmp, 4)['files'])
            with self.assertRaises(Exception):
                spawn(2, 'load', tmp)

    def test_blob_checkpoint_is_not_a_dcp_checkpoint(self):
        with tempfile.TemporaryDirectory() as tmp:
            cc.save_global(tmp, b'weights', 1, 0, 1)
            with self.assertRaisesRegex(ValueError, 'not a DCP checkpoint'):
                dcp_checkpoint.read_commit(tmp, 1)


if __name__ == '__main__':
    unittest.main()
