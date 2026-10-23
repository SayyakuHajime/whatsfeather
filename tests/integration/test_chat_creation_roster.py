"""Exercise the real Qt New chat dialog with a private synthetic RPC fixture."""
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
BINARY = ROOT / "build/bin/whatsfeather"


class ChatCreationRoster(unittest.TestCase):
    def setUp(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable first")

    def isolated_environment(self, root):
        for name in ("config", "data", "cache"):
            (root / name).mkdir(mode=0o700)
        env = dict(os.environ, QT_QPA_PLATFORM="offscreen",
                   QT_QUICK_BACKEND="software", QT_FORCE_STDERR_LOGGING="1",
                   QT_SCALE_FACTOR="1", XDG_RUNTIME_DIR=str(root),
                   XDG_CONFIG_HOME=str(root / "config"),
                   XDG_DATA_HOME=str(root / "data"),
                   XDG_CACHE_HOME=str(root / "cache"))
        return env

    def run_app(self, args, env):
        return subprocess.run([str(BINARY), *args], env=env, capture_output=True,
                              text=True, timeout=20)

    def test_keyboard_roster_selection_opens_only_the_verified_returned_chat(self):
        with tempfile.TemporaryDirectory(prefix="wf-roster-ui-") as temp:
            root = Path(temp)
            env = self.isolated_environment(root)
            result = self.run_app([
                "--demo", "--chat-roster-test", "--data-dir", str(root / "profile")
            ], env)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native chat roster UI PASS:", output)
            self.assertNotIn("Native chat roster UI FAIL:", output)
            self.assertNotIn("QQmlApplicationEngine failed", output)
            self.assertNotIn("qml:", output.lower())

    def test_fixture_flag_rejects_live_mode_and_external_socket_without_connecting(self):
        with tempfile.TemporaryDirectory(prefix="wf-roster-cli-") as temp:
            root = Path(temp)
            env = self.isolated_environment(root)
            live = self.run_app([
                "--live", "--chat-roster-test", "--data-dir", str(root / "live-profile")
            ], env)
            self.assertEqual(live.returncode, 2, live.stdout + live.stderr)
            self.assertIn("Capture/test flags are demo-only", live.stdout + live.stderr)

            sentinel_path = root / "sentinel.sock"
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sentinel:
                sentinel.bind(str(sentinel_path))
                os.chmod(sentinel_path, 0o600)
                sentinel.listen(1)
                rejected = self.run_app([
                    "--demo", "--chat-roster-test", "--socket", str(sentinel_path),
                    "--data-dir", str(root / "demo-profile"),
                ], env)
                output = rejected.stdout + rejected.stderr
                self.assertEqual(rejected.returncode, 2, output)
                self.assertIn("chat-roster-test requires --demo and rejects --socket", output)
                sentinel.settimeout(0.15)
                with self.assertRaises(socket.timeout, msg="test flag connected to caller socket"):
                    connection, _ = sentinel.accept()
                    connection.close()


if __name__ == "__main__":
    unittest.main(verbosity=2)
