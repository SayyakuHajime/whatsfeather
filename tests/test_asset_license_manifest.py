from __future__ import annotations

import hashlib
import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ASSET_ROOTS = (
    ROOT / "backend/internal/app/testdata",
    ROOT / "desktop/assets",
    ROOT / "desktop/icons",
)
ASSET_SUFFIXES = {".png", ".webp", ".svg"}
RECORD = re.compile(r"\| `([0-9a-f]{64})` \| `([^`]+)` \|")


class AssetLicenseManifestTests(unittest.TestCase):
    def test_manifest_covers_all_licensed_assets_and_current_hashes(self) -> None:
        manifest = ROOT / "LICENSES/ASSET-LICENSES.md"
        self.assertTrue(manifest.is_file(), "asset license manifest is required")
        text = manifest.read_text(encoding="utf-8")
        self.assertIn("GPL-3.0-only", text)

        records: dict[str, str] = {}
        for line in text.splitlines():
            match = RECORD.fullmatch(line.strip())
            if match:
                digest, relative = match.groups()
                self.assertNotIn(relative, records, f"duplicate manifest entry: {relative}")
                records[relative] = digest

        actual = {
            path.relative_to(ROOT).as_posix()
            for asset_root in ASSET_ROOTS
            for path in asset_root.rglob("*")
            if path.is_file() and path.suffix.lower() in ASSET_SUFFIXES
        }
        self.assertEqual(len(records), 32)
        self.assertEqual(set(records), actual, "manifest must cover every current source asset exactly once")
        for relative, expected in records.items():
            content = (ROOT / relative).read_bytes()
            self.assertEqual(hashlib.sha256(content).hexdigest(), expected, relative)


if __name__ == "__main__":
    unittest.main(verbosity=2)
