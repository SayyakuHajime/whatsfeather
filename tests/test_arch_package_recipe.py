from __future__ import annotations

import os
import re
import shlex
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
PKGBUILD = ROOT / "PKGBUILD"


class ArchPackageRecipeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.recipe = PKGBUILD.read_text(encoding="utf-8") if PKGBUILD.is_file() else ""

    def test_recipe_exists(self) -> None:
        self.assertTrue(PKGBUILD.is_file(), "the Arch package recipe is missing")

    def _run_package_with_stub_cmake(
        self, work: Path, pkgdir: Path, cmake_exit_code: int = 0
    ) -> subprocess.CompletedProcess[str]:
        commit_match = re.search(r"^_source_commit='([0-9a-f]{40})'$", self.recipe, re.MULTILINE)
        self.assertIsNotNone(commit_match)
        assert commit_match is not None

        fake_bin = work / "bin"
        fake_bin.mkdir()
        cmake_stub = fake_bin / "cmake"
        cmake_stub.write_text(
            '#!/bin/sh\nexit "${STUB_CMAKE_EXIT:-0}"\n', encoding="utf-8"
        )
        cmake_stub.chmod(0o755)

        srcdir = work / "src"
        (srcdir / f"whatsfeather-{commit_match.group(1)}").mkdir(parents=True)
        harness = "\n".join(
            (
                # makepkg's error() only prints; it does not abort or return failure.
                "error() { printf '==> ERROR: %s\\n' \"$*\" >&2; }",
                f"source {shlex.quote(str(PKGBUILD))}",
                f"srcdir={shlex.quote(str(srcdir))}",
                f"pkgdir={shlex.quote(str(pkgdir))}",
                "package",
            )
        )
        environment = os.environ.copy()
        environment["PATH"] = f"{fake_bin}{os.pathsep}{environment['PATH']}"
        environment["STUB_CMAKE_EXIT"] = str(cmake_exit_code)
        return subprocess.run(
            ["bash", "-c", harness],
            cwd=ROOT,
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )

    def test_package_fails_when_required_file_is_missing(self) -> None:
        with tempfile.TemporaryDirectory(
            prefix="wf-arch-package-test-", dir=os.environ.get("TMPDIR")
        ) as temporary_directory:
            work = Path(temporary_directory)
            pkgdir = work / "pkg"
            pkgdir.mkdir()
            result = self._run_package_with_stub_cmake(work, pkgdir)
        self.assertNotEqual(
            result.returncode,
            0,
            "package() must fail when CMake omits a required installed file",
        )

    def test_package_fails_when_required_icon_is_missing(self) -> None:
        with tempfile.TemporaryDirectory(
            prefix="wf-arch-package-test-", dir=os.environ.get("TMPDIR")
        ) as temporary_directory:
            work = Path(temporary_directory)
            pkgdir = work / "pkg"
            required = (
                "usr/bin/whatsfeather",
                "usr/bin/whatsfeatherd",
                "usr/share/applications/whatsfeather.desktop",
                "usr/share/licenses/WhatsFeather/LICENSE",
                "usr/share/licenses/WhatsFeather/ASSET-LICENSES.md",
                "usr/share/licenses/WhatsFeather/THIRD-PARTY-NOTICES.md",
            )
            for relative_path in required:
                path = pkgdir / relative_path
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("present\n", encoding="utf-8")
            for size in (24, 32, 48, 64, 128, 256, 512):
                path = pkgdir / f"usr/share/icons/hicolor/{size}x{size}/apps/whatsfeather.png"
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b"present")

            result = self._run_package_with_stub_cmake(work, pkgdir)

        self.assertNotEqual(
            result.returncode,
            0,
            "package() must fail when CMake omits a required application icon",
        )

    def test_package_fails_when_cmake_install_fails(self) -> None:
        with tempfile.TemporaryDirectory(
            prefix="wf-arch-package-test-", dir=os.environ.get("TMPDIR")
        ) as temporary_directory:
            work = Path(temporary_directory)
            pkgdir = work / "pkg"
            required = (
                "usr/bin/whatsfeather",
                "usr/bin/whatsfeatherd",
                "usr/share/applications/whatsfeather.desktop",
                "usr/share/licenses/WhatsFeather/LICENSE",
                "usr/share/licenses/WhatsFeather/ASSET-LICENSES.md",
                "usr/share/licenses/WhatsFeather/THIRD-PARTY-NOTICES.md",
            )
            for relative_path in required:
                path = pkgdir / relative_path
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("present\n", encoding="utf-8")
            for size in (16, 24, 32, 48, 64, 128, 256, 512):
                path = pkgdir / f"usr/share/icons/hicolor/{size}x{size}/apps/whatsfeather.png"
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b"present")

            result = self._run_package_with_stub_cmake(
                work, pkgdir, cmake_exit_code=1
            )

        self.assertNotEqual(
            result.returncode,
            0,
            "package() must fail if CMake install exits unsuccessfully",
        )

    def test_package_recipe_contract_is_wired_into_canonical_verifier(self) -> None:
        verify_script = ROOT / "scripts" / "verify.sh"
        contents = verify_script.read_text(encoding="utf-8")
        self.assertIn('python3 "$ROOT/tests/test_arch_package_recipe.py"', contents)

    @unittest.skipUnless(PKGBUILD.is_file(), "recipe does not exist yet")
    def test_recipe_is_valid_shell_and_makepkg_metadata(self) -> None:
        syntax = subprocess.run(["bash", "-n", str(PKGBUILD)], cwd=ROOT,
                                text=True, capture_output=True, check=False)
        self.assertEqual(syntax.returncode, 0, syntax.stderr)
        if shutil.which("makepkg"):
            srcinfo = subprocess.run(["makepkg", "--printsrcinfo", "-p", str(PKGBUILD)],
                                     cwd=ROOT, text=True, capture_output=True, check=False)
            self.assertEqual(srcinfo.returncode, 0, srcinfo.stderr)
            metadata = {line.strip() for line in srcinfo.stdout.splitlines()}
            expected = {
                "pkgbase = whatsfeather-prealpha",
                "pkgname = whatsfeather-prealpha",
                "arch = x86_64",
                "license = GPL-3.0-only",
                "depends = ffmpeg>=2:9.0.3",
                "depends = qt6-base>=6.12.0",
                "depends = qt6-declarative>=6.12.0",
                "depends = qt6-multimedia>=6.12.0",
                "depends = qt6-svg>=6.12.0",
                "makedepends = go>=2:1.27.3",
            }
            self.assertTrue(expected.issubset(metadata), f"missing SRCINFO metadata: {expected - metadata}")

    @unittest.skipUnless(PKGBUILD.is_file(), "recipe does not exist yet")
    def test_source_is_pinned_with_sha256_checksum(self) -> None:
        commit_match = re.search(r"^_source_commit='([0-9a-f]{40})'$", self.recipe, re.MULTILINE)
        checksum_match = re.search(r"^sha256sums=\('([0-9a-f]{64})'\)$", self.recipe, re.MULTILINE)
        self.assertIsNotNone(commit_match, "source must be pinned to a full immutable commit")
        self.assertIsNotNone(checksum_match, "source archive must have a SHA-256 checksum")
        assert commit_match is not None
        version_match = re.search(r"^pkgver=([A-Za-z0-9.+_]+)$", self.recipe, re.MULTILINE)
        self.assertIsNotNone(version_match)
        assert version_match is not None
        self.assertTrue(version_match.group(1).endswith("g" + commit_match.group(1)[:7]))
        self.assertIn("${_source_commit}.tar.gz", self.recipe)
        self.assertNotIn("sha256sums=('SKIP')", self.recipe)

    @unittest.skipUnless(PKGBUILD.is_file(), "recipe does not exist yet")
    def test_arch_x86_64_expected_dependency_metadata_tokens_are_present(self) -> None:
        self.assertRegex(self.recipe, r"(?m)^arch=\('x86_64'\)$")
        self.assertIn("license=('GPL-3.0-only')", self.recipe)
        self.assertIn("qt6-base", self.recipe)
        self.assertIn("qt6-declarative", self.recipe)
        self.assertIn("qt6-svg", self.recipe)
        self.assertIn("qt6-multimedia", self.recipe)
        self.assertIn("'ffmpeg>=2:9.0.3'", self.recipe)
        self.assertIn("'go>=2:1.27.3'", self.recipe)
        self.assertIn("usr/share/licenses/WhatsFeather/THIRD-PARTY-NOTICES.md", self.recipe)
        self.assertIn("GOTOOLCHAIN=local", self.recipe)
        self.assertIn('DESTDIR="$pkgdir" cmake --install', self.recipe)
        self.assertIn("./cmd/whatsfeatherd", self.recipe)
        self.assertIn("not a production release", self.recipe)

    @unittest.skipUnless(PKGBUILD.is_file(), "recipe does not exist yet")
    def test_package_build_uses_vendored_go_dependencies(self) -> None:
        self.assertIn("go build -mod=vendor", self.recipe)
        self.assertIn('tar -xzf "$source_dir/backend/vendor.tar.gz" -C "$source_dir/backend"',
                      self.recipe)
        archive_path = ROOT / "backend/vendor.tar.gz"
        self.assertTrue(archive_path.is_file(), "offline package builds require the pinned vendor archive")
        with tarfile.open(archive_path, "r:gz") as archive:
            names = set(archive.getnames())
            self.assertIn("vendor/modules.txt", names)

    @unittest.skipUnless(PKGBUILD.is_file(), "recipe does not exist yet")
    def test_pinned_source_snapshot_contains_all_required_build_inputs(self) -> None:
        match = re.search(r"^_source_commit='([0-9a-f]{40})'$", self.recipe, re.MULTILINE)
        self.assertIsNotNone(match, "source must be pinned to a full immutable commit")
        assert match is not None

        commit = match.group(1)
        commit_object = subprocess.run(
            ["git", "cat-file", "-e", f"{commit}^{{commit}}"],
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )
        if commit_object.returncode != 0:
            shallow = subprocess.run(
                ["git", "rev-parse", "--is-shallow-repository"],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
            )
            if shallow.returncode == 0 and shallow.stdout.strip() == "true":
                self.skipTest("shallow checkout does not include the pinned commit object")
        self.assertEqual(commit_object.returncode, 0, "pinned source commit object is unavailable")

        for path in ("backend/vendor.tar.gz", "LICENSES/THIRD-PARTY-NOTICES.md"):
            with self.subTest(path=path):
                result = subprocess.run(
                    ["git", "cat-file", "-e", f"{commit}:{path}"],
                    cwd=ROOT,
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(
                    result.returncode,
                    0,
                    f"the pinned source snapshot must contain {path} for an offline package build",
                )

    def test_expanded_vendor_cache_is_ignored_but_archive_is_not(self) -> None:
        expanded = subprocess.run(
            ["git", "check-ignore", "--quiet", "--", "backend/vendor/"],
            cwd=ROOT,
            check=False,
        )
        archive = subprocess.run(
            ["git", "check-ignore", "--quiet", "--", "backend/vendor.tar.gz"],
            cwd=ROOT,
            check=False,
        )
        self.assertEqual(expanded.returncode, 0, "expanded build cache should stay out of Git")
        self.assertEqual(archive.returncode, 1, "the pinned source archive must remain trackable")


if __name__ == "__main__":
    unittest.main(verbosity=2)
