from __future__ import annotations

import importlib.util
import hashlib
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


def _install_go_toolchain_license(root: Path) -> None:
    destination = root / "LICENSES" / "supplemental" / "go-toolchain-LICENSE.txt"
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_bytes((ROOT / "LICENSES" / "supplemental" / "go-toolchain-LICENSE.txt").read_bytes())


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
        self.assertEqual(len(module_entries), 31)
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
            _install_go_toolchain_license(root)
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
            _install_go_toolchain_license(root)
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
        self.assertIn(nested_text.decode("utf-8"), rendered)
        self.assertIn("Source path: `vendor/example.test/module/subdir/LICENSE`", rendered)

    def test_render_attributes_nested_notice_files_to_the_longest_module_ancestor(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            entries = [
                ("vendor/modules.txt", b"# example.test/module v1.0.0\n# example.test/module/child v2.0.0\n"),
                ("vendor/example.test/module/LICENSE", b"parent license\n"),
                ("vendor/example.test/module/docs/NOTICE", b"parent nested notice\n"),
                ("vendor/example.test/module/child/LICENSE", b"child license\n"),
                ("vendor/example.test/module/child/docs/COPYING", b"child nested copying\n"),
            ]
            _write_archive(root / "backend/vendor.tar.gz", entries)

            documents = module._license_documents(root)

        attribution = {document.path: document.module for document in documents}
        self.assertEqual(attribution, {
            "vendor/example.test/module/LICENSE": "example.test/module",
            "vendor/example.test/module/docs/NOTICE": "example.test/module",
            "vendor/example.test/module/child/LICENSE": "example.test/module/child",
            "vendor/example.test/module/child/docs/COPYING": "example.test/module/child",
        })

    def test_render_rejects_noncanonical_or_traversing_archive_paths(self) -> None:
        module = _generator_module()
        unsafe_paths = (
            "../vendor/escape/LICENSE",
            "/vendor/example.test/module/absolute.txt",
            "vendor//example.test/module/repeated-slash.txt",
            "vendor/example.test/module/./dot.txt",
            "vendor/example.test/module/../../escape.txt",
            "vendor\\\\example.test\\\\module\\\\backslash.txt",
        )
        for unsafe_path in unsafe_paths:
            with self.subTest(path=unsafe_path), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                _write_archive(root / "backend/vendor.tar.gz", [
                    ("vendor/modules.txt", b"# example.test/module v1.2.3\n"),
                    ("vendor/example.test/module/LICENSE", b"license\n"),
                    (unsafe_path, b"untrusted\n"),
                ])
                with self.assertRaisesRegex(ValueError, "non-canonical vendored archive member path"):
                    module.render_notices(root)

    def test_render_rejects_unsafe_archive_entry_types_even_outside_notice_paths(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive_path = root / "backend/vendor.tar.gz"
            archive_path.parent.mkdir(parents=True)
            with tarfile.open(archive_path, "w:gz") as archive:
                for name, content in [
                    ("vendor/modules.txt", b"# example.test/module v1.2.3\n"),
                    ("vendor/example.test/module/LICENSE", b"license\n"),
                ]:
                    info = tarfile.TarInfo(name)
                    info.size = len(content)
                    archive.addfile(info, io.BytesIO(content))
                link = tarfile.TarInfo("vendor/example.test/module/source.go")
                link.type = tarfile.SYMTYPE
                link.linkname = "../../outside.go"
                archive.addfile(link)

            with self.assertRaisesRegex(ValueError, "unsupported vendored archive member type"):
                module.render_notices(root)

    def test_nested_notices_still_obey_per_file_and_aggregate_text_limits(self) -> None:
        module = _generator_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_archive(root / "backend/vendor.tar.gz", [
                ("vendor/modules.txt", b"# example.test/module v1.2.3\n"),
                ("vendor/example.test/module/LICENSE", b"L\n"),
                ("vendor/example.test/module/deep/NOTICE", b"12345"),
            ])
            with patch.object(module, "MAX_LICENSE_FILE_BYTES", 4):
                with self.assertRaisesRegex(ValueError, "license file exceeds the size limit"):
                    module.render_notices(root)

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_archive(root / "backend/vendor.tar.gz", [
                ("vendor/modules.txt", b"# example.test/module v1.2.3\n"),
                ("vendor/example.test/module/LICENSE", b"123456"),
                ("vendor/example.test/module/deep/NOTICE", b"abcdef"),
            ])
            with patch.object(module, "MAX_TOTAL_LICENSE_BYTES", 10):
                with self.assertRaisesRegex(ValueError, "vendored license texts exceed the total size limit"):
                    module.render_notices(root)

    def test_generated_notices_retain_reviewed_modernc_header_attribution_and_terms(self) -> None:
        module = _generator_module()
        rendered = module.render_notices(ROOT)
        self.assertIn("## Reviewed modernc generated-source header coverage", rendered)
        self.assertIn("vendor/modernc.org/libc/sys/types/types_linux_amd64.go", rendered)
        self.assertIn("1ada8b2599b13281b8fe191cfb24c02896ec3781b95a3b5cf76e1d74424eea2e", rendered)
        self.assertIn("vendor/modernc.org/libc/uuid/uuid/uuid_linux_amd64.go", rendered)
        self.assertIn("c677ffac9af69102fabc82443b8799b4071c7ead9f02237bb2f01af7782af428", rendered)
        self.assertIn("Copyright (C) 1991-2020 Free Software Foundation, Inc.", rendered)
        self.assertIn("Copyright (C) 1996, 1997, 1998 Theodore Ts'o.", rendered)
        self.assertIn("This file is part of GCC.", rendered)
        self.assertIn("LGPL-2.1-or-later", rendered)
        self.assertIn("GPL-3.0-or-later WITH GCC-exception-3.1", rendered)
        self.assertIn("GNU LESSER GENERAL PUBLIC LICENSE", rendered)
        self.assertIn("GCC RUNTIME LIBRARY EXCEPTION", rendered)
        self.assertIn("generated declarations", rendered)
        self.assertIn("source availability and reproducible build instructions remain unresolved", rendered)
        self.assertIn("not a legal determination", rendered)

    def test_runtime_notice_table_covers_direct_qt_and_encrypted_mode_tool_dependencies(self) -> None:
        module = _generator_module()
        rendered = module.render_notices(ROOT)
        self.assertIn("`qt6-imageformats`", rendered)
        self.assertIn("`gocryptfs`", rendered)
        self.assertIn("`libsecret`", rendered)
        self.assertIn("`fuse3`", rendered)
        self.assertIn("/usr/bin/gocryptfs", rendered)
        self.assertIn("/usr/bin/secret-tool", rendered)
        self.assertIn("/usr/bin/fusermount3", rendered)
        self.assertIn("Provides the `secret-tool` executable", rendered)
        self.assertIn("Provides the `fusermount3` helper", rendered)
        self.assertIn("`ffmpeg>=2:9.0.2-2.3`", rendered)
        self.assertIn("`gocryptfs=2.6.1.r20261022.ga8b36f0-1`", rendered)
        self.assertIn("explicitly declared runtime dependencies", rendered)
        self.assertIn("future FFmpeg versions are not pre-approved", rendered)
        self.assertIn("Qt license choices are alternatives in package metadata; this inventory does not select one", rendered)
        self.assertIn("Qt and FFmpeg are system-provided, not bundled in the app artifact", rendered)
        self.assertIn("reviewed candidate FFmpeg package's `.PKGINFO` reports `GPL-3.0-or-later`", rendered)
        self.assertIn("does not change the project's authorized `GPL-3.0-only` selection", rendered)

    def test_recovery_dependency_root_licenses_and_patents_are_reproduced(self) -> None:
        module = _generator_module()
        expected_paths = (
            "vendor/filippo.io/age/LICENSE",
            "vendor/filippo.io/hpke/LICENSE",
            "vendor/golang.org/x/crypto/PATENTS",
            "vendor/golang.org/x/sys/PATENTS",
        )
        with tarfile.open(ROOT / "backend" / "vendor.tar.gz", "r:gz") as archive:
            manifest_file = archive.extractfile("vendor/modules.txt")
            self.assertIsNotNone(manifest_file)
            assert manifest_file is not None
            modules = manifest_file.read().decode("utf-8")
            names = set(archive.getnames())
            raw_documents = {}
            for path in expected_paths:
                file = archive.extractfile(path)
                self.assertIsNotNone(file, f"missing pinned archive notice: {path}")
                assert file is not None
                raw_documents[path] = file.read()
        self.assertIn("# filippo.io/age v1.3.2", modules)
        self.assertIn("# filippo.io/hpke v0.4.0", modules)
        self.assertIn("# golang.org/x/crypto v0.58.0", modules)
        self.assertIn("# golang.org/x/sys v0.49.0", modules)
        self.assertTrue(set(expected_paths).issubset(names))
        # Isolate the new inventory from the previously reviewed modernc archive hash.
        with patch.object(module, "MODERNC_MODULE", "test.invalid/not-present"):
            with patch.object(module, "MODERNC_GENERATED_SOURCES", ()):
                rendered = module.render_notices(ROOT)
        for path, raw in raw_documents.items():
            self.assertIn(f"Source path: `{path}`", rendered)
            self.assertIn(hashlib.sha256(raw).hexdigest(), rendered)
            self.assertIn(raw.decode("utf-8").rstrip("\n"), rendered)

    def test_age_bech32_inline_permission_notice_is_complete_and_hash_anchored(self) -> None:
        module = _generator_module()
        source_path = "vendor/filippo.io/age/internal/bech32/bech32.go"
        with tarfile.open(ROOT / "backend" / "vendor.tar.gz", "r:gz") as archive:
            source_file = archive.extractfile(source_path)
            self.assertIsNotNone(source_file)
            assert source_file is not None
            source = source_file.read().decode("utf-8")
        complete_header = "\n".join(source.splitlines()[:20])
        with patch.object(module, "MODERNC_MODULE", "test.invalid/not-present"):
            with patch.object(module, "MODERNC_GENERATED_SOURCES", ()):
                rendered = module.render_notices(ROOT)
        self.assertIn("## Reviewed age/bech32 inline source notice", rendered)
        self.assertIn(source_path, rendered)
        self.assertIn("b541ad4caca899fb3e11902aa3f478321fe9d623edbf43a76dd306433a7b0436", rendered)
        self.assertIn(complete_header, rendered)
        self.assertIn("The above copyright notice and this permission notice shall be included in", rendered)

    def test_go_runtime_license_is_pinned_to_actual_toolchain_identity(self) -> None:
        module = _generator_module()
        license_path = ROOT / "LICENSES" / "supplemental" / "go-toolchain-LICENSE.txt"
        canonical_license = license_path.read_text(encoding="utf-8").rstrip("\n")
        with patch.object(module, "MODERNC_MODULE", "test.invalid/not-present"):
            with patch.object(module, "MODERNC_GENERATED_SOURCES", ()):
                rendered = module.render_notices(ROOT)
        self.assertIn("## Go runtime toolchain license and source identity", rendered)
        self.assertIn("Upstream Go source pin: `go1.27.3`", rendered)
        self.assertIn("Toolchain identity: `go version go1.27.3-X:nodwarf5 linux/amd64`", rendered)
        self.assertIn("Toolchain executable SHA-256: `21401c8d2dae2e6db0d756f7b9ae5a885ac6c14882cbbe521c6f49b184da8402`", rendered)
        self.assertIn("GOROOT/VERSION SHA-256: `49b1651ee92b70282766993b556905e9464050a811572e047a74f95534ee9ce6`", rendered)
        self.assertIn("SHA-256 `911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad`", rendered)
        self.assertIn(canonical_license, rendered)
        self.assertIn("not a legal approval", rendered)

    def test_age_inline_source_hash_and_archive_size_are_enforced(self) -> None:
        module = _generator_module()
        with patch.object(module, "MODERNC_MODULE", "test.invalid/not-present"):
            with patch.object(module, "AGE_BECH32_SOURCE_SHA256", "0" * 64, create=True):
                with self.assertRaisesRegex(ValueError, "reviewed generated source changed"):
                    module.render_notices(ROOT)
        with patch.object(module, "MODERNC_MODULE", "test.invalid/not-present"):
            with patch.object(module, "MAX_REVIEWED_GENERATED_SOURCE_BYTES", 100):
                with self.assertRaisesRegex(ValueError, "reviewed generated source exceeds the size limit"):
                    module.render_notices(ROOT)

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
