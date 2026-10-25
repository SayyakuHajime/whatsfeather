from __future__ import annotations

import importlib.util
import io
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
GENERATOR = ROOT / "scripts" / "generate-third-party-notices.py"
NOTICE = ROOT / "LICENSES" / "THIRD-PARTY-NOTICES.md"


def _generator_module():
    spec = importlib.util.spec_from_file_location("third_party_notices", GENERATOR)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load third-party notice generator")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _write_archive(path: Path, entries: list[tuple[str, bytes]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(path, "w:gz") as archive:
        for name, content in entries:
            info = tarfile.TarInfo(name)
            info.size = len(content)
            archive.addfile(info, io.BytesIO(content))


class ThirdPartyNoticeTests(unittest.TestCase):
    def test_checked_in_notices_match_all_vendored_module_license_texts(self) -> None:
        self.assertTrue(GENERATOR.is_file(), "third-party notice generator is missing")
        self.assertTrue(NOTICE.is_file(), "generated third-party notices are missing")
        module = _generator_module()
        self.assertEqual(NOTICE.read_text(encoding="utf-8"), module.render_notices(ROOT))
        with tarfile.open(ROOT / "backend" / "vendor.tar.gz", "r:gz") as archive:
            manifest_file = archive.extractfile("vendor/modules.txt")
            self.assertIsNotNone(manifest_file)
            assert manifest_file is not None
            manifest = manifest_file.read().decode("utf-8")
        module_entries = [
            line for line in manifest.splitlines()
            if line.startswith("# ") and not line.startswith("## ")
        ]
        self.assertEqual(len(module_entries), 29)
        rendered = NOTICE.read_text(encoding="utf-8")
        for entry in module_entries:
            module_path, version = entry[2:].split()[:2]
            self.assertIn(
                f"`{module_path}` `{version}`",
                rendered,
                f"missing vendored module notice for {module_path}@{version}",
            )
        self.assertIn("## System-provided runtime packages", rendered)
        self.assertIn("LicenseRef-Qt-Commercial", rendered)
        self.assertTrue("not a direct PKGBUILD dependency" in rendered)

    def test_render_uses_the_archive_manifest_without_an_extracted_vendor_tree(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            backend = root / "backend"
            backend.mkdir()
            archive_path = backend / "vendor.tar.gz"
            with tarfile.open(archive_path, "w:gz") as archive:
                manifest = b"# example.test/module v1.2.3\n"
                manifest_info = tarfile.TarInfo("vendor/modules.txt")
                manifest_info.size = len(manifest)
                archive.addfile(manifest_info, io.BytesIO(manifest))
                license_text = b"Example license from the pinned vendor archive.\n"
                license_info = tarfile.TarInfo("vendor/example.test/module/LICENSE")
                license_info.size = len(license_text)
                archive.addfile(license_info, io.BytesIO(license_text))
            rendered = module.render_notices(root)
        self.assertIn("`example.test/module` `v1.2.3`", rendered)
        self.assertIn("Example license from the pinned vendor archive.", rendered)

    def test_render_rejects_vendored_module_without_root_license(self) -> None:
        self.assertTrue(GENERATOR.is_file(), "third-party notice generator is missing")
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            backend = root / "backend"
            backend.mkdir()
            archive_path = root / "backend" / "vendor.tar.gz"
            with tarfile.open(archive_path, "w:gz") as archive:
                manifest = b"# example.test/without-license v1.2.3\n"
                manifest_info = tarfile.TarInfo("vendor/modules.txt")
                manifest_info.size = len(manifest)
                archive.addfile(manifest_info, io.BytesIO(manifest))
                source = b"package withoutlicense\n"
                info = tarfile.TarInfo("vendor/example.test/without-license/source.go")
                info.size = len(source)
                archive.addfile(info, io.BytesIO(source))
            with self.assertRaisesRegex(ValueError, "no root license or notice"):
                module.render_notices(root)

    def test_render_reproduces_root_license_and_copyright_bytes_from_archive(self) -> None:
        self.assertTrue(GENERATOR.is_file(), "third-party notice generator is missing")
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            backend = root / "backend"
            backend.mkdir()
            license_text = b"Example license text\nwith exact bytes.\n"
            copyright_text = b"Copyright 2026 Example Project.\n"
            archive_path = root / "backend" / "vendor.tar.gz"
            with tarfile.open(archive_path, "w:gz") as archive:
                manifest = b"# example.test/module v2.4.6\n"
                manifest_info = tarfile.TarInfo("vendor/modules.txt")
                manifest_info.size = len(manifest)
                archive.addfile(manifest_info, io.BytesIO(manifest))
                info = tarfile.TarInfo("vendor/example.test/module/LICENSE")
                info.size = len(license_text)
                archive.addfile(info, io.BytesIO(license_text))
                copyright_info = tarfile.TarInfo("vendor/example.test/module/COPYRIGHT")
                copyright_info.size = len(copyright_text)
                archive.addfile(copyright_info, io.BytesIO(copyright_text))
                nested_text = b"Not a module-root license.\n"
                nested = tarfile.TarInfo("vendor/example.test/module/subdir/LICENSE")
                nested.size = len(nested_text)
                archive.addfile(nested, io.BytesIO(nested_text))
            rendered = module.render_notices(root)
        self.assertIn("`example.test/module` `v2.4.6`", rendered)
        self.assertIn(license_text.decode("utf-8"), rendered)
        self.assertIn(copyright_text.decode("utf-8"), rendered)
        self.assertNotIn(nested_text.decode("utf-8"), rendered)

    def test_render_rejects_archive_above_compressed_size_limit(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_archive(root / "backend/vendor.tar.gz", [
                ("vendor/modules.txt", b"# example.test/module v1.2.3\n"),
                ("vendor/example.test/module/LICENSE", b"license\n"),
            ])
            with patch.object(module, "MAX_ARCHIVE_COMPRESSED_BYTES", 1, create=True):
                with self.assertRaisesRegex(ValueError, "compressed size limit"):
                    module.render_notices(root)

    def test_render_rejects_archive_above_member_count_limit(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_archive(root / "backend/vendor.tar.gz", [
                ("vendor/modules.txt", b"# example.test/module v1.2.3\n"),
                ("vendor/example.test/module/LICENSE", b"license\n"),
            ])
            with patch.object(module, "MAX_ARCHIVE_MEMBERS", 1, create=True):
                with self.assertRaisesRegex(ValueError, "member-count limit"):
                    module.render_notices(root)

    def test_render_rejects_archive_above_single_member_size_limit(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_archive(root / "backend/vendor.tar.gz", [
                ("vendor/modules.txt", b"# example.test/module v1.2.3\n"),
                ("vendor/example.test/module/LICENSE", b"license\n"),
            ])
            with patch.object(module, "MAX_ARCHIVE_MEMBER_BYTES", 1, create=True):
                with self.assertRaisesRegex(ValueError, "member-size limit"):
                    module.render_notices(root)

    def test_render_rejects_archive_above_total_declared_member_size_limit(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_archive(root / "backend/vendor.tar.gz", [
                ("vendor/modules.txt", b"# example.test/module v1.2.3\n"),
                ("vendor/example.test/module/LICENSE", b"license\n"),
            ])
            with patch.object(module, "MAX_ARCHIVE_TOTAL_MEMBER_BYTES", 1):
                with self.assertRaisesRegex(ValueError, "total declared member-size limit"):
                    module.render_notices(root)

    def test_render_rejects_archive_with_oversized_pax_metadata(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive_path = root / "backend/vendor.tar.gz"
            archive_path.parent.mkdir(parents=True)
            with tarfile.open(archive_path, "w:gz", format=tarfile.PAX_FORMAT) as archive:
                manifest = b"# example.test/module v1.2.3\n"
                manifest_info = tarfile.TarInfo("vendor/modules.txt")
                manifest_info.size = len(manifest)
                archive.addfile(manifest_info, io.BytesIO(manifest))
                license_text = b"license\n"
                license_info = tarfile.TarInfo("vendor/example.test/module/LICENSE")
                license_info.size = len(license_text)
                license_info.pax_headers = {"comment": "x" * 4096}
                archive.addfile(license_info, io.BytesIO(license_text))
            with patch.object(module, "MAX_ARCHIVE_EXPANDED_BYTES", 1024):
                with self.assertRaisesRegex(ValueError, "expanded byte limit"):
                    module.render_notices(root)


if __name__ == "__main__":
    unittest.main(verbosity=2)
