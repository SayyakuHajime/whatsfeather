from __future__ import annotations

import importlib.util
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "check-release.py"
SPEC = importlib.util.spec_from_file_location("check_release", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
release_module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release_module)


class ReleaseCheckTests(unittest.TestCase):
    def _make_repo(self, root: Path) -> None:
        (root / "backend").mkdir()
        (root / "scripts").mkdir()
        (root / "CMakeLists.txt").write_text(
            "cmake_minimum_required(VERSION 3.22)\n"
            "find_package(Qt6 6.5 REQUIRED COMPONENTS Core Gui Quick)\n",
            encoding="utf-8",
        )
        (root / "backend" / "go.mod").write_text(
            "module example.test/app\n\ngo 1.24.0\n\nrequire example.test/dep v1.0.0\n",
            encoding="utf-8",
        )
        (root / "backend" / "go.sum").write_text(
            "example.test/dep v1.0.0 h1:abcd\n", encoding="utf-8"
        )
        (root / ".gitignore").write_text(".env\nprofiles/\nignored.txt\n", encoding="utf-8")
        subprocess.run(["git", "init", "-q"], cwd=root, check=True)

    def test_scanner_finds_common_token_without_returning_value(self) -> None:
        token = "ghp_" + "A" * 36
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._make_repo(root)
            source = root / "scripts" / "sample.py"
            source.write_text("print('clean')\n", encoding="utf-8")
            go_source = root / "backend" / "sample.go"
            go_source.write_text(f"const credential = {token!r}\n", encoding="utf-8")
            (root / ".env").write_text(f"TOKEN={token}\n", encoding="utf-8")
            (root / ".envrc").write_text(f"credential = {token!r}\n", encoding="utf-8")
            (root / "profiles").mkdir()
            (root / "profiles" / "private.py").write_text(f"credential = {token!r}\n", encoding="utf-8")
            (root / "ignored.txt").write_text(f"credential = {token!r}\n", encoding="utf-8")
            subprocess.run(["git", "add", "-f", ".envrc", "ignored.txt"], cwd=root, check=True)
            paths = release_module.candidate_paths(root)
            self.assertEqual(
                {path.as_posix() for path in paths},
                {
                    ".gitignore", "CMakeLists.txt", "backend/go.mod", "backend/go.sum",
                    "backend/sample.go", "scripts/sample.py",
                },
            )
            count, findings = release_module.scan_repository(root)
            self.assertEqual(count, 6)
            self.assertEqual(findings, [("backend/sample.go", 1, "github-token")])
            rendered = "\n".join(str(item) for item in findings)
            self.assertNotIn(token, rendered)

    def test_generic_secret_assignment_reports_only_location_and_rule(self) -> None:
        secret = "QwErTy1234567890" + "-+=AbCdEfGhIjKlMn"
        findings = release_module.scan_text(f'api_key = "{secret}"\n')
        self.assertEqual(findings, [(1, "high-entropy-secret-assignment")])
        self.assertNotIn(secret, repr(findings))

    def test_release_mode_fails_missing_license_but_verify_mode_allows_it(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._make_repo(root)
            (root / "source.py").write_text("print('hello')\n", encoding="utf-8")
            asset_manifest = root / "LICENSES" / "ASSET-LICENSES.md"
            asset_manifest.parent.mkdir()
            asset_manifest.write_text("Assets are licensed under GPL-3.0-only.\n", encoding="utf-8")
            errors_verify, _, _ = release_module.evaluate(root, "verify")
            errors_release, _, _ = release_module.evaluate(root, "release")
            self.assertEqual(errors_verify, [])
            self.assertEqual(errors_release, [
                "release mode requires a non-empty root LICENSE; no license has been selected"
            ])
            (root / "LICENSE").write_text("License terms pending maintainer review.\n", encoding="utf-8")
            errors_release_present, _, _ = release_module.evaluate(root, "release")
            self.assertEqual(errors_release_present, [])

    def test_release_mode_requires_asset_license_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._make_repo(root)
            (root / "LICENSE").write_text("GPL-3.0-only\n", encoding="utf-8")
            errors, _, _ = release_module.evaluate(root, "release")
        self.assertEqual(errors, [
            "release mode requires a non-empty LICENSES/ASSET-LICENSES.md"
        ])

    def test_cli_returns_failure_without_echoing_secret(self) -> None:
        token = "ghp_" + "B" * 36
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._make_repo(root)
            (root / "leak.py").write_text(f"value = {token!r}\n", encoding="utf-8")
            result = subprocess.run(
                [sys.executable, str(SCRIPT), "--root", str(root), "--mode", "verify"],
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
        self.assertEqual(result.returncode, 1)
        self.assertIn("secret pattern github-token", result.stdout)
        self.assertNotIn(token, result.stdout + result.stderr)

    def test_verify_gate_is_repeatable_for_an_unchanged_tree(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._make_repo(root)
            (root / "source.py").write_text("print('clean')" + chr(10), encoding="utf-8")
            first = release_module.evaluate(root, "verify")
            second = release_module.evaluate(root, "verify")
        self.assertEqual(first, second)

    def test_unscannable_source_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._make_repo(root)
            (root / "broken.py").write_bytes(b"source" + bytes([0]) + b"with-nul")
            with self.assertRaisesRegex(OSError, "NUL byte"):
                release_module.scan_repository(root)

    def test_force_added_private_paths_fail_without_reading_their_contents(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._make_repo(root)
            (root / ".env").write_text("PRIVATE CONTENT MUST NOT BE READ\n")
            (root / "profiles").mkdir()
            (root / "profiles" / "history.db").write_bytes(b"private fixture")
            subprocess.run(["git", "add", "-f", ".env", "profiles/history.db"], cwd=root, check=True)
            errors, _, _ = release_module.evaluate(root, "verify")
            self.assertIn("forbidden publication path: .env", errors)
            self.assertIn("forbidden publication path: profiles/history.db", errors)
            self.assertNotIn("PRIVATE CONTENT", repr(errors))

    def test_private_key_marker_is_detected(self) -> None:
        marker = "-----BEGIN OPENSSH " + "PRIVATE KEY-----"
        self.assertEqual(release_module.scan_text(marker + "\n"), [(1, "private-key-marker")])


if __name__ == "__main__":
    unittest.main()
