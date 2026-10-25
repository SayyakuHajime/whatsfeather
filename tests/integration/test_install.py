#!/usr/bin/env python3
"""Exercise a relocated CMake install using only a new synthetic demo profile.

Host Qt libraries/plugins are still required. This is not a portable-package,
real-version upgrade, signed-release, cross-distro, or live-account test.
"""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


def run(command, **kwargs):
    return subprocess.run(command, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT, timeout=35, check=False, **kwargs)


class RelocatedInstall(unittest.TestCase):
    def test_install_launch_and_reinstall_preserve_synthetic_profile(self):
        self.assertTrue((ROOT / "build/bin/whatsfeather").is_file())
        self.assertTrue((ROOT / "build/bin/whatsfeatherd").is_file())
        with tempfile.TemporaryDirectory(prefix="wfi-") as temporary:
            base = Path(temporary)
            # Keep AF_UNIX paths short even when TMPDIR is a long workspace.
            with tempfile.TemporaryDirectory(prefix="wfir-") as runtime_dir:
                self.assertLess(len(os.fsencode(runtime_dir + "/whatsfeather/ui-12345678.sock")), 104)
                prefix = base / "install"
                def install():
                    result = run(["cmake", "--install", str(ROOT / "build"),
                                  "--prefix", str(prefix)])
                    self.assertEqual(result.returncode, 0, result.stdout)
                install()
                installed_license = prefix / "share/licenses/WhatsFeather/LICENSE"
                self.assertTrue(installed_license.is_file())
                self.assertEqual(installed_license.read_bytes(), (ROOT / "LICENSE").read_bytes())
                installed_asset_manifest = prefix / "share/licenses/WhatsFeather/ASSET-LICENSES.md"
                self.assertTrue(installed_asset_manifest.is_file())
                self.assertEqual(installed_asset_manifest.read_bytes(),
                                 (ROOT / "LICENSES/ASSET-LICENSES.md").read_bytes())
                installed_third_party_notices = prefix / "share/licenses/WhatsFeather/THIRD-PARTY-NOTICES.md"
                self.assertTrue(installed_third_party_notices.is_file())
                self.assertEqual(installed_third_party_notices.read_bytes(),
                                 (ROOT / "LICENSES/THIRD-PARTY-NOTICES.md").read_bytes())
                for name in ("whatsfeather", "whatsfeatherd"):
                    installed = prefix / "bin" / name
                    self.assertTrue(installed.is_file())
                    self.assertTrue(os.access(installed, os.X_OK))
                    self.assertEqual(installed.read_bytes()[:4], b"\x7fELF")
                    # CMake rewrites GUI RUNPATH to $ORIGIN at install time;
                    # byte identity with the build-tree executable is invalid.
                    if name == "whatsfeatherd":
                        self.assertEqual(hashlib.sha256(installed.read_bytes()).digest(),
                                         hashlib.sha256((ROOT / "build/bin" / name).read_bytes()).digest())
                entry = prefix / "share/applications/whatsfeather.desktop"
                self.assertTrue(entry.is_file())
                self.assertIn("Exec=whatsfeather\n", entry.read_text())
                if shutil.which("desktop-file-validate"):
                    valid = run(["desktop-file-validate", str(entry)])
                    self.assertEqual(valid.returncode, 0, valid.stdout)
                for size in (16, 24, 32, 48, 64, 128, 256, 512):
                    self.assertTrue((prefix / f"share/icons/hicolor/{size}x{size}/apps/whatsfeather.png").is_file())
                self.assertFalse(any(p.name in (".hermes", ".env") for p in prefix.rglob("*")))
                env = os.environ.copy()
                # Do not allow the test to launch a caller-selected real daemon.
                for name in list(env):
                    if name.startswith("WHATSFEATHER_"):
                        env.pop(name)
                for name in ("home", "config", "data", "cache"):
                    (base / name).mkdir(mode=0o700)
                env.update({"HOME": str(base / "home"),
                            "XDG_RUNTIME_DIR": runtime_dir,
                            "XDG_CONFIG_HOME": str(base / "config"),
                            "XDG_DATA_HOME": str(base / "data"),
                            "XDG_CACHE_HOME": str(base / "cache"),
                            "QT_QPA_PLATFORM": "offscreen",
                            "QT_QUICK_BACKEND": "software",
                            "QT_FORCE_STDERR_LOGGING": "1"})
                os.chmod(runtime_dir, 0o700)
                profile = base / "demo-profile"
                command = [str(prefix / "bin/whatsfeather"), "--demo", "--ui-test",
                           "--data-dir", str(profile)]
                result = run(command, env=env, cwd=base)
                self.assertEqual(result.returncode, 0, result.stdout)
                self.assertIn("Native UI smoke PASS:", result.stdout)
                self.assertNotIn("QQmlApplicationEngine failed", result.stdout)
                databases = list(profile.rglob("*.db"))
                self.assertTrue(databases, "Installed daemon must create actual synthetic storage")
                before = {p.relative_to(profile): hashlib.sha256(p.read_bytes()).digest()
                          for p in databases}
                install()
                after = {p.relative_to(profile): hashlib.sha256(p.read_bytes()).digest()
                         for p in databases}
                self.assertEqual(before, after, "Reinstall must not touch user data")
                reopened = run(command, env=env, cwd=base)
                self.assertEqual(reopened.returncode, 0, reopened.stdout)
                self.assertIn("Native UI smoke PASS:", reopened.stdout)
                self.assertTrue(all((p.stat().st_mode & 0o777) == 0o600 for p in databases))


if __name__ == "__main__":
    unittest.main(verbosity=2)
