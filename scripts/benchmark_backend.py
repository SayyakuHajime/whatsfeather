#!/usr/bin/env python3
"""Measure the real Go backend using an isolated synthetic demo profile only."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import platform
import socket
import sqlite3
import statistics
import stat
import subprocess
import sys
import tempfile
import time
from contextlib import closing
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterator

MAX_RPC_FRAME_BYTES = 262_144
RPC_TIMEOUT_SECONDS = 2.0
STARTUP_TIMEOUT_SECONDS = 10.0
ROOT = Path(__file__).resolve().parents[1]
DAEMON_BINARY = ROOT / "build" / "bin" / "whatsfeatherd"
PERFORMANCE_PROPOSAL = ROOT / ".hermes" / "project" / "docs" / "PERFORMANCE.md"


def _bounded_int(minimum: int, maximum: int):
    def parse(value: str) -> int:
        try:
            parsed = int(value)
        except ValueError as exc:
            raise argparse.ArgumentTypeError("must be an integer") from exc
        if not minimum <= parsed <= maximum:
            raise argparse.ArgumentTypeError(f"must be between {minimum} and {maximum}")
        return parsed

    return parse


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Benchmark the local Go daemon against a private, deterministic synthetic demo dataset. "
            "The tool always creates its own temporary profile and AF_UNIX socket."
        ),
        epilog="Backend-only measurements: no GUI, browser, WhatsApp account, or network comparison.",
    )
    parser.add_argument("--case", choices=("small", "large"), default="small")
    parser.add_argument("--runs", type=_bounded_int(1, 20), default=5)
    parser.add_argument("--rpc-samples", type=_bounded_int(1, 100), default=20)
    parser.add_argument("--idle-seconds", type=_bounded_int(1, 60), default=2)
    return parser.parse_args(argv)


@dataclass(frozen=True)
class DatasetSpec:
    name: str
    chat_count: int
    message_count: int
    busy_chat_messages: int


def dataset_spec(case: str) -> DatasetSpec:
    if case == "small":
        return DatasetSpec(case, chat_count=20, message_count=2_000, busy_chat_messages=100)
    if case == "large":
        return DatasetSpec(case, chat_count=1_000, message_count=100_000, busy_chat_messages=50_050)
    raise ValueError(f"unknown dataset case: {case}")


def synthetic_chat_rows(spec: DatasetSpec) -> Iterator[tuple[str, str, str, str, int]]:
    colors = ("#526a63", "#6b617b", "#626f8a", "#8a7357")
    for index in range(spec.chat_count):
        title = f"Synthetic Bench Chat {index:04d}"
        initials = f"B{index % 100:02d}"
        yield (f"bench:chat-{index:04d}", title, initials, colors[index % len(colors)], int(index == 0))


def synthetic_message_rows(spec: DatasetSpec) -> Iterator[tuple[str, str, str, int, int, str]]:
    regular_count = (spec.message_count - spec.busy_chat_messages) // (spec.chat_count - 1)
    global_index = 0
    for chat_index in range(spec.chat_count):
        chat_id = f"bench:chat-{chat_index:04d}"
        count = spec.busy_chat_messages if chat_index == 0 else regular_count
        for _ in range(count):
            message_id = f"bench-msg-{global_index:06d}"
            body = f"Synthetic benchmark message {global_index:06d} for chat {chat_index:04d}"
            outgoing = int(global_index % 3 == 1)
            status = "demo_sent" if outgoing else "demo_received"
            timestamp = 1_700_000_000_000 + global_index
            yield (message_id, chat_id, body, timestamp, outgoing, status)
            global_index += 1


def seed_synthetic_dataset(profile_dir: Path, spec: DatasetSpec) -> dict[str, int]:
    profile_dir = Path(profile_dir)
    db_path = profile_dir / "history.db"
    try:
        profile_stat = profile_dir.lstat()
        db_stat = db_path.lstat()
    except OSError as exc:
        raise RuntimeError(f"benchmark profile/database is unavailable: {exc}") from exc
    if not stat.S_ISDIR(profile_stat.st_mode) or stat.S_ISLNK(profile_stat.st_mode):
        raise RuntimeError("benchmark profile must be a real directory")
    if not stat.S_ISREG(db_stat.st_mode) or stat.S_ISLNK(db_stat.st_mode) or db_stat.st_nlink != 1:
        raise RuntimeError("benchmark database must be a private regular file")
    if db_stat.st_uid != os.getuid() or db_stat.st_mode & 0o077:
        raise RuntimeError("benchmark database must be owner-only and owned by this user")

    allowed_demo_chats = {"demo:alex", "demo:maya", "demo:studio", "demo:notes"}
    with closing(sqlite3.connect(db_path, timeout=5)) as db:
        db.execute("PRAGMA foreign_keys=ON")
        try:
            mode = db.execute("SELECT value FROM meta WHERE key='mode'").fetchone()
            chat_ids = {row[0] for row in db.execute("SELECT id FROM chats")}
            message_count = db.execute("SELECT count(*) FROM messages").fetchone()[0]
        except sqlite3.Error as exc:
            raise RuntimeError(f"fresh demo profile schema is not recognized: {exc}") from exc
        if mode != ("demo",) or chat_ids != allowed_demo_chats or message_count != 128:
            raise RuntimeError("refusing to seed anything except the untouched four-chat demo fixture")

        try:
            db.execute("BEGIN IMMEDIATE")
            db.execute("DELETE FROM messages")
            db.execute("DELETE FROM chats")
            table_names = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            if "communities" in table_names:
                db.execute("DELETE FROM communities")
            db.executemany(
                "INSERT INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,?,?)",
                synthetic_chat_rows(spec),
            )
            db.executemany(
                "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES(?,?,?,?,?,?)",
                synthetic_message_rows(spec),
            )
            if "community_sync_state" in table_names:
                db.execute("UPDATE community_sync_state SET source='benchmark_synthetic',refreshed_at='' WHERE id=1")
            db.commit()
        except Exception:
            db.rollback()
            raise
        actual_chats = db.execute("SELECT count(*) FROM chats").fetchone()[0]
        actual_messages = db.execute("SELECT count(*) FROM messages").fetchone()[0]
        busy_messages = db.execute("SELECT count(*) FROM messages WHERE chat_id='bench:chat-0000'").fetchone()[0]
        if (actual_chats, actual_messages, busy_messages) != (
            spec.chat_count, spec.message_count, spec.busy_chat_messages
        ):
            raise RuntimeError("synthetic dataset count verification failed")
        if db.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
            raise RuntimeError("synthetic benchmark database failed integrity_check")
        if db.execute("PRAGMA foreign_key_check").fetchone() is not None:
            raise RuntimeError("synthetic benchmark database has foreign-key violations")
    return {"chats": actual_chats, "messages": actual_messages, "busy_chat_messages": busy_messages}


def distribution(values: list[float]) -> dict[str, int | float]:
    if not values:
        raise ValueError("distribution requires at least one sample")
    ordered = sorted(float(value) for value in values)
    def rank(percent: float) -> float:
        index = max(0, math.ceil(percent * len(ordered)) - 1)
        return ordered[index]

    return {
        "count": len(ordered),
        "min": round(ordered[0], 3),
        "max": round(ordered[-1], 3),
        "mean": round(statistics.fmean(ordered), 3),
        "p50": round(rank(0.50), 3),
        "p95": round(rank(0.95), 3),
        "p99": round(rank(0.99), 3),
    }


def _proc_field_kb(text: str, field: str) -> int:
    for line in text.splitlines():
        parts = line.split()
        if parts and parts[0].rstrip(":") == field:
            if len(parts) < 2:
                break
            return int(parts[1])
    raise ValueError(f"/proc data is missing {field}")


def parse_proc_memory_kb(status_text: str, smaps_rollup_text: str) -> dict[str, int]:
    return {
        "rss_kb": _proc_field_kb(status_text, "VmRSS"),
        "pss_kb": _proc_field_kb(smaps_rollup_text, "Pss"),
    }


def parse_proc_cpu_ticks(stat_text: str) -> int:
    closing_paren = stat_text.rfind(")")
    if closing_paren < 0:
        raise ValueError("invalid /proc stat command field")
    fields = stat_text[closing_paren + 1 :].split()
    if len(fields) <= 12:
        raise ValueError("/proc stat is missing CPU counters")
    return int(fields[11]) + int(fields[12])


def read_proc_snapshot(pid: int) -> dict[str, int]:
    base = Path("/proc") / str(pid)
    try:
        status_text = (base / "status").read_text(encoding="ascii", errors="replace")
        smaps_text = (base / "smaps_rollup").read_text(encoding="ascii", errors="replace")
        stat_text = (base / "stat").read_text(encoding="ascii", errors="replace")
    except OSError as exc:
        raise RuntimeError(f"cannot read /proc metrics for pid {pid}: {exc}") from exc
    memory = parse_proc_memory_kb(status_text, smaps_text)
    memory["cpu_ticks"] = parse_proc_cpu_ticks(stat_text)
    return memory


def measure_idle_cpu_percent(pid: int, seconds: int) -> tuple[float, dict[str, int], dict[str, int]]:
    before = read_proc_snapshot(pid)
    started = time.monotonic()
    time.sleep(seconds)
    after = read_proc_snapshot(pid)
    elapsed = time.monotonic() - started
    tick_delta = after["cpu_ticks"] - before["cpu_ticks"]
    if tick_delta < 0 or elapsed <= 0:
        raise RuntimeError("invalid process CPU counters during idle measurement")
    ticks_per_second = os.sysconf("SC_CLK_TCK")
    percent = 100.0 * tick_delta / ticks_per_second / elapsed
    return round(percent, 3), before, after


def parse_rpc_frame(frame: bytes, request_id: str) -> dict:
    if not frame or len(frame) > MAX_RPC_FRAME_BYTES:
        raise ValueError("RPC response exceeds the frame limit or is empty")
    if not frame.endswith(b"\n") or frame.count(b"\n") != 1:
        raise ValueError("RPC response is not one newline-terminated frame")
    try:
        response = json.loads(frame)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError(f"invalid RPC JSON response: {exc}") from exc
    if not isinstance(response, dict) or response.get("id") != request_id:
        raise ValueError("RPC response ID does not match request")
    if "error" in response:
        raise RuntimeError(f"RPC error: {response['error']}")
    if "result" not in response:
        raise ValueError("RPC response has no result")
    return response["result"]


def rpc_call(
    socket_path: str | Path,
    method: str,
    params: dict,
    request_id: str,
    timeout: float = RPC_TIMEOUT_SECONDS,
) -> tuple[dict, float, int]:
    request = {"v": 1, "id": request_id, "method": method, "params": params}
    encoded = json.dumps(request, ensure_ascii=False, separators=(",", ":")).encode("utf-8") + b"\n"
    if len(encoded) > MAX_RPC_FRAME_BYTES:
        raise ValueError("RPC request exceeds the frame limit")
    started = time.perf_counter()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(timeout)
        conn.connect(str(socket_path))
        conn.sendall(encoded)
        with conn.makefile("rb") as stream:
            frame = stream.readline(MAX_RPC_FRAME_BYTES + 1)
    result = parse_rpc_frame(frame, request_id)
    elapsed_ms = (time.perf_counter() - started) * 1000.0
    return result, elapsed_ms, len(frame)


def daemon_diagnostic(log_path: Path) -> str:
    try:
        with log_path.open("rb") as log_file:
            raw = log_file.read(16_384)
        return raw.decode("utf-8", errors="replace").strip()
    except OSError:
        return ""


def stop_daemon(process: subprocess.Popen, log_path: Path) -> tuple[int, str]:
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
    diagnostic = daemon_diagnostic(log_path)
    return int(process.returncode), diagnostic


def wait_for_readiness(process: subprocess.Popen, socket_path: Path) -> dict:
    deadline = time.monotonic() + STARTUP_TIMEOUT_SECONDS
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"demo daemon exited before readiness (code {process.returncode})")
        try:
            status, _, _ = rpc_call(socket_path, "app.status", {}, "benchmark-readiness")
            if not isinstance(status, dict) or status.get("mode") != "demo" or status.get("state") != "demo":
                raise RuntimeError("daemon readiness did not report the expected demo state")
            return status
        except (OSError, TimeoutError, ValueError, RuntimeError) as exc:
            last_error = exc
            time.sleep(0.02)
    raise RuntimeError(f"demo daemon did not become RPC-ready within {STARTUP_TIMEOUT_SECONDS}s: {last_error}")


def start_daemon(
    profile_dir: Path, socket_path: Path, workspace: Path, new_profile: bool
) -> tuple[subprocess.Popen, Path, dict, float]:
    if not DAEMON_BINARY.is_file() or not os.access(DAEMON_BINARY, os.X_OK):
        raise RuntimeError(f"existing daemon binary is required; build was not run: {DAEMON_BINARY}")
    if socket_path.exists():
        raise RuntimeError("temporary AF_UNIX socket path unexpectedly already exists")
    if new_profile:
        if profile_dir.exists():
            raise RuntimeError("temporary demo profile path unexpectedly already exists")
        profile_dir.mkdir(mode=0o700)
    else:
        try:
            profile_stat = profile_dir.lstat()
        except OSError as exc:
            raise RuntimeError(f"previously initialized demo profile is unavailable: {exc}") from exc
        if not stat.S_ISDIR(profile_stat.st_mode) or stat.S_ISLNK(profile_stat.st_mode):
            raise RuntimeError("reused demo profile is not a real directory")
        if profile_stat.st_uid != os.getuid() or profile_stat.st_mode & 0o077:
            raise RuntimeError("reused demo profile is not private and owner-owned")
    if len(os.fsencode(socket_path)) >= 100:
        raise RuntimeError("temporary AF_UNIX socket path is too long; choose a shorter TMPDIR")
    log_path = workspace / f"daemon-{time.monotonic_ns()}.stderr"
    fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    started = time.perf_counter()
    try:
        with os.fdopen(fd, "wb") as log_file:
            process = subprocess.Popen(
                [str(DAEMON_BINARY), "--demo", "--socket", str(socket_path), "--data-dir", str(profile_dir)],
                cwd=ROOT,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=log_file,
                close_fds=True,
                start_new_session=True,
            )
    except Exception:
        try:
            os.unlink(log_path)
        except OSError:
            pass
        raise
    try:
        status = wait_for_readiness(process, socket_path)
    except Exception as exc:
        return_code, diagnostic = stop_daemon(process, log_path)
        details = f"; exit={return_code}; stderr={diagnostic}" if diagnostic else f"; exit={return_code}"
        raise RuntimeError(f"demo daemon failed readiness{details}") from exc
    ready_ms = (time.perf_counter() - started) * 1000.0
    return process, log_path, status, ready_ms


def rpc_probes(spec: DatasetSpec) -> list[tuple[str, str, dict]]:
    busy_tail = f"bench-msg-{spec.busy_chat_messages - 1:06d}"
    return [
        ("app.status", "app.status", {}),
        ("chat.list.limit_1000", "chat.list", {"limit": 1000}),
        ("message.list.busy.limit_100", "message.list", {"chat_id": "bench:chat-0000", "limit": 100}),
        ("message.list.regular.limit_50", "message.list", {"chat_id": "bench:chat-0001", "limit": 50}),
        ("message.context.busy_tail", "message.context", {"chat_id": "bench:chat-0000", "message_id": busy_tail}),
    ]


def validate_probe_result(name: str, result: dict, spec: DatasetSpec) -> None:
    if not isinstance(result, dict):
        raise RuntimeError(f"{name} did not return a JSON object")
    if name == "app.status":
        if result.get("mode") != "demo" or result.get("state") != "demo":
            raise RuntimeError("RPC probe reported unexpected non-demo state")
    elif name == "chat.list.limit_1000":
        chats = result.get("chats")
        if not isinstance(chats, list) or not chats or not all(
            isinstance(chat, dict) and str(chat.get("id", "")).startswith("bench:chat-") for chat in chats
        ):
            raise RuntimeError("chat.list did not return the deterministic synthetic dataset")
    elif name in {"message.list.busy.limit_100", "message.list.regular.limit_50"}:
        messages = result.get("messages")
        expected_chat = "bench:chat-0000" if ".busy." in name else "bench:chat-0001"
        if not isinstance(messages, list) or not messages or any(message.get("chat_id") != expected_chat for message in messages):
            raise RuntimeError(f"{name} did not return messages from the requested synthetic chat")
    elif name == "message.context.busy_tail":
        messages = result.get("messages")
        target = f"bench-msg-{spec.busy_chat_messages - 1:06d}"
        if not isinstance(messages, list) or not any(message.get("id") == target for message in messages):
            raise RuntimeError("message.context omitted the selected synthetic target")


def measure_rpc_run(
    process: subprocess.Popen,
    socket_path: Path,
    spec: DatasetSpec,
    rpc_samples: int,
    idle_seconds: int,
    run_index: int,
) -> dict:
    latencies: dict[str, list[float]] = {}
    response_sizes: dict[str, list[int]] = {}
    for name, method, params in rpc_probes(spec):
        result, _, _ = rpc_call(socket_path, method, params, f"warmup-{run_index}-{name}")
        validate_probe_result(name, result, spec)
        latencies[name] = []
        response_sizes[name] = []
        for sample_index in range(rpc_samples):
            request_id = f"bench-{run_index}-{sample_index}-{name}"
            result, elapsed_ms, response_bytes = rpc_call(socket_path, method, params, request_id)
            validate_probe_result(name, result, spec)
            latencies[name].append(elapsed_ms)
            response_sizes[name].append(response_bytes)

    idle_cpu_percent, before_idle, after_idle = measure_idle_cpu_percent(process.pid, idle_seconds)
    return {
        "rpc_latency_ms": latencies,
        "rpc_response_bytes": response_sizes,
        "idle_cpu_percent_one_logical_core": idle_cpu_percent,
        "rss_before_idle_kb": before_idle["rss_kb"],
        "pss_before_idle_kb": before_idle["pss_kb"],
        "rss_after_idle_kb": after_idle["rss_kb"],
        "pss_after_idle_kb": after_idle["pss_kb"],
        "idle_cpu_window_seconds": idle_seconds,
    }


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def backend_source_digest() -> tuple[str, int]:
    backend_root = ROOT / "backend"
    paths = sorted(
        path for path in backend_root.rglob("*")
        if path.is_file() and not path.is_symlink() and (path.suffix == ".go" or path.name in {"go.mod", "go.sum"})
    )
    digest = hashlib.sha256()
    for path in paths:
        relative = path.relative_to(ROOT).as_posix().encode("utf-8")
        digest.update(relative + b"\0")
        with path.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        digest.update(b"\0")
    return digest.hexdigest(), len(paths)


def git_head() -> str | None:
    try:
        result = subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True, capture_output=True, timeout=3, check=False
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    return result.stdout.strip() if result.returncode == 0 else None


def cpu_model() -> str | None:
    try:
        with Path("/proc/cpuinfo").open(encoding="ascii", errors="replace") as cpuinfo:
            for line in cpuinfo:
                if line.lower().startswith(("model name", "hardware")) and ":" in line:
                    return line.split(":", 1)[1].strip()
    except OSError:
        return None
    return None


def environment_info() -> dict:
    try:
        go = subprocess.run(["go", "version"], text=True, capture_output=True, timeout=3, check=False)
        go_version = go.stdout.strip() if go.returncode == 0 else None
    except (OSError, subprocess.TimeoutExpired):
        go_version = None
    try:
        page_size = os.sysconf("SC_PAGE_SIZE")
    except (OSError, ValueError):
        page_size = None
    return {
        "os": platform.system(),
        "platform": platform.platform(),
        "kernel": platform.release(),
        "architecture": platform.machine(),
        "cpu_model": cpu_model(),
        "logical_cpu_count": os.cpu_count(),
        "page_size_bytes": page_size,
        "python": sys.version.split()[0],
        "sqlite": sqlite3.sqlite_version,
        "go_toolchain": go_version,
        "procfs": Path("/proc/self/smaps_rollup").exists(),
    }


def source_hashes() -> dict:
    backend_digest, backend_file_count = backend_source_digest()
    return {
        "git_head": git_head(),
        "backend_source_tree_sha256": backend_digest,
        "backend_source_file_count": backend_file_count,
        "benchmark_script_sha256": sha256_file(Path(__file__).resolve()),
        "daemon_binary_sha256": sha256_file(DAEMON_BINARY),
        "performance_proposal_sha256": sha256_file(PERFORMANCE_PROPOSAL)
        if PERFORMANCE_PROPOSAL.is_file()
        else None,
        "hash_scope": "backend/**/*.go plus backend/go.mod and backend/go.sum; daemon binary; benchmark script; proposal",
    }


def run_benchmark(args: argparse.Namespace) -> dict:
    if platform.system() != "Linux" or not Path("/proc/self/smaps_rollup").is_file():
        raise RuntimeError("this benchmark requires Linux /proc, including smaps_rollup")
    if not DAEMON_BINARY.is_file() or not os.access(DAEMON_BINARY, os.X_OK):
        raise RuntimeError(f"existing daemon binary is required; build was not run: {DAEMON_BINARY}")
    hashes = source_hashes()
    spec = dataset_spec(args.case)
    tmp_root = Path(tempfile.gettempdir()).resolve()
    startup_samples: list[float] = []
    rpc_latencies: dict[str, list[float]] = {name: [] for name, _, _ in rpc_probes(spec)}
    response_sizes: dict[str, list[int]] = {name: [] for name, _, _ in rpc_probes(spec)}
    idle_cpu_samples: list[float] = []
    memory_samples: dict[str, list[int]] = {
        "rss_before_idle_kb": [],
        "pss_before_idle_kb": [],
        "rss_after_idle_kb": [],
        "pss_after_idle_kb": [],
    }
    run_records: list[dict] = []

    with tempfile.TemporaryDirectory(prefix="wfbb-", dir=tmp_root) as temp_name:
        workspace = Path(temp_name)
        profile_dir = workspace / "p"
        socket_path = workspace / "s"
        print("Initializing a private demo profile for the benchmark fixture.", file=sys.stderr, flush=True)
        bootstrap, bootstrap_log, bootstrap_status, _ = start_daemon(profile_dir, socket_path, workspace, True)
        if bootstrap_status.get("mode") != "demo":
            stop_daemon(bootstrap, bootstrap_log)
            raise RuntimeError("bootstrap daemon was not in demo mode")
        bootstrap_code, bootstrap_diagnostic = stop_daemon(bootstrap, bootstrap_log)
        if bootstrap_code != 0:
            raise RuntimeError(f"bootstrap daemon did not stop cleanly: {bootstrap_diagnostic}")
        if bootstrap.poll() is None:
            raise RuntimeError("bootstrap daemon must be stopped before synthetic data insertion")
        dataset_counts = seed_synthetic_dataset(profile_dir, spec)
        print(
            f"Prepared {dataset_counts['chats']} synthetic chats / {dataset_counts['messages']} synthetic messages.",
            file=sys.stderr,
            flush=True,
        )

        for run_index in range(args.runs):
            print(f"Measured daemon startup {run_index + 1}/{args.runs}.", file=sys.stderr, flush=True)
            process, log_path, status, ready_ms = start_daemon(profile_dir, socket_path, workspace, False)
            startup_samples.append(ready_ms)
            try:
                if status.get("mode") != "demo" or status.get("state") != "demo":
                    raise RuntimeError("measured daemon readiness did not report demo mode")
                one_run = measure_rpc_run(
                    process, socket_path, spec, args.rpc_samples, args.idle_seconds, run_index
                )
            finally:
                exit_code, diagnostic = stop_daemon(process, log_path)
            if exit_code != 0:
                raise RuntimeError(f"measured daemon exited with code {exit_code}: {diagnostic}")
            for name, values in one_run["rpc_latency_ms"].items():
                rpc_latencies[name].extend(values)
            for name, values in one_run["rpc_response_bytes"].items():
                response_sizes[name].extend(values)
            idle_cpu_samples.append(one_run["idle_cpu_percent_one_logical_core"])
            for key in memory_samples:
                memory_samples[key].append(one_run[key])
            run_records.append({"run": run_index + 1, "startup_readiness_ms": round(ready_ms, 3), **one_run})

    rpc_measurements = {
        name: {
            "distribution_ms": distribution(values),
            "samples_ms": [round(value, 3) for value in values],
            "response_bytes_distribution": distribution([float(value) for value in response_sizes[name]]),
            "response_bytes_samples": response_sizes[name],
            "max_response_frame_bytes": MAX_RPC_FRAME_BYTES,
        }
        for name, values in rpc_latencies.items()
    }
    memory_measurements = {
        key: {"distribution_kb": distribution([float(value) for value in values]), "samples_kb": values}
        for key, values in memory_samples.items()
    }
    return {
        "schema_version": 1,
        "generated_at_utc": datetime.now(timezone.utc).isoformat(),
        "classification": "synthetic-only backend benchmark",
        "scope": {
            "backend_daemon_only": True,
            "gui_or_qt_rendering_measured": False,
            "whatsapp_web_comparison_performed": False,
            "network_used": False,
            "real_profile_or_account_used": False,
            "live_mode_available_in_cli": False,
        },
        "dataset": {
            "case": spec.name,
            "synthetic": True,
            "generator": "fixed counter formula; deterministic IDs, bodies, statuses and timestamps; no RNG",
            **dataset_counts,
            "busy_chat_id": "bench:chat-0000",
            "regular_chat_messages_each": (spec.message_count - spec.busy_chat_messages) // (spec.chat_count - 1),
            "busy_group_distribution": "small: uniform 100/chat; large: 50,050 in chat 0 and 50 in each other chat",
        },
        "configuration": {
            "daemon_runs": args.runs,
            "rpc_samples_per_probe_per_run": args.rpc_samples,
            "idle_cpu_window_seconds_per_run": args.idle_seconds,
            "rpc_timeout_seconds": RPC_TIMEOUT_SECONDS,
            "startup_timeout_seconds": STARTUP_TIMEOUT_SECONDS,
            "rpc_max_frame_bytes_including_newline": MAX_RPC_FRAME_BYTES,
            "percentile_method": "nearest rank",
            "temporary_profile": "new private temporary demo profile, deleted on exit",
        },
        "environment": environment_info(),
        "source_hashes": hashes,
        "measurements": {
            "daemon_startup_readiness_ms": {
                "distribution": distribution(startup_samples),
                "samples": [round(value, 3) for value in startup_samples],
                "definition": "process launch to first successful app.status RPC reporting demo state; measured after fixture seeding",
            },
            "rpc_round_trip": rpc_measurements,
            "daemon_memory": memory_measurements,
            "daemon_idle_cpu_percent_single_logical_core": {
                "distribution": distribution(idle_cpu_samples),
                "samples": idle_cpu_samples,
                "definition": "delta of daemon /proc utime+stime ticks during an RPC-free wall-clock window; 100% equals one logical core",
            },
            "per_run": run_records,
        },
        "interpretation": {
            "comparison": "backend-only; excludes the Qt desktop process, GUI startup/rendering, and WhatsApp Web/browser",
            "cold_start": "not measured; repeated runs share the OS page cache and the same temporary seeded database",
            "acceptance": "not scored; this evidence does not establish full performance acceptance or proposed GUI/Web targets",
        },
    }


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        report = run_benchmark(args)
    except (OSError, RuntimeError, ValueError, sqlite3.Error, subprocess.SubprocessError) as exc:
        print(f"benchmark_backend: {exc}", file=sys.stderr)
        return 1
    json.dump(report, sys.stdout, ensure_ascii=False, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
