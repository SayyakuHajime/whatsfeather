"""Safe synthetic-only backend benchmark regression tests."""

from __future__ import annotations

import contextlib
import io
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import benchmark_backend  # noqa: E402


class BenchmarkArgumentsTests(unittest.TestCase):
    def test_cli_rejects_live_and_caller_selected_profile_or_socket(self):
        args = benchmark_backend.parse_args(
            ["--case", "small", "--runs", "3", "--rpc-samples", "10", "--idle-seconds", "1"]
        )
        self.assertEqual(args.case, "small")
        self.assertEqual(args.runs, 3)
        self.assertEqual(args.rpc_samples, 10)
        self.assertEqual(args.idle_seconds, 1)

        for forbidden in ("--live", "--socket", "--data-dir", "--profile"):
            with self.subTest(forbidden=forbidden):
                with contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as raised:
                        benchmark_backend.parse_args([forbidden, "/tmp/not-allowed"])
                self.assertEqual(raised.exception.code, 2)


class SyntheticDatasetTests(unittest.TestCase):
    def test_fixture_sizes_and_large_case_busy_group_skew_are_deterministic(self):
        small = benchmark_backend.dataset_spec("small")
        large = benchmark_backend.dataset_spec("large")
        self.assertEqual((small.chat_count, small.message_count), (20, 2_000))
        self.assertEqual((large.chat_count, large.message_count), (1_000, 100_000))

        small_counts = {}
        for row in benchmark_backend.synthetic_message_rows(small):
            small_counts[row[1]] = small_counts.get(row[1], 0) + 1
        self.assertEqual(len(small_counts), 20)
        self.assertEqual(set(small_counts.values()), {100})

        large_counts = {}
        first_rows = []
        for index, row in enumerate(benchmark_backend.synthetic_message_rows(large)):
            large_counts[row[1]] = large_counts.get(row[1], 0) + 1
            if index < 3:
                first_rows.append(row)
        self.assertEqual(len(large_counts), 1_000)
        self.assertEqual(large_counts["bench:chat-0000"], 50_050)
        self.assertTrue(all(count == 50 for key, count in large_counts.items() if key != "bench:chat-0000"))
        self.assertEqual(first_rows, list(benchmark_backend.synthetic_message_rows(large))[:3])


class DistributionTests(unittest.TestCase):
    def test_latency_distribution_reports_nearest_rank_percentiles(self):
        summary = benchmark_backend.distribution([1.0, 2.0, 3.0, 4.0, 5.0])
        self.assertEqual(summary["count"], 5)
        self.assertEqual(summary["min"], 1.0)
        self.assertEqual(summary["max"], 5.0)
        self.assertEqual(summary["mean"], 3.0)
        self.assertEqual(summary["p50"], 3.0)
        self.assertEqual(summary["p95"], 5.0)
        self.assertEqual(summary["p99"], 5.0)


class ProcMetricsParsingTests(unittest.TestCase):
    def test_proc_parsers_read_rss_pss_and_cpu_with_spaces_in_process_name(self):
        status = "Name:\tdaemon\nVmRSS:\t1234 kB\n"
        smaps_rollup = "Rss: 1400 kB\nPss: 987 kB\n"
        self.assertEqual(
            benchmark_backend.parse_proc_memory_kb(status, smaps_rollup),
            {"rss_kb": 1234, "pss_kb": 987},
        )

        fields = ["S"] + ["0"] * 17
        fields[11] = "321"
        fields[12] = "654"
        stat = "42 (daemon with spaces) " + " ".join(fields)
        self.assertEqual(benchmark_backend.parse_proc_cpu_ticks(stat), 975)

    @unittest.skipUnless(Path("/proc/self").exists(), "Linux /proc is required")
    def test_live_proc_snapshot_reads_rss_pss_and_cpu_for_this_test_process(self):
        import os

        sample = benchmark_backend.read_proc_snapshot(os.getpid())
        self.assertGreater(sample["rss_kb"], 0)
        self.assertGreater(sample["pss_kb"], 0)
        self.assertGreaterEqual(sample["cpu_ticks"], 0)


