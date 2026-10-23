from __future__ import annotations

import importlib.util
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "dependency-inventory.py"
SPEC = importlib.util.spec_from_file_location("dependency_inventory", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
inventory_module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(inventory_module)


class DependencyInventoryTests(unittest.TestCase):
    def test_current_manifest_inventory_is_offline_and_does_not_interpret_project_license(self) -> None:
        root = SCRIPT.parents[1]
        report = inventory_module.build_inventory(root)
        self.assertFalse(report["network_access"])
        self.assertEqual(report["integrity"]["go_requirements_missing_h1_sum"], [])
        self.assertEqual(report["project_license_status"], "present_not_interpreted")
        self.assertGreater(report["integrity"]["go_requirements"], 0)
        qt = [item for item in report["dependencies"] if item["ecosystem"] == "Qt"]
        self.assertGreater(len(qt), 0)
        self.assertEqual({item["version_constraint"] for item in qt}, {">=6.5"})
        self.assertTrue(all(item["license_status"] == "not_assessed" for item in report["dependencies"]))

    def test_parser_marks_indirect_requirements_and_checksum_gaps(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "backend").mkdir()
            (root / "backend" / "go.mod").write_text(
                "module example.test/app\n\ngo 1.24.0\n\n"
                "require (\n\texample.test/direct v1.2.3\n\texample.test/indirect v2.0.0 // indirect\n)\n",
                encoding="utf-8",
            )
            (root / "backend" / "go.sum").write_text(
                "example.test/direct v1.2.3 h1:abcd\n", encoding="utf-8"
            )
            (root / "CMakeLists.txt").write_text(
                "cmake_minimum_required(VERSION 3.22)\n"
                "find_package(Qt6 6.5 REQUIRED COMPONENTS Core Gui Quick)\n",
                encoding="utf-8",
            )
            report = inventory_module.build_inventory(root)
        go = [item for item in report["dependencies"] if item["ecosystem"] == "Go"]
        by_name = {item["name"]: item for item in go}
        self.assertEqual(by_name["example.test/direct"]["dependency_type"], "direct")
        self.assertEqual(by_name["example.test/indirect"]["dependency_type"], "indirect")
        self.assertEqual(report["integrity"]["go_requirements_missing_h1_sum"], ["example.test/indirect v2.0.0"])
        self.assertEqual(report["build_requirements"]["cmake_minimum"], "3.22")
        self.assertEqual(report["build_requirements"]["qt_minimum"], "6.5")

    def test_report_does_not_claim_license_approval(self) -> None:
        report = inventory_module.build_inventory(SCRIPT.parents[1])
        self.assertIn("license_status=not_assessed is not a license approval", report["scope_note"])


if __name__ == "__main__":
    unittest.main()
