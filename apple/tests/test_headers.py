# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import subprocess, sys, unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class HeaderTests(unittest.TestCase):
    def test_all_sources_carry_spdx_and_copyright(self):
        r = subprocess.run([sys.executable, str(ROOT / "scripts/check-headers.py")], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stdout)


if __name__ == "__main__":
    unittest.main()
