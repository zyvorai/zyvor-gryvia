# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

WORKER = Path(__file__).with_name("checkpoint_worker.py")


class CheckpointProtocolTest(unittest.TestCase):
    def test_save_acknowledge_and_resume(self):
        with tempfile.TemporaryDirectory() as directory:
            env = dict(os.environ, GRYVIA_CHECKPOINT_DIR=directory, GRYVIA_RESUME_IF_PRESENT="true")
            first = subprocess.Popen([sys.executable, str(WORKER), "--interval", "0.01"], env=env, stdout=subprocess.PIPE, text=True)
            try:
                self.assertEqual(first.stdout.readline().strip(), "resumed step=0")
                subprocess.run([sys.executable, str(WORKER), "--request", "--timeout", "3"], env=env, check=True, timeout=5)
                checkpoint = json.loads((Path(directory) / "state-0.json").read_text())
                ack = json.loads((Path(directory) / "ack-0.json").read_text())
                self.assertGreater(checkpoint["step"], 0)
                self.assertGreaterEqual(checkpoint["step"], ack["step"])
            finally:
                first.terminate()
                first.communicate(timeout=5)
            saved = json.loads((Path(directory) / "state-0.json").read_text())["step"]
            second = subprocess.Popen([sys.executable, str(WORKER)], env=env, stdout=subprocess.PIPE, text=True)
            try:
                self.assertEqual(second.stdout.readline().strip(), f"resumed step={saved}")
            finally:
                second.terminate()
                second.communicate(timeout=5)

    def test_hook_fails_without_trainer(self):
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([sys.executable, str(WORKER), "--request", "--timeout", "0.1"], env=dict(os.environ, GRYVIA_CHECKPOINT_DIR=directory), capture_output=True, timeout=3)
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
