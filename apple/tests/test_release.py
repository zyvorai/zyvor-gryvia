# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import json, os, subprocess, sys, tempfile, unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def run(*cmd, **kw):
    return subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True, **kw)


class VersionTests(unittest.TestCase):
    def test_all_targets_match_version_file(self):
        r = run(sys.executable, "scripts/set-version.py", "--check")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_version_is_semver(self):
        v = (ROOT / "VERSION").read_text().strip()
        self.assertRegex(v, r"^\d+\.\d+\.\d+")

    def test_cli_reports_version_and_licence(self):
        r = run(sys.executable, "scripts/gryvia.py", "--version")
        v = (ROOT / "VERSION").read_text().strip()
        self.assertIn(v, r.stdout)
        self.assertIn("BUSL-1.1", r.stdout)


class SbomTests(unittest.TestCase):
    def test_cyclonedx_shape(self):
        r = run(sys.executable, "scripts/make-sbom.py", "9.9.9")
        self.assertEqual(r.returncode, 0, r.stderr)
        bom = json.loads(r.stdout)
        self.assertEqual((bom["bomFormat"], bom["specVersion"]), ("CycloneDX", "1.5"))
        self.assertEqual(bom["metadata"]["component"]["version"], "9.9.9")
        self.assertEqual(bom["metadata"]["component"]["licenses"][0]["license"]["id"], "BUSL-1.1")
        scopes = {c["name"]: c["scope"] for c in bom["components"]}
        self.assertEqual(scopes["python-standard-library"], "required")
        self.assertEqual(scopes["playwright"], "excluded")   # test-time only, never shipped


class ReleaseArchiveTests(unittest.TestCase):
    def test_package_release_builds_archives_and_checksums(self):
        with tempfile.TemporaryDirectory() as out:
            r = run("bash", "scripts/package-release.sh", env={**os.environ, "OUT": out})
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
            v = (ROOT / "VERSION").read_text().strip()
            names = sorted(p.name for p in Path(out).iterdir())
            self.assertIn(f"gryvia-server-{v}.tar.gz", names)
            self.assertIn(f"gryvia-sdk-{v}.tar.gz", names)
            self.assertIn(f"gryvia-{v}.cdx.json", names)
            sums = (Path(out) / "SHA256SUMS").read_text().splitlines()
            self.assertEqual(len(sums), 3)
            # the archive must not leak local state or secrets
            listing = subprocess.run(["tar", "-tzf", f"{out}/gryvia-server-{v}.tar.gz"], capture_output=True, text=True).stdout
            for bad in ("credentials.env", "config.json", ".db", "local/", "__pycache__", ".build/"):
                self.assertNotIn(bad, listing)
            self.assertIn(f"gryvia-server-{v}/LICENSE", listing)

    def test_package_release_rejects_mismatched_version(self):
        with tempfile.TemporaryDirectory() as out:
            r = run("bash", "scripts/package-release.sh", "9.9.9", env={**os.environ, "OUT": out})
            self.assertNotEqual(r.returncode, 0)


class CommunityFilesTests(unittest.TestCase):
    def test_required_files_exist(self):
        for rel in ["LICENSE", "NOTICE", "SECURITY.md", "CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "CHANGELOG.md", "VERSION",
                    "docs/RELEASE.md", "docs/TESTING.md", ".github/PULL_REQUEST_TEMPLATE.md", ".github/CODEOWNERS",
                    ".github/ISSUE_TEMPLATE/bug.yml", ".github/ISSUE_TEMPLATE/feature.yml", ".github/workflows/ci.yml",
                    ".github/workflows/release.yml", "packaging/homebrew/gryvia.rb"]:
            self.assertTrue((ROOT / rel).is_file(), rel)

    def test_licence_wording_is_source_available(self):
        readme = (ROOT / "README.md").read_text().lower()
        self.assertNotIn("open source", readme.replace("source-available", "").replace("not open source", "").replace("not osi open source", ""))

    def test_security_has_a_contact(self):
        s = (ROOT / "SECURITY.md").read_text()
        self.assertRegex(s, r"(security@|advisories/new|private)")


if __name__ == "__main__":
    unittest.main()
