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

    def test_offline_go_job_does_not_install_system_packages(self):
        job = re.search(
            r"(?ms)^  offline-gates-and-go:\n(.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            self.workflow,
        )
        if job is None:
            self.fail("offline Go job is missing")
        self.assertNotRegex(job.group(1), r"(?m)^\s*sudo apt-get\b")

    def native_qt_job(self):
        job = re.search(
            r"(?ms)^  native-qt:\n(.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            self.workflow,
        )
        if job is None:
            self.fail("native Qt job is missing")
        return job.group(1)

    def native_qt_step(self, name):
        job = self.native_qt_job()
        step = re.search(
            rf"(?ms)^      - name: {re.escape(name)}\n(.*?)(?=^      - name:|\Z)",
            job,
        )
        if step is None:
            self.fail(f"native Qt step is missing: {name}")
        return step.group(1)

    def test_native_qt_job_has_cold_install_headroom(self):
        job_timeout = re.search(
            r"(?m)^    timeout-minutes:\s*(\d+)\s*$", self.native_qt_job()
        )
        if job_timeout is None:
            self.fail("native Qt job needs a hard deadline")
        self.assertGreaterEqual(int(job_timeout.group(1)), 90)

    def test_native_prerequisites_bound_apt_and_install_ffmpeg_without_recommends(self):
        prerequisites = self.native_qt_step("Install native build/runtime prerequisites")
        self.assertRegex(
            prerequisites,
            r"(?m)^        timeout-minutes:\s*50\s*$",
        )
        self.assertRegex(
            prerequisites,
            r"(?m)^\s*sudo apt-get install --yes --no-install-recommends"
            r"[^\n]*\bffmpeg\b",
        )

    def test_native_qt_and_acceptance_steps_have_hard_deadlines(self):
        for name in (
            "Install Qt baseline and required modules",
            "Run canonical build and synthetic acceptance",
        ):
            with self.subTest(step=name):
                self.assertRegex(
                    self.native_qt_step(name),
                    r"(?m)^        timeout-minutes:\s*45\s*$",
                )


if __name__ == "__main__":
    unittest.main()
