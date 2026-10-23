#!/usr/bin/env python3
"""Dependency-free checks for the hosted Qt workflow matrix."""

from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = ROOT / ".github" / "workflows" / "verify.yml"
EXPECTED_QT_ARCHES = {
    "6.5.3": "gcc_64",
    "6.8.3": "linux_gcc_64",
}


class WorkflowConfigurationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")

    def test_qt_matrix_maps_each_version_to_its_package_architecture(self):
        matrix_start = re.search(r"(?m)^      matrix:\s*$", self.workflow)
        if matrix_start is None:
            self.fail("native-qt matrix block is missing")
        steps_start = re.search(
            r"(?m)^    steps:\s*$", self.workflow[matrix_start.end() :]
        )
        if steps_start is None:
            self.fail("native-qt steps block is missing")
        matrix = self.workflow[
            matrix_start.end() : matrix_start.end() + steps_start.start()
        ]

        entries = re.findall(
            r"(?m)^          - qt-version:\s*['\"]?([^'\"\s]+)['\"]?\s*\n"
            r"            qt-arch:\s*([A-Za-z0-9_]+)\s*$",
            matrix,
        )
        self.assertEqual(len(entries), len(EXPECTED_QT_ARCHES))
        self.assertEqual(dict(entries), EXPECTED_QT_ARCHES)

    def test_install_qt_action_consumes_the_matrix_architecture(self):
        action = re.search(
            r"(?ms)^      - name: Install Qt baseline and required modules\n"
            r"(.*?)(?=^      - name:|\Z)",
            self.workflow,
        )
        if action is None:
            self.fail("install-qt action step is missing")
        self.assertRegex(
            action.group(1),
            r"(?m)^          arch:\s*\$\{\{\s*matrix\.qt-arch\s*\}\}\s*$",
        )


if __name__ == "__main__":
    unittest.main()
