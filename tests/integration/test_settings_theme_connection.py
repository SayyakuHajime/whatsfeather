"""Verify demo Settings theme persistence and live-action visibility in the Qt app."""
import os
from pathlib import Path
import struct
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
APP = ROOT / "build/bin/whatsfeather"


class SettingsThemeConnection(unittest.TestCase):
    def test_demo_theme_persists_across_restart_without_live_actions(self):
        with tempfile.TemporaryDirectory(prefix="wf-settings-") as temp:
            root = Path(temp)
            for name in ("runtime", "data", "config", "cache"):
                (root / name).mkdir(mode=0o700)
            evidence_root = Path(os.environ.get("WHATSFEATHER_QA_ROOT", root / "qa"))
            capture = evidence_root / "settings-theme-connection"
            capture.mkdir(parents=True, exist_ok=True)
            profile = root / "profile"
            env = dict(
                os.environ,
                QT_QPA_PLATFORM="offscreen",
                QT_QUICK_BACKEND="software",
                QT_FORCE_STDERR_LOGGING="1",
                QT_SCALE_FACTOR="1",
                XDG_RUNTIME_DIR=str(root / "runtime"),
                XDG_DATA_HOME=str(root / "data"),
                XDG_CONFIG_HOME=str(root / "config"),
                XDG_CACHE_HOME=str(root / "cache"),
                WHATSFEATHER_QA_CAPTURE_DIR=str(capture),
            )

            first = subprocess.run(
                [str(APP), "--demo", "--settings-test", "--theme", "dark",
                 "--data-dir", str(profile)],
                env=dict(env, WHATSFEATHER_SETTINGS_TEST_EXPECT_THEME="dark",
                         WHATSFEATHER_SETTINGS_TEST_SET_THEME="light"),
                capture_output=True, text=True, timeout=30,
            )
            first_output = first.stdout + first.stderr
            self.assertEqual(first.returncode, 0, first_output)
            self.assertIn("Native settings controls PASS", first_output)
            self.assertIn("Native settings theme saved PASS: light", first_output)

            restarted = subprocess.run(
                [str(APP), "--demo", "--settings-test", "--data-dir", str(profile)],
                env=dict(env, WHATSFEATHER_SETTINGS_TEST_EXPECT_THEME="light",
                         WHATSFEATHER_SETTINGS_TEST_SET_THEME=""),
                capture_output=True, text=True, timeout=30,
            )
            restarted_output = restarted.stdout + restarted.stderr
            self.assertEqual(restarted.returncode, 0, restarted_output)
            self.assertIn("Native settings controls PASS", restarted_output)
            self.assertIn("Native settings preference restored PASS: light", restarted_output)
            self.assertNotIn("Connecting", first_output + restarted_output)
            self.assertNotIn("QR", first_output + restarted_output)

            for name in ("settings-dark.png", "settings-light.png",
                         "settings-restored-light.png"):
                image = capture / name
                self.assertTrue(image.is_file(), f"missing rendered settings capture: {image}")
                self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n")
                self.assertEqual(struct.unpack(">II", image.read_bytes()[16:24]), (1180, 780))
    def test_settings_test_flag_rejects_live_mode_and_external_socket(self):
        import socket
        with tempfile.TemporaryDirectory(prefix="wf-settings-guard-") as temp:
            root = Path(temp)
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen",
                       QT_FORCE_STDERR_LOGGING="1")
            live = subprocess.run(
                [str(APP), "--live", "--settings-test"], env=env,
                capture_output=True, text=True, timeout=10,
            )
            live_output = live.stdout + live.stderr
            self.assertEqual(live.returncode, 2, live_output)
            self.assertIn("Capture/test flags are demo-only", live_output)

            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            listener.bind(str(root / "sentinel.sock"))
            listener.listen(1)
            listener.settimeout(0.2)
            try:
                external = subprocess.run(
                    [str(APP), "--demo", "--settings-test", "--socket",
                     str(root / "sentinel.sock")], env=env,
                    capture_output=True, text=True, timeout=10,
                )
                output = external.stdout + external.stderr
                self.assertEqual(external.returncode, 2, output)
                self.assertIn("settings-test requires --demo and rejects --socket", output)
                with self.assertRaises(socket.timeout):
                    connection, _ = listener.accept()
                    connection.close()
                    self.fail("settings-test connected to a caller-supplied socket")
            finally:
                listener.close()


if __name__ == "__main__":
    unittest.main()
