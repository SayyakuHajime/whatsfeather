"""Exercise the verifier shell contract without rebuilding Qt in this unit test."""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class VerificationIsolation(unittest.TestCase):
    def test_verifier_keeps_existing_evidence_and_allocates_new_private_runs(self):
        with tempfile.TemporaryDirectory(prefix="wf-verifier-test-") as temp:
            root = Path(temp)
            (root / "scripts").mkdir()
            shutil.copy2(ROOT / "scripts/verify.sh", root / "scripts/verify.sh")
            (root / "backend").mkdir()
            (root / "build/bin").mkdir(parents=True)
            (root / "mock-bin").mkdir()
            (root / "scratch").mkdir(mode=0o700)
            for legacy in ("qa", "sticker-qa"):
                target = root / ".hermes" / legacy
                target.mkdir(parents=True)
                (target / "sentinel").write_text("prior evidence")
            build = root / "scripts/build.sh"
            build.write_text("#!/bin/sh\nexit 0\n")
            build.chmod(0o700)
            # Only the allocation -c call needs a real Python interpreter.
            python = root / "mock-bin/python3"
            python.write_text("#!/bin/sh\nif [ \"$1\" = -c ]; then exec \"$REAL_PYTHON\" \"$@\"; fi\nexit 0\n")
            python.chmod(0o700)
            for tool in ("go", "ctest"):
                path = root / "mock-bin" / tool
                path.write_text("#!/bin/sh\nexit 0\n")
                path.chmod(0o700)
            gui = root / "build/bin/whatsfeather"
            gui.write_text("#!/bin/sh\nprintf '%s\\n' \"$WHATSFEATHER_QA_ROOT\" >> \"$RUN_RECORD\"\nwhile [ $# -gt 0 ]; do\n if [ \"$1\" = --screenshot ]; then shift; printf capture > \"$1\"; fi\n shift\ndone\n")
            gui.chmod(0o700)
            env = dict(os.environ, PATH=str(root / "mock-bin") + os.pathsep + os.environ["PATH"],
                       TMPDIR=str(root / "scratch"), REAL_PYTHON=sys.executable, RUN_RECORD=str(root / "runs"))
            for _ in range(2):
                run = subprocess.run(["bash", str(root / "scripts/verify.sh")], env=env, capture_output=True, text=True)
                self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
            runs = (root / "runs").read_text().splitlines()
            self.assertTrue(all(runs), "verifier must supply an isolated evidence root")
            unique = set(runs)
            self.assertEqual(len(unique), 2)
            for path in unique:
                evidence = Path(path)
                self.assertEqual(evidence.stat().st_mode & 0o777, 0o700)
                self.assertTrue((evidence / "dark.png").is_file())
                self.assertTrue((evidence / "light.png").is_file())
                self.assertFalse((evidence / "runtime").exists(), "test runtime is cleaned on exit")
            for legacy in ("qa", "sticker-qa"):
                path = root / ".hermes" / legacy
                self.assertEqual(sorted(x.name for x in path.iterdir()), ["sentinel"])
                self.assertEqual((path / "sentinel").read_text(), "prior evidence")


if __name__ == "__main__":
    unittest.main(verbosity=2)
