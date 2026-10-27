"""Exercise offline legal-document access in the actual Qt application."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
BINARY = ROOT / "build/bin/whatsfeather"
DOCUMENTS = {
    "project-license": ROOT / "LICENSE",
    "asset-licenses": ROOT / "LICENSES/ASSET-LICENSES.md",
    "third-party-notices": ROOT / "LICENSES/THIRD-PARTY-NOTICES.md",
}
DOCUMENT_MARKER = re.compile(
    r"About legal document PASS: id=([a-z-]+) bytes=(\d+) sha256=([0-9a-f]{64})"
)


def isolated_environment(root: Path, capture_dir: Path) -> dict[str, str]:
    for name in ("runtime", "data", "config", "cache"):
        (root / name).mkdir(mode=0o700, parents=True, exist_ok=True)
    capture_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    env = dict(os.environ)
    env.update(
        QT_QPA_PLATFORM="offscreen",
        QT_QUICK_BACKEND="software",
        QT_FORCE_STDERR_LOGGING="1",
        QT_SCALE_FACTOR="1",
        HOME=str(root),
        XDG_RUNTIME_DIR=str(root / "runtime"),
        XDG_DATA_HOME=str(root / "data"),
        XDG_CONFIG_HOME=str(root / "config"),
        XDG_CACHE_HOME=str(root / "cache"),
        WHATSFEATHER_QA_CAPTURE_DIR=str(capture_dir),
    )
    return env


def assert_document_hashes(test: unittest.TestCase, output: str) -> None:
    records = DOCUMENT_MARKER.findall(output)
    test.assertEqual([record[0] for record in records], list(DOCUMENTS), output)
    for document_id, raw_size, digest in records:
        canonical = DOCUMENTS[document_id].read_bytes()
        test.assertEqual(int(raw_size), len(canonical), document_id)
        test.assertEqual(digest, hashlib.sha256(canonical).hexdigest(), document_id)


class AboutLegalAccess(unittest.TestCase):
    def test_about_exposes_plaintext_offline_document_actions(self):
        qml = (ROOT / "desktop/qml/Main.qml").read_text(encoding="utf-8")
        main = (ROOT / "desktop/src/main.cpp").read_text(encoding="utf-8")
        for object_name in (
            "projectLicenseButton",
            "assetLicensesButton",
            "thirdPartyNoticesButton",
            "legalDocumentDialog",
            "legalDocumentText",
            "legalDocumentScroll",
        ):
            with self.subTest(object_name=object_name):
                self.assertTrue(
                    f'objectName: "{object_name}"' in qml,
                    f"missing QML control: {object_name}",
                )
        self.assertTrue(
            "textFormat: TextEdit.PlainText" in qml,
            "legal text must use explicit PlainText rendering",
        )
        self.assertTrue("selectByMouse: true" in qml, "legal text must be selectable")
        self.assertTrue("readOnly: true" in qml, "legal text must remain read-only")
        self.assertTrue(
            "legalDocuments" in main,
            "native legal-document provider is not bound to QML",
        )
        self.assertTrue("about-legal-test" in main, "native About regression mode is missing")

    def test_actual_native_ui_and_relocated_install_render_canonical_documents(self):
        self.assertTrue(BINARY.is_file(), "build the native application before this integration test")
        with tempfile.TemporaryDirectory(prefix="wf-about-legal-") as temp:
            root = Path(temp)
            capture_root = Path(os.environ.get("WHATSFEATHER_QA_ROOT", root / "captures"))
            for theme in ("dark", "light"):
                capture_dir = capture_root / f"about-legal-{theme}"
                env = isolated_environment(root / f"xdg-{theme}", capture_dir)
                result = subprocess.run(
                    [
                        str(BINARY),
                        "--demo",
                        "--data-dir",
                        str(root / f"profile-{theme}"),
                        "--theme",
                        theme,
                        "--about-legal-test",
                    ],
                    cwd=root,
                    env=env,
                    capture_output=True,
                    text=True,
                    timeout=45,
                )
                output = result.stdout + result.stderr
                (capture_root / f"about-legal-{theme}.log").write_text(output)
                self.assertEqual(result.returncode, 0, output)
                self.assertIn("Native About legal access PASS", output)
                self.assertNotIn("qrc:", output)
                self.assertNotIn("QQmlApplicationEngine failed", output)
                assert_document_hashes(self, output)
                for state in ("about-wide", "about-compact", "legal-notices-wide", "legal-notices-compact"):
                    image = capture_dir / f"{state}-{theme}.png"
                    self.assertTrue(image.is_file(), str(image))
                    self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n", str(image))

            prefix = root / "relocated-install"
            installed = subprocess.run(
                ["cmake", "--install", str(ROOT / "build"), "--prefix", str(prefix)],
                cwd=root,
                env=isolated_environment(root / "install-xdg", root / "install-captures"),
                capture_output=True,
                text=True,
                timeout=45,
            )
            self.assertEqual(installed.returncode, 0, installed.stdout + installed.stderr)
            (capture_root / "relocated-install.log").write_text(
                installed.stdout + installed.stderr
            )
            legal_dir = prefix / "share/licenses/WhatsFeather"
            for document in DOCUMENTS.values():
                packaged = legal_dir / document.name
                self.assertEqual(packaged.read_bytes(), document.read_bytes(), str(packaged))

            capture_dir = capture_root / "about-legal-relocated-installed"
            env = isolated_environment(root / "relocated-xdg", capture_dir)
            result = subprocess.run(
                [
                    str(prefix / "bin/whatsfeather"),
                    "--demo",
                    "--data-dir",
                    str(root / "relocated-profile"),
                    "--theme",
                    "dark",
                    "--about-legal-test",
                ],
                cwd=root,
                env=env,
                capture_output=True,
                text=True,
                timeout=45,
            )
            output = result.stdout + result.stderr
            (capture_root / "relocated-installed-ui.log").write_text(output)
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native About legal access PASS", output)
            assert_document_hashes(self, output)

    def test_about_legal_test_rejects_caller_supplied_socket(self):
        self.assertTrue(BINARY.is_file(), "build the native application before this integration test")
        with tempfile.TemporaryDirectory(prefix="wf-about-socket-") as temp:
            root = Path(temp)
            env = isolated_environment(root / "xdg", root / "captures")
            sentinel_path = root / "sentinel.sock"
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sentinel:
                sentinel.bind(str(sentinel_path))
                sentinel.listen(1)
                sentinel.settimeout(0.15)
                result = subprocess.run(
                    [
                        str(BINARY),
                        "--demo",
                        "--data-dir",
                        str(root / "profile"),
                        "--socket",
                        str(sentinel_path),
                        "--about-legal-test",
                    ],
                    cwd=root,
                    env=env,
                    capture_output=True,
                    text=True,
                    timeout=10,
                )
                output = result.stdout + result.stderr
                self.assertEqual(result.returncode, 2, output)
                self.assertIn("about-legal-test requires --demo and rejects --socket", output)
                with self.assertRaises(TimeoutError):
                    sentinel.accept()


if __name__ == "__main__":
    unittest.main()
