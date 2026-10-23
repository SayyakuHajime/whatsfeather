"""Unit tests for the synthetic native-desktop benchmark safety boundaries."""

from __future__ import annotations

import contextlib
import io
import os
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import benchmark_desktop  # noqa: E402


class ArgumentTests(unittest.TestCase):
    def test_cli_defaults_to_bounded_demo_only_desktop_runs(self):
        args = benchmark_desktop.parse_args(["--runs", "3"])
        self.assertEqual(args.runs, 3)
        self.assertEqual(args.startup_timeout, benchmark_desktop.STARTUP_TIMEOUT_SECONDS)
        self.assertEqual(args.close_timeout, benchmark_desktop.CLOSE_TIMEOUT_SECONDS)
        self.assertEqual(args.poll_interval_ms, benchmark_desktop.POLL_INTERVAL_MS)

    def test_cli_rejects_live_profile_socket_and_timer_overrides(self):
        for forbidden in (
            "--live",
            "--socket",
            "--data-dir",
            "--profile",
            "--quit-after",
            "--platform",
        ):
            with self.subTest(forbidden=forbidden):
                with contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as raised:
                        benchmark_desktop.parse_args([forbidden, "value"])
                self.assertEqual(raised.exception.code, 2)

    def test_cli_rejects_out_of_range_run_counts(self):
        for value in ("0", "21", "not-an-int"):
            with self.subTest(value=value):
                with contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as raised:
                        benchmark_desktop.parse_args(["--runs", value])
                self.assertEqual(raised.exception.code, 2)


