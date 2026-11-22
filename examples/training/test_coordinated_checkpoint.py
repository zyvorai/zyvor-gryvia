# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import json
import multiprocessing as mp
from pathlib import Path
import tempfile
import unittest

from coordinated_checkpoint import (discard_uncommitted, latest_committed_step, load_global, load_replicated, prune,
                                    save_global)


def _rank(root, step, rank, world, payload, timeout, out, skip_save=False, delay=0.0):
    import time
    time.sleep(delay)
    if skip_save:
        out.put((rank, 'died'))
        return
    try:
        out.put((rank, save_global(root, payload, step, rank, world, timeout=timeout, poll=0.02)))
    except TimeoutError:
        out.put((rank, 'timeout'))


def run_step(root, step, world, timeout=5.0, dead=(), delays=None):
    ctx = mp.get_context('spawn')
    out = ctx.Queue()
    procs = [ctx.Process(target=_rank, args=(root, step, r, world, ('model-%d-step-%d' % (r, step)).encode(), timeout, out,
                                             r in dead, (delays or {}).get(r, 0.0))) for r in range(world)]
    for p in procs:
        p.start()
    results = dict(out.get(timeout=30) for _ in procs)
    for p in procs:
        p.join(30)
    return results


class CoordinatedCheckpointTests(unittest.TestCase):
    def test_all_ranks_commit_and_resume_the_same_step(self):
        with tempfile.TemporaryDirectory() as root:
            self.assertEqual(run_step(root, 10, 3, delays={0: 0.2}), {0: 10, 1: 10, 2: 10})
            self.assertEqual(latest_committed_step(root), 10)
            for r in range(3):
                payload, step = load_global(root, r, 3)
                self.assertEqual((payload, step), (('model-%d-step-10' % r).encode(), 10))

    def test_a_dead_rank_leaves_the_previous_step_as_the_resume_point_for_everyone(self):
        with tempfile.TemporaryDirectory() as root:
            run_step(root, 10, 3)
            res = run_step(root, 20, 3, timeout=0.6, dead=(2,))
            self.assertEqual(res[0], 'timeout')          # rank 0 never saw rank 2
            self.assertEqual(res[1], 'timeout')          # rank 1 never saw a COMMIT
            self.assertEqual(latest_committed_step(root), 10)
            # rank 0 and 1 DID write step 20 blobs, but nobody may resume from them
            self.assertTrue(Path(root, 'steps', '20', 'rank-0.bin').exists())
            for r in range(3):
                self.assertEqual(load_global(root, r, 3)[1], 10)

    def test_nothing_committed_means_nothing_to_load(self):
        with tempfile.TemporaryDirectory() as root:
            self.assertIsNone(load_global(root, 0, 2))

    def test_world_size_mismatch_and_bad_inputs(self):
        with tempfile.TemporaryDirectory() as root:
            run_step(root, 5, 2)
            self.assertRaises(ValueError, load_global, root, 0, 4)
            self.assertRaises(ValueError, save_global, root, b'x', 6, 2, 2)
            self.assertRaises(ValueError, save_global, root, b'x', -1, 0, 1)

    def test_corruption_is_detected_against_the_commit_record(self):
        with tempfile.TemporaryDirectory() as root:
            run_step(root, 7, 2)
            Path(root, 'steps', '7', 'rank-1.bin').write_bytes(b'tampered!!')
            self.assertEqual(load_global(root, 0, 2)[1], 7)
            self.assertRaises(ValueError, load_global, root, 1, 2)

    def test_missing_commit_record_is_an_error_not_a_silent_old_step(self):
        with tempfile.TemporaryDirectory() as root:
            run_step(root, 3, 1)
            Path(root, 'steps', '3', 'COMMIT').unlink()
            self.assertRaises(ValueError, load_global, root, 0, 1)

    def test_committed_pointer_never_moves_backwards(self):
        with tempfile.TemporaryDirectory() as root:
            run_step(root, 30, 1)
            run_step(root, 20, 1)   # a late, older step finishing afterwards
            self.assertEqual(latest_committed_step(root), 30)

    def test_prune_keeps_recent_committed_and_drops_old_uncommitted(self):
        with tempfile.TemporaryDirectory() as root:
            for s in (1, 2, 3, 4):
                run_step(root, s, 1)
            run_step(root, 5, 2, timeout=0.3, dead=(1,))   # uncommitted, newer than latest committed (4)
            Path(root, 'steps', '0').mkdir(parents=True)    # uncommitted and old
            removed = prune(root, keep=2)
            self.assertEqual(removed, [0, 1, 2])
            self.assertEqual(sorted(p.name for p in Path(root, 'steps').iterdir()), ['3', '4', '5'])
            self.assertEqual(load_global(root, 0, 1)[1], 4)

    def test_commit_record_lists_every_rank(self):
        with tempfile.TemporaryDirectory() as root:
            run_step(root, 9, 3)
            rec = json.loads(Path(root, 'steps', '9', 'COMMIT').read_text())
            self.assertEqual([r['rank'] for r in rec['ranks']], [0, 1, 2])
            self.assertEqual(rec['world_size'], 3)

    def test_replicated_state_resumes_with_another_world_size(self):
        with tempfile.TemporaryDirectory() as root:
            self.assertIsNone(load_replicated(root))
            run_step(root, 10, 2)
            with self.assertRaises(ValueError):
                load_global(root, 0, 1)
            self.assertEqual(load_replicated(root), (b'model-0-step-10', 10))
            Path(root, 'steps', '10', 'rank-0.bin').write_bytes(b'tampered')
            with self.assertRaises(ValueError):
                load_replicated(root)

    def test_discard_uncommitted_drops_half_published_steps_above_the_commit(self):
        with tempfile.TemporaryDirectory() as root:
            run_step(root, 10, 2)
            run_step(root, 15, 2, timeout=0.3, dead=(0,))   # rank 1 published, rank 0 died: no COMMIT
            self.assertTrue(Path(root, 'steps', '15', 'rank-1.json').exists())
            self.assertEqual(discard_uncommitted(root), [15])
            self.assertEqual(sorted(p.name for p in Path(root, 'steps').iterdir()), ['10'])
            # Saving step 15 again with a smaller world now commits only what this attempt published.
            self.assertEqual(run_step(root, 15, 1), {0: 15})
            self.assertEqual(json.loads(Path(root, 'steps', '15', 'COMMIT').read_text())['world_size'], 1)
            with tempfile.TemporaryDirectory() as empty:
                self.assertEqual(discard_uncommitted(empty), [])


if __name__ == '__main__':
    unittest.main()
