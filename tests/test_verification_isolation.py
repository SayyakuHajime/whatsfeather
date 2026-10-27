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
    def _prepare_fixture(self, root: Path) -> None:
        (root / "scripts").mkdir()
        shutil.copy2(ROOT / "scripts/verify.sh", root / "scripts/verify.sh")
        helper = root / "scripts/verify-encrypted-profile.sh"
        # Deliberate test stub: shell-contract coverage must never mount real FUSE.
        helper.write_text(
            "#!/bin/sh\n"
            "printf '%s|%s\\n' \"${WHATSFEATHER_VERIFY_ENCRYPTED_PROFILE:-unset}\" "
            "\"${WHATSFEATHER_TEST_GCRYPTFS:-unset}\" >> \"$GATE_RECORD\"\n"
            "exit \"${ENCRYPTED_GATE_EXIT:-0}\"\n",
            encoding="utf-8",
        )
        helper.chmod(0o700)
        (root / "backend").mkdir()
        (root / "build/bin").mkdir(parents=True)
        (root / "mock-bin").mkdir()
        (root / "scratch").mkdir(mode=0o700)
        for legacy in ("qa", "sticker-qa"):
            target = root / ".hermes" / legacy
            target.mkdir(parents=True)
            (target / "sentinel").write_text("prior evidence")

        build = root / "scripts/build.sh"
        build.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        build.chmod(0o700)
        # Only the allocation -c call needs a real Python interpreter.
        python = root / "mock-bin/python3"
        python.write_text(
            "#!/bin/sh\nif [ \"$1\" = -c ]; then exec \"$REAL_PYTHON\" \"$@\"; fi\nexit 0\n",
            encoding="utf-8",
        )
        python.chmod(0o700)
        for tool in ("go", "ctest"):
            path = root / "mock-bin" / tool
            path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            path.chmod(0o700)
        gui = root / "build/bin/whatsfeather"
        gui.write_text(
            "#!/bin/sh\nprintf '%s\\n' \"$WHATSFEATHER_QA_ROOT\" >> \"$RUN_RECORD\"\n"
            "while [ $# -gt 0 ]; do\n"
            " if [ \"$1\" = --screenshot ]; then shift; printf capture > \"$1\"; fi\n"
            " shift\ndone\n",
            encoding="utf-8",
        )
        gui.chmod(0o700)

    def _environment(self, root: Path) -> dict[str, str]:
        env = dict(
            os.environ,
            PATH=str(root / "mock-bin") + os.pathsep + os.environ["PATH"],
            TMPDIR=str(root / "scratch"),
            REAL_PYTHON=sys.executable,
            RUN_RECORD=str(root / "runs"),
            GATE_RECORD=str(root / "gate-runs"),
            ENCRYPTED_GATE_EXIT="0",
        )
        for key in (
            "WHATSFEATHER_VERIFY_ENCRYPTED_PROFILE",
            "WHATSFEATHER_TEST_GCRYPTFS",
            "WHATSFEATHER_TEST_FUSERMOUNT3",
            "WHATSFEATHER_RUN_NATIVE_ENCRYPTED_ACCEPTANCE",
            "WHATSFEATHER_NATIVE_PROFILE_TEST_BIN",
        ):
            env.pop(key, None)
        return env

    def _run_verifier(self, root: Path, env: dict[str, str]) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["bash", str(root / "scripts/verify.sh")],
            env=env,
            capture_output=True,
            text=True,
        )

    def test_encrypted_gate_opt_in_dispatches_to_fixture_stub(self):
        with tempfile.TemporaryDirectory(prefix="wf-verifier-test-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            self._prepare_fixture(root)
            env = self._environment(root)
            env["WHATSFEATHER_VERIFY_ENCRYPTED_PROFILE"] = "1"
            env["WHATSFEATHER_TEST_GCRYPTFS"] = "fixture-only-not-an-engine"

            run = self._run_verifier(root, env)

            self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
            self.assertEqual(
                (root / "gate-runs").read_text(encoding="utf-8").splitlines(),
                ["1|fixture-only-not-an-engine"],
                "the opt-in dispatch test uses only the fixture helper, never real FUSE acceptance",
            )

    def test_encrypted_gate_defaults_to_clear_skip_without_dispatch(self):
        with tempfile.TemporaryDirectory(prefix="wf-verifier-test-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            self._prepare_fixture(root)
            env = self._environment(root)
            env.pop("WHATSFEATHER_VERIFY_ENCRYPTED_PROFILE", None)
            env.pop("WHATSFEATHER_TEST_GCRYPTFS", None)

            run = self._run_verifier(root, env)

            self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
            self.assertIn("SKIP: real FUSE/native encrypted-profile acceptance is opt-in", run.stdout)
            self.assertFalse((root / "gate-runs").exists(), "default-clear must not call the helper")

    def test_encrypted_gate_rejects_invalid_flag_without_dispatch(self):
        with tempfile.TemporaryDirectory(prefix="wf-verifier-test-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            self._prepare_fixture(root)
            env = self._environment(root)
            env["WHATSFEATHER_VERIFY_ENCRYPTED_PROFILE"] = "true"

            run = self._run_verifier(root, env)

            self.assertEqual(run.returncode, 2, run.stdout + run.stderr)
            self.assertIn("WHATSFEATHER_VERIFY_ENCRYPTED_PROFILE must be 1 when set", run.stderr)
            self.assertFalse((root / "gate-runs").exists(), "invalid opt-in must not call the helper")

    def test_encrypted_gate_propagates_fixture_helper_failure(self):
        with tempfile.TemporaryDirectory(prefix="wf-verifier-test-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            self._prepare_fixture(root)
            env = self._environment(root)
            env["WHATSFEATHER_VERIFY_ENCRYPTED_PROFILE"] = "1"
            env["WHATSFEATHER_TEST_GCRYPTFS"] = "fixture-only-not-an-engine"
            env["ENCRYPTED_GATE_EXIT"] = "23"

            run = self._run_verifier(root, env)

            self.assertEqual(run.returncode, 23, run.stdout + run.stderr)
            self.assertEqual((root / "gate-runs").read_text(encoding="utf-8").splitlines(),
                             ["1|fixture-only-not-an-engine"])

    def test_verifier_keeps_existing_evidence_and_allocates_new_private_runs(self):
        with tempfile.TemporaryDirectory(prefix="wf-verifier-test-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            self._prepare_fixture(root)
            env = self._environment(root)
            for _ in range(2):
                run = self._run_verifier(root, env)
                self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
                self.assertIn(
                    "SKIP: real FUSE/native encrypted-profile acceptance is opt-in",
                    run.stdout,
                    "the shell-contract fixture must not inherit the caller's encrypted-gate opt-in",
                )
            self.assertFalse((root / "gate-runs").exists(), "the isolated evidence test must not dispatch the gate")
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