class ProcParserTests(unittest.TestCase):
    def test_proc_memory_parser_reads_vm_rss_and_pss_with_units(self):
        status = "Name:\twhatsfeather\nUid:\t1000\t1000\t1000\t1000\nVmRSS:\t43210 kB\n"
        smaps = "Rss: 50000 kB\nPss: 12345 kB\n"
        self.assertEqual(
            benchmark_desktop.parse_proc_memory_kb(status, smaps),
            {"rss_kb": 43210, "pss_kb": 12345},
        )

    def test_proc_memory_parser_rejects_missing_or_malformed_fields(self):
        with self.assertRaises(ValueError):
            benchmark_desktop.parse_proc_memory_kb("VmRSS: 10 kB\n", "")
        with self.assertRaises(ValueError):
            benchmark_desktop.parse_proc_memory_kb("VmRSS: nope kB\n", "Pss: 2 kB\n")

    def test_proc_stat_parser_handles_spaces_and_parentheses_in_comm(self):
        fields = ["S", "1", "845", "845", "0", "0", "0", "0", "0", "0", "0", "12", "34"]
        text = "845 (WhatsFeather demo (Qt)) " + " ".join(fields)
        result = benchmark_desktop.parse_proc_stat(text)
        self.assertEqual(result["ppid"], 1)
        self.assertEqual(result["pgrp"], 845)
        self.assertEqual(result["cpu_ticks"], 46)

    def test_child_pid_parser_rejects_invalid_duplicate_or_zero_ids(self):
        self.assertEqual(benchmark_desktop.parse_child_pids("345 346\n"), [345, 346])
        for value in ("0", "-3", "12 12", "abc", "12/../13"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                benchmark_desktop.parse_child_pids(value)


class ProcessIdentitySafetyTests(unittest.TestCase):
    def _fake_process(self, proc_root: Path, pid: int, exe: Path, ppid: int, pgrp: int, uid: int):
        proc = proc_root / str(pid)
        (proc / "task" / str(pid)).mkdir(parents=True)
        fields = ["S", str(ppid), str(pgrp), str(pgrp), "0", "0", "0", "0", "0", "0", "0", "0", "0"]
        (proc / "stat").write_text(f"{pid} (owned benchmark) " + " ".join(fields), encoding="ascii")
        (proc / "status").write_text(
            f"Name:\towned benchmark\nUid:\t{uid}\t{uid}\t{uid}\t{uid}\n",
            encoding="ascii",
        )
        (proc / "task" / str(pid) / "children").write_text("", encoding="ascii")
        (proc / "exe").symlink_to(exe)
        return proc

    def test_process_identity_requires_expected_uid_parent_group_and_executable(self):
        with tempfile.TemporaryDirectory(prefix="wf-desktop-proc-") as temp:
            root = Path(temp)
            proc_root = root / "proc"
            proc_root.mkdir()
            executable = root / "whatsfeather"
            executable.write_text("synthetic executable", encoding="ascii")
            proc = self._fake_process(proc_root, 845, executable, ppid=700, pgrp=845, uid=os.getuid())

            identity = benchmark_desktop.read_process_identity(
                845,
                expected_exe=executable,
                expected_parent=700,
                expected_pgrp=845,
                expected_uid=os.getuid(),
                proc_root=proc_root,
            )
            self.assertEqual(identity["pid"], 845)
            self.assertEqual(identity["ppid"], 700)
            self.assertEqual(identity["pgrp"], 845)
            self.assertEqual(identity["uid"], os.getuid())

            with self.assertRaises(RuntimeError):
                benchmark_desktop.read_process_identity(
                    845, executable, expected_parent=1, expected_pgrp=845,
                    expected_uid=os.getuid(), proc_root=proc_root,
                )
            with self.assertRaises(RuntimeError):
                benchmark_desktop.read_process_identity(
                    845, executable, expected_parent=700, expected_pgrp=846,
                    expected_uid=os.getuid(), proc_root=proc_root,
                )
            with self.assertRaises(RuntimeError):
                benchmark_desktop.read_process_identity(
                    845, root / "other", expected_parent=700, expected_pgrp=845,
                    expected_uid=os.getuid(), proc_root=proc_root,
                )
            proc.joinpath("status").write_text("Uid:\t99999\t99999\t99999\t99999\n", encoding="ascii")
            with self.assertRaises(RuntimeError):
                benchmark_desktop.read_process_identity(
                    845, executable, expected_parent=700, expected_pgrp=845,
                    expected_uid=os.getuid(), proc_root=proc_root,
                )

    def test_daemon_argv_is_exact_demo_socket_and_fresh_profile_only(self):
        argv = [
            "/opt/whatsfeather/bin/whatsfeatherd",
            "--demo",
            "--socket",
            "/private/runtime/ui-123.sock",
            "--data-dir",
            "/private/data/whatsfeather/demo",
        ]
        self.assertTrue(
            benchmark_desktop.validate_daemon_argv(
                argv,
                expected_binary=Path("/opt/whatsfeather/bin/whatsfeatherd"),
                expected_socket=Path("/private/runtime/ui-123.sock"),
                expected_data_dir=Path("/private/data/whatsfeather/demo"),
            )
        )
        for unsafe in (
            [*argv[:1], "--live", *argv[2:]],
            [*argv, "--socket", "/other.sock"],
            [*argv[:-1], "/other/profile"],
        ):
            with self.subTest(argv=unsafe), self.assertRaises(RuntimeError):
                benchmark_desktop.validate_daemon_argv(
                    unsafe,
                    expected_binary=Path("/opt/whatsfeather/bin/whatsfeatherd"),
                    expected_socket=Path("/private/runtime/ui-123.sock"),
                    expected_data_dir=Path("/private/data/whatsfeather/demo"),
                )


class RPCAndWindowParserTests(unittest.TestCase):
    def test_rpc_frame_requires_one_bounded_matching_response(self):
        frame = b'{"id":"ready-1","result":{"mode":"demo","state":"demo"}}\n'
        self.assertEqual(
            benchmark_desktop.parse_rpc_frame(frame, "ready-1"),
            {"mode": "demo", "state": "demo"},
        )
        invalid = (
            b'{"id":"ready-1","result":{}}',
            b'{"id":"other","result":{}}\n',
            b'{"id":"ready-1","error":{"message":"no"}}\n',
            b"not-json\n",
            b"x" * (benchmark_desktop.MAX_RPC_FRAME_BYTES + 1),
        )
        for value in invalid:
            with self.subTest(value=value[:40]), self.assertRaises((ValueError, RuntimeError)):
                benchmark_desktop.parse_rpc_frame(value, "ready-1")

    def test_xdotool_parser_accepts_only_unique_positive_decimal_window_ids(self):
        self.assertEqual(benchmark_desktop.parse_window_ids("1048577\n2097154\n"), [1048577, 2097154])
        for value in ("0\n", "-1\n", "0x123\n", "1\n1\n", "abc\n"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                benchmark_desktop.parse_window_ids(value)


class DistributionAndEnvironmentTests(unittest.TestCase):
    def test_distribution_reports_raw_count_and_nearest_rank_summary(self):
        self.assertEqual(
            benchmark_desktop.distribution([1.0, 2.0, 3.0, 4.0, 5.0]),
            {"count": 5, "min": 1.0, "max": 5.0, "mean": 3.0, "p50": 3.0, "p95": 5.0, "p99": 5.0},
        )
        with self.assertRaises(ValueError):
            benchmark_desktop.distribution([])

    def test_each_run_gets_owner_only_fresh_xdg_directories_without_mutating_parent_env(self):
        with tempfile.TemporaryDirectory(prefix="wf-desktop-xdg-") as temp:
            root = Path(temp) / "run"
            root.mkdir(mode=0o700)
            original = {"HOME": "/home/unchanged", "DISPLAY": ":test", "XDG_DATA_HOME": "/old"}
            env = benchmark_desktop.prepare_isolated_environment(root, original)
            self.assertEqual(original["XDG_DATA_HOME"], "/old")
            self.assertEqual(env["HOME"], "/home/unchanged")
            self.assertEqual(env["DISPLAY"], ":test")
            for key in ("XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"):
                path = Path(env[key])
                self.assertTrue(path.is_relative_to(root))
                self.assertTrue(path.is_dir())
                self.assertEqual(path.stat().st_mode & 0o777, 0o700)
            self.assertEqual(env["QT_QPA_PLATFORM"], "xcb")


if __name__ == "__main__":
    unittest.main()