class DatasetSeedingTests(unittest.TestCase):
    def test_seeding_replaces_only_the_fresh_demo_fixture_with_exact_synthetic_rows(self):
        import sqlite3
        import tempfile

        with tempfile.TemporaryDirectory(prefix="wf-benchmark-test-") as temp:
            profile = Path(temp)
            db_path = profile / "history.db"
            with sqlite3.connect(db_path) as db:
                db.executescript(
                    "CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);"
                    "CREATE TABLE chats(id TEXT PRIMARY KEY,title TEXT NOT NULL,initials TEXT NOT NULL,color TEXT NOT NULL,is_group INTEGER NOT NULL);"
                    "CREATE TABLE messages(id TEXT NOT NULL,chat_id TEXT NOT NULL,body TEXT NOT NULL,timestamp INTEGER NOT NULL,outgoing INTEGER NOT NULL,status TEXT NOT NULL);"
                )
                db.execute("INSERT INTO meta VALUES('mode','demo')")
                for chat_id in ("demo:alex", "demo:maya", "demo:studio", "demo:notes"):
                    db.execute("INSERT INTO chats VALUES(?,?,?,?,0)", (chat_id, "Demo", "D", "#123456"))
                db.executemany(
                    "INSERT INTO messages VALUES(?,?,?,?,?,?)",
                    ((f"seed-{index}", "demo:alex", "old synthetic", index, 0, "demo_received") for index in range(128)),
                )
            db.close()
            db_path.chmod(0o600)

            result = benchmark_backend.seed_synthetic_dataset(profile, benchmark_backend.dataset_spec("small"))
            self.assertEqual(result, {"chats": 20, "messages": 2_000, "busy_chat_messages": 100})
            with sqlite3.connect(db_path) as db:
                self.assertEqual(db.execute("SELECT value FROM meta WHERE key='mode'").fetchone()[0], "demo")
                self.assertEqual(db.execute("SELECT count(*) FROM chats").fetchone()[0], 20)
                self.assertEqual(db.execute("SELECT count(*) FROM messages").fetchone()[0], 2_000)
                self.assertEqual(db.execute("SELECT count(*) FROM messages WHERE chat_id='bench:chat-0000'").fetchone()[0], 100)
                self.assertEqual(db.execute("SELECT count(*) FROM messages WHERE id LIKE 'seed-%'").fetchone()[0], 0)
            db.close()


class RPCFrameTests(unittest.TestCase):
    def test_rpc_response_requires_matching_id_and_a_bounded_newline_frame(self):
        frame = b'{"id":"req-1","result":{"mode":"demo"}}\n'
        self.assertEqual(benchmark_backend.parse_rpc_frame(frame, "req-1"), {"mode": "demo"})
        with self.assertRaises(ValueError):
            benchmark_backend.parse_rpc_frame(b'{"id":"req-1","result":{}}', "req-1")
        with self.assertRaises(ValueError):
            benchmark_backend.parse_rpc_frame(b"x" * (benchmark_backend.MAX_RPC_FRAME_BYTES + 1), "req-1")
        with self.assertRaises(ValueError):
            benchmark_backend.parse_rpc_frame(b'{"id":"other","result":{}}\n', "req-1")


class UnixRPCClientTests(unittest.TestCase):
    def test_rpc_round_trip_uses_local_unix_socket_and_returns_bounded_timing(self):
        import json
        import socket
        import tempfile
        import threading

        with tempfile.TemporaryDirectory(prefix="wf-rpc-unit-") as temp:
            path = str(Path(temp) / "s")
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            listener.bind(path)
            listener.listen(1)

            def serve_one():
                with listener.accept()[0] as peer:
                    request = json.loads(peer.makefile("rb").readline(benchmark_backend.MAX_RPC_FRAME_BYTES + 1))
                    frame = json.dumps({"id": request["id"], "result": {"ok": True}}).encode() + b"\n"
                    peer.sendall(frame)

            server = threading.Thread(target=serve_one, daemon=True)
            server.start()
            try:
                result, elapsed_ms, response_bytes = benchmark_backend.rpc_call(
                    path, "app.status", {}, "unit-1"
                )
            finally:
                listener.close()
                server.join(timeout=2)
            self.assertEqual(result, {"ok": True})
            self.assertGreaterEqual(elapsed_ms, 0.0)
            self.assertLessEqual(response_bytes, benchmark_backend.MAX_RPC_FRAME_BYTES)


if __name__ == "__main__":
    unittest.main()
