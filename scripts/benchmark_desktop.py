#!/usr/bin/env python3
"""Measure real Qt desktop startup and owned-process memory with demo-only data."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import platform
import shutil
import signal
import socket
import statistics
import stat
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping, Sequence

ROOT = Path(__file__).resolve().parents[1]
APP_BINARY = ROOT / "build" / "bin" / "whatsfeather"
DAEMON_BINARY = ROOT / "build" / "bin" / "whatsfeatherd"
EXPECTED_WINDOW_TITLE = "WhatsFeather — Demo"
EXPECTED_DEMO_CHAT_IDS = {
    "demo:alex",
    "demo:maya",
    "demo:studio",
    "demo:notes",
}
MAX_RPC_FRAME_BYTES = 262_144
RPC_TIMEOUT_SECONDS = 2.0
STARTUP_TIMEOUT_SECONDS = 30
CLOSE_TIMEOUT_SECONDS = 8
POLL_INTERVAL_MS = 25


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
            "Measure the real native Qt application and its owned Go daemon using a fresh, "
            "synthetic demo profile for every run."
        ),
        epilog=(
            "No live mode, caller-selected profile/socket, arbitrary quit timer, browser, "
            "GPU/scroll benchmark, or WhatsApp Web comparison is available. Requires an "
            "X11/XWayland display and xdotool to observe the app's own mapped window."
        ),
    )
    parser.add_argument("--runs", type=_bounded_int(1, 20), default=5)
    parser.add_argument("--startup-timeout", type=_bounded_int(3, 120), default=STARTUP_TIMEOUT_SECONDS)
    parser.add_argument("--close-timeout", type=_bounded_int(1, 30), default=CLOSE_TIMEOUT_SECONDS)
    parser.add_argument("--poll-interval-ms", type=_bounded_int(5, 250), default=POLL_INTERVAL_MS)
    return parser.parse_args(argv)


def parse_proc_stat(text: str) -> dict[str, int | str]:
    left = text.find("(")
    right = text.rfind(")")
    if left <= 0 or right <= left:
        raise ValueError("invalid /proc stat command field")
    try:
        pid = int(text[:left].strip())
        comm = text[left + 1 : right]
    except ValueError as exc:
        raise ValueError("invalid /proc stat pid") from exc
    fields = text[right + 1 :].split()
    if pid <= 0 or len(fields) <= 12:
        raise ValueError("/proc stat is missing process identity or CPU fields")
    try:
        ppid = int(fields[1])
        pgrp = int(fields[2])
        cpu_ticks = int(fields[11]) + int(fields[12])
    except ValueError as exc:
        raise ValueError("/proc stat contains invalid numeric fields") from exc
    if ppid < 0 or pgrp < 0 or cpu_ticks < 0:
        raise ValueError("/proc stat contains negative identity or CPU fields")
    return {"pid": pid, "comm": comm, "state": fields[0], "ppid": ppid, "pgrp": pgrp, "cpu_ticks": cpu_ticks}


def _proc_field_kb(text: str, field: str) -> int:
    for line in text.splitlines():
        parts = line.split()
        if parts and parts[0].rstrip(":") == field:
            if len(parts) != 3 or parts[2] != "kB":
                break
            try:
                value = int(parts[1])
            except ValueError as exc:
                raise ValueError(f"invalid /proc memory value for {field}") from exc
            if value < 0:
                raise ValueError(f"negative /proc memory value for {field}")
            return value
    raise ValueError(f"/proc data is missing or has malformed {field}")


def parse_proc_memory_kb(status_text: str, smaps_rollup_text: str) -> dict[str, int]:
    return {
        "rss_kb": _proc_field_kb(status_text, "VmRSS"),
        "pss_kb": _proc_field_kb(smaps_rollup_text, "Pss"),
    }


def _proc_uid(status_text: str) -> int:
    for line in status_text.splitlines():
        if line.startswith("Uid:"):
            fields = line.split()[1:]
            if len(fields) != 4:
                break
            try:
                uid = int(fields[0])
            except ValueError as exc:
                raise ValueError("invalid /proc uid") from exc
            if uid < 0:
                break
            return uid
    raise ValueError("/proc status is missing a valid Uid field")


def parse_child_pids(text: str) -> list[int]:
    if not text.strip():
        return []
    values: list[int] = []
    for token in text.split():
        if not token.isascii() or not token.isdecimal():
            raise ValueError("invalid child PID list")
        pid = int(token)
        if pid <= 1 or pid in values:
            raise ValueError("child PID list contains a nonpositive or duplicate PID")
        values.append(pid)
    return values


def parse_window_ids(text: str) -> list[int]:
    if not text.strip():
        return []
    window_ids: list[int] = []
    for token in text.split():
        if not token.isascii() or not token.isdecimal():
            raise ValueError("xdotool returned an invalid window ID")
        window_id = int(token)
        if window_id <= 0 or window_id in window_ids:
            raise ValueError("xdotool returned a nonpositive or duplicate window ID")
        window_ids.append(window_id)
    return window_ids


def read_process_identity(
    pid: int,
    expected_exe: Path,
    expected_parent: int | None,
    expected_pgrp: int,
    expected_uid: int | None = None,
    proc_root: Path = Path("/proc"),
) -> dict[str, int | str]:
    """Read identity only for a PID already obtained from this run's Popen/children file."""
    if type(pid) is not int or pid <= 1:
        raise RuntimeError("refusing an invalid benchmark-owned PID")
    base = Path(proc_root) / str(pid)
    try:
        stat_data = parse_proc_stat((base / "stat").read_text(encoding="ascii"))
        status_text = (base / "status").read_text(encoding="ascii", errors="replace")
        actual_exe_text = os.readlink(base / "exe")
        actual_uid = _proc_uid(status_text)
        actual_exe = Path(actual_exe_text).resolve(strict=True)
        target_exe = Path(expected_exe).resolve(strict=True)
    except (OSError, ValueError) as exc:
        raise RuntimeError(f"cannot verify benchmark-owned process {pid}: {exc}") from exc
    if stat_data["pid"] != pid:
        raise RuntimeError(f"/proc identity changed while inspecting benchmark PID {pid}")
    if actual_exe != target_exe:
        raise RuntimeError(f"benchmark PID {pid} is not the expected executable")
    if expected_parent is not None and stat_data["ppid"] != expected_parent:
        raise RuntimeError(f"benchmark PID {pid} is not a direct child of its expected owner")
    if stat_data["pgrp"] != expected_pgrp:
        raise RuntimeError(f"benchmark PID {pid} escaped its private process group")
    if expected_uid is not None and actual_uid != expected_uid:
        raise RuntimeError(f"benchmark PID {pid} is not owned by the current user")
    return {**stat_data, "exe": str(actual_exe), "uid": actual_uid}


def read_direct_children(parent_pid: int, proc_root: Path = Path("/proc")) -> list[int]:
    if type(parent_pid) is not int or parent_pid <= 1:
        raise RuntimeError("refusing to inspect children of an invalid PID")
    path = Path(proc_root) / str(parent_pid) / "task" / str(parent_pid) / "children"
    try:
        return parse_child_pids(path.read_text(encoding="ascii"))
    except (OSError, ValueError) as exc:
        raise RuntimeError(f"cannot read direct children of benchmark PID {parent_pid}: {exc}") from exc


def read_process_cmdline(pid: int, proc_root: Path = Path("/proc")) -> list[str]:
    if type(pid) is not int or pid <= 1:
        raise RuntimeError("refusing to read command line for an invalid PID")
    try:
        raw = (Path(proc_root) / str(pid) / "cmdline").read_bytes()
    except OSError as exc:
        raise RuntimeError(f"cannot read command line for benchmark-owned PID {pid}: {exc}") from exc
    if not raw or not raw.endswith(b"\0"):
        raise RuntimeError(f"benchmark-owned PID {pid} has an invalid command line")
    return [os.fsdecode(part) for part in raw[:-1].split(b"\0")]


def validate_daemon_argv(
    argv: list[str], expected_binary: Path, expected_socket: Path, expected_data_dir: Path
) -> bool:
    expected = [
        str(Path(expected_binary).absolute()),
        "--demo",
        "--socket",
        str(Path(expected_socket).absolute()),
        "--data-dir",
        str(Path(expected_data_dir).absolute()),
    ]
    normalized = list(argv)
    if normalized:
        normalized[0] = str(Path(normalized[0]).absolute())
    if normalized != expected:
        raise RuntimeError("owned daemon arguments differ from the exact synthetic demo/socket/profile contract")
    if "--live" in normalized:
        raise RuntimeError("refusing a non-demo owned daemon")
    return True


def parse_rpc_frame(frame: bytes, request_id: str) -> dict[str, Any]:
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
    result = response.get("result")
    if not isinstance(result, dict):
        raise ValueError("RPC response result is not an object")
    return result


def rpc_call(
    socket_path: Path,
    method: str,
    request_id: str,
    timeout: float = RPC_TIMEOUT_SECONDS,
) -> tuple[dict[str, Any], float, int]:
    request = {"v": 1, "id": request_id, "method": method, "params": {}}
    encoded = json.dumps(request, separators=(",", ":")).encode("utf-8") + b"\n"
    if len(encoded) > MAX_RPC_FRAME_BYTES:
        raise ValueError("RPC request exceeds the frame limit")
    started = time.perf_counter()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(timeout)
        connection.connect(str(socket_path))
        connection.sendall(encoded)
        with connection.makefile("rb") as stream:
            frame = stream.readline(MAX_RPC_FRAME_BYTES + 1)
    response = parse_rpc_frame(frame, request_id)
    return response, (time.perf_counter() - started) * 1000.0, len(frame)


def distribution(values: Sequence[float | int]) -> dict[str, int | float]:
    if not values:
        raise ValueError("distribution requires at least one sample")
    ordered = sorted(float(value) for value in values)

    def rank(percent: float) -> float:
        return ordered[max(0, math.ceil(percent * len(ordered)) - 1)]

    return {
        "count": len(ordered),
        "min": round(ordered[0], 3),
        "max": round(ordered[-1], 3),
        "mean": round(statistics.fmean(ordered), 3),
        "p50": round(rank(0.50), 3),
        "p95": round(rank(0.95), 3),
        "p99": round(rank(0.99), 3),
    }


def prepare_isolated_environment(root: Path, base_environment: Mapping[str, str] | None = None) -> dict[str, str]:
    root = Path(root)
    try:
        root_info = root.lstat()
    except OSError as exc:
        raise RuntimeError(f"private run directory is unavailable: {exc}") from exc
    if not stat.S_ISDIR(root_info.st_mode) or stat.S_ISLNK(root_info.st_mode):
        raise RuntimeError("benchmark run root must be a real directory")
    if root_info.st_uid != os.getuid() or root_info.st_mode & 0o077:
        raise RuntimeError("benchmark run root must be owner-only and owned by this user")

    paths = {
        "XDG_DATA_HOME": root / "data",
        "XDG_CONFIG_HOME": root / "config",
        "XDG_CACHE_HOME": root / "cache",
        "XDG_STATE_HOME": root / "state",
        "XDG_RUNTIME_DIR": root / "runtime",
    }
    for path in paths.values():
        path.mkdir(mode=0o700)
        os.chmod(path, 0o700)
    cache = paths["XDG_CACHE_HOME"]
    qml_cache = cache / "qml"
    shader_cache = cache / "shader"
    qml_cache.mkdir(mode=0o700)
    shader_cache.mkdir(mode=0o700)

    environment = dict(os.environ if base_environment is None else base_environment)
    environment.update({key: str(value) for key, value in paths.items()})
    environment["QT_QPA_PLATFORM"] = "xcb"
    environment["QML_DISK_CACHE_PATH"] = str(qml_cache)
    environment["QT_SHADER_CACHE_DIR"] = str(shader_cache)
    environment["WHATSFEATHER_BACKEND"] = str(DAEMON_BINARY.resolve())
    environment.pop("QML_DISABLE_DISK_CACHE", None)
    return environment


def _run_xdotool(arguments: list[str], timeout: float = 1.5) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            ["xdotool", *arguments],
            text=True,
            capture_output=True,
            timeout=timeout,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise RuntimeError(f"xdotool readiness probe failed: {exc}") from exc


def find_visible_window(pid: int) -> dict[str, int | str] | None:
    result = _run_xdotool(["search", "--onlyvisible", "--pid", str(pid)])
    if result.returncode != 0 and not result.stdout.strip():
        if "Can't open display" in result.stderr or "not found" in result.stderr.lower():
            raise RuntimeError(f"xdotool cannot inspect the current display: {result.stderr.strip()}")
        return None
    ids = parse_window_ids(result.stdout)
    for window_id in ids:
        name_result = _run_xdotool(["getwindowname", str(window_id)])
        if name_result.returncode == 0:
            title = name_result.stdout.rstrip("\r\n")
            if title == EXPECTED_WINDOW_TITLE:
                return {"window_id": window_id, "title": title, "pid": pid, "mapped_viewable": True}
    return None


def _validate_demo_status(status: dict[str, Any]) -> None:
    if status.get("mode") != "demo" or status.get("state") != "demo":
        raise RuntimeError("owned daemon readiness did not report the expected demo mode/state")


def _validate_demo_chats(result: dict[str, Any]) -> list[str]:
    chats = result.get("chats")
    if not isinstance(chats, list):
        raise RuntimeError("demo chat.list readiness response has no chats array")
    ids: list[str] = []
    for chat in chats:
        if not isinstance(chat, dict) or not isinstance(chat.get("id"), str):
            raise RuntimeError("demo chat.list readiness response contains an invalid chat")
        ids.append(chat["id"])
    if len(ids) != len(EXPECTED_DEMO_CHAT_IDS) or set(ids) != EXPECTED_DEMO_CHAT_IDS:
        raise RuntimeError("demo chat.list did not return the untouched four-chat synthetic fixture")
    return ids


def _verify_executable(binary: Path, label: str) -> Path:
    try:
        info = Path(binary).lstat()
        resolved = Path(binary).resolve(strict=True)
    except OSError as exc:
        raise RuntimeError(f"existing {label} binary is required; no build was run: {binary}: {exc}") from exc
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or not os.access(resolved, os.X_OK):
        raise RuntimeError(f"existing {label} binary must be a regular executable: {binary}")
    return resolved


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _tree_digest(paths: list[Path], include_suffixes: set[str], include_names: set[str] | None = None) -> tuple[str, int]:
    include_names = include_names or set()
    files: set[Path] = set()
    for root in paths:
        if root.is_file() and not root.is_symlink():
            if root.suffix in include_suffixes or root.name in include_names:
                files.add(root)
        elif root.is_dir():
            for path in root.rglob("*"):
                if path.is_file() and not path.is_symlink() and (path.suffix in include_suffixes or path.name in include_names):
                    files.add(path)
    digest = hashlib.sha256()
    for path in sorted(files):
        digest.update(path.relative_to(ROOT).as_posix().encode("utf-8") + b"\0")
        with path.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        digest.update(b"\0")
    return digest.hexdigest(), len(files)


def git_head() -> str | None:
    try:
        result = subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True, capture_output=True, timeout=3, check=False
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    return result.stdout.strip() if result.returncode == 0 else None


def source_hashes(app_binary: Path, daemon_binary: Path) -> dict[str, Any]:
    desktop_digest, desktop_count = _tree_digest(
        [ROOT / "desktop" / "src", ROOT / "desktop" / "qml", ROOT / "desktop" / "icons", ROOT / "desktop" / "assets", ROOT / "desktop" / "packaging", ROOT / "CMakeLists.txt"],
        {".cpp", ".h", ".qml", ".svg", ".png", ".webp", ".desktop"},
        {"CMakeLists.txt"},
    )
    backend_digest, backend_count = _tree_digest(
        [ROOT / "backend"], {".go"}, {"go.mod", "go.sum"}
    )
    return {
        "git_head": git_head(),
        "desktop_source_tree_sha256": desktop_digest,
        "desktop_source_file_count": desktop_count,
        "backend_source_tree_sha256": backend_digest,
        "backend_source_file_count": backend_count,
        "benchmark_script_sha256": sha256_file(Path(__file__).resolve()),
        "benchmark_test_sha256": sha256_file(ROOT / "tests" / "test_benchmark_desktop.py"),
        "app_binary_sha256": sha256_file(app_binary),
        "daemon_binary_sha256": sha256_file(daemon_binary),
        "hash_scope": (
            "desktop C++/headers, QML, SVG and bundled image/desktop assets, CMakeLists.txt; "
            "backend Go sources plus go.mod/go.sum; benchmark script and tests; exact executable binaries"
        ),
        "source_snapshot": "current working-tree file contents, including untracked implementation; not inferred from git HEAD",
    }


def _version(command: list[str]) -> str | None:
    try:
        result = subprocess.run(command, text=True, capture_output=True, timeout=3, check=False)
    except (OSError, subprocess.TimeoutExpired):
        return None
    if result.returncode != 0:
        return None
    return (result.stdout or result.stderr).strip().splitlines()[0] if (result.stdout or result.stderr).strip() else None


def desktop_environment() -> dict[str, Any]:
    if not os.environ.get("DISPLAY"):
        raise RuntimeError("a real X11/XWayland DISPLAY is required; offscreen mode is not a desktop benchmark")
    if not shutil.which("xdotool"):
        raise RuntimeError("xdotool is required to observe only the benchmark app's own mapped window")
    if not shutil.which("wmctrl"):
        raise RuntimeError("wmctrl is required to send a graceful close to only the benchmark app's exact window ID")
    geometry_result = _run_xdotool(["getdisplaygeometry"])
    try:
        dimensions = [int(part) for part in geometry_result.stdout.split()]
    except ValueError as exc:
        raise RuntimeError("xdotool could not read display geometry") from exc
    if geometry_result.returncode != 0 or len(dimensions) != 2 or min(dimensions) <= 0:
        raise RuntimeError("xdotool could not read display geometry")
    return {
        "os": platform.system(),
        "platform": platform.platform(),
        "kernel": platform.release(),
        "architecture": platform.machine(),
        "cpu_model": platform.processor() or None,
        "logical_cpu_count": os.cpu_count(),
        "python": sys.version.split()[0],
        "libc": platform.libc_ver(),
        "qt_version": _version(["qmake6", "-query", "QT_VERSION"]),
        "xdotool_version": _version(["xdotool", "--version"]),
        "wmctrl_version": _version(["wmctrl", "-h"]),
        "qt_qpa_platform": "xcb",
        "desktop_session_type": os.environ.get("XDG_SESSION_TYPE") or None,
        "x11_display_available": True,
        "display_geometry_px": {"width": dimensions[0], "height": dimensions[1]},
        "render_scale_environment": {
            key: os.environ[key]
            for key in ("QT_SCALE_FACTOR", "QT_SCREEN_SCALE_FACTORS", "QT_AUTO_SCREEN_SCALE_FACTOR", "QT_STYLE_OVERRIDE")
            if key in os.environ
        },
        "fresh_xdg_directories_per_run": True,
        "os_page_cache_flushed": False,
    }


def _read_memory_snapshot(
    pid: int,
    expected_exe: Path,
    expected_parent: int,
    expected_pgrp: int,
    expected_uid: int,
) -> dict[str, int]:
    read_process_identity(pid, expected_exe, expected_parent, expected_pgrp, expected_uid)
    base = Path("/proc") / str(pid)
    try:
        status_text = (base / "status").read_text(encoding="ascii", errors="replace")
        smaps_text = (base / "smaps_rollup").read_text(encoding="ascii", errors="replace")
    except OSError as exc:
        raise RuntimeError(f"cannot read PSS/RSS for benchmark-owned PID {pid}: {exc}") from exc
    memory = parse_proc_memory_kb(status_text, smaps_text)
    read_process_identity(pid, expected_exe, expected_parent, expected_pgrp, expected_uid)
    return memory


def _read_log(log_path: Path) -> str:
    try:
        with Path(log_path).open("rb") as stream:
            data = stream.read(16_384)
    except OSError:
        return ""
    return data.decode("utf-8", errors="replace").strip()


def _diagnostic(process: subprocess.Popen, log_path: Path) -> str:
    return f"exit={process.poll()}; stderr={_read_log(log_path)}"


def _validate_child(
    app_pid: int,
    child_pid: int,
    socket_path: Path,
    data_dir: Path,
    app_group: int,
) -> list[str]:
    read_process_identity(child_pid, DAEMON_BINARY, app_pid, app_group, os.getuid())
    argv = read_process_cmdline(child_pid)
    validate_daemon_argv(argv, DAEMON_BINARY, socket_path, data_dir)
    return argv


def wait_for_desktop_ready(
    process: subprocess.Popen,
    profile_dir: Path,
    runtime_dir: Path,
    timeout_seconds: int,
    poll_interval_ms: int,
    state: dict[str, Any],
) -> tuple[float, dict[str, Any]]:
    started = state["launch_started"]
    deadline = started + timeout_seconds
    app_pid = process.pid
    read_process_identity(app_pid, APP_BINARY, os.getpid(), app_pid, os.getuid())
    data_dir = Path(profile_dir).absolute()
    runtime_root = Path(runtime_dir) / "whatsfeather"
    last_transport_error: str | None = None

    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"Qt desktop process exited before readiness ({_diagnostic(process, state['log_path'])})")
        children = read_direct_children(app_pid)
        if len(children) > 1:
            raise RuntimeError("Qt desktop process spawned unexpected direct children; refusing unsafe measurement")
        child_pid = children[0] if children else None
        if child_pid is not None:
            state["daemon_pid"] = child_pid
            # The exact socket/profile command line is the boundary that keeps the
            # app's backend attached to this run's fresh demo state only.
            try:
                argv = read_process_cmdline(child_pid)
                if len(argv) < 4 or "--socket" not in argv:
                    raise RuntimeError("owned daemon did not expose the expected local socket argument")
                socket_index = argv.index("--socket")
                if socket_index + 1 >= len(argv):
                    raise RuntimeError("owned daemon socket argument is incomplete")
                socket_path = Path(argv[socket_index + 1]).absolute()
                if socket_path.parent != runtime_root or len(os.fsencode(socket_path)) >= 100:
                    raise RuntimeError("owned daemon socket is outside this private short XDG runtime directory")
                _validate_child(app_pid, child_pid, socket_path, data_dir, app_pid)
            except RuntimeError:
                raise
            window = find_visible_window(app_pid)
            if window is not None:
                state["window_id"] = int(window["window_id"])
                try:
                    status, _, _ = rpc_call(socket_path, "app.status", f"desktop-ready-status-{app_pid}")
                    _validate_demo_status(status)
                    chats, _, _ = rpc_call(socket_path, "chat.list", f"desktop-ready-chats-{app_pid}")
                    chat_ids = _validate_demo_chats(chats)
                except OSError as exc:
                    last_transport_error = str(exc)
                else:
                    elapsed_ms = (time.perf_counter() - started) * 1000.0
                    evidence = {
                        "window": window,
                        "window_readiness": "xdotool --onlyvisible --pid found the benchmark PID's exact WhatsFeather — Demo top-level window",
                        "owned_daemon_pid": child_pid,
                        "owned_daemon_executable_verified": True,
                        "owned_daemon_arguments_verified": ["--demo", "--socket <fresh-private-XDG-runtime>", "--data-dir <fresh-XDG-data-home>/whatsfeather/demo"],
                        "daemon_app_status": {"mode": status.get("mode"), "state": status.get("state")},
                        "daemon_chat_list_count": len(chat_ids),
                        "daemon_chat_ids": chat_ids,
                        "frontend_model_consumption_observed": False,
                    }
                    return elapsed_ms, evidence
        time.sleep(poll_interval_ms / 1000.0)
    raise RuntimeError(
        f"native desktop did not satisfy mapped-window plus synthetic daemon RPC readiness within {timeout_seconds}s; "
        f"last local RPC error={last_transport_error!r}; {_diagnostic(process, state['log_path'])}"
    )


def _signal_owned_group(process: subprocess.Popen, daemon_pid: int | None, sig: int) -> None:
    if process.poll() is not None:
        return
    read_process_identity(process.pid, APP_BINARY, os.getpid(), process.pid, os.getuid())
    if daemon_pid is not None:
        try:
            read_process_identity(daemon_pid, DAEMON_BINARY, process.pid, process.pid, os.getuid())
        except RuntimeError:
            # It may have already exited. The isolated session still contains
            # only this launched app and descendants; never search/signal PIDs globally.
            pass
    try:
        os.killpg(process.pid, sig)
    except ProcessLookupError:
        pass


def _owned_daemon_alive(daemon_pid: int, app_pgid: int) -> bool:
    try:
        identity = read_process_identity(
            daemon_pid,
            DAEMON_BINARY,
            expected_parent=None,
            expected_pgrp=app_pgid,
            expected_uid=os.getuid(),
        )
    except RuntimeError:
        return False
    return identity.get("state") != "Z"


def _stop_owned_daemon(daemon_pid: int, app_pgid: int) -> None:
    if not _owned_daemon_alive(daemon_pid, app_pgid):
        return
    identity = read_process_identity(
        daemon_pid, DAEMON_BINARY, expected_parent=None, expected_pgrp=app_pgid, expected_uid=os.getuid()
    )
    if identity.get("state") == "Z":
        return
    os.kill(daemon_pid, signal.SIGTERM)
    deadline = time.monotonic() + 3.0
    while time.monotonic() < deadline and _owned_daemon_alive(daemon_pid, app_pgid):
        time.sleep(0.025)
    if _owned_daemon_alive(daemon_pid, app_pgid):
        # Revalidate exact executable, owner and private process group immediately
        # before escalation; do not kill a reused or unrelated PID.
        read_process_identity(
            daemon_pid, DAEMON_BINARY, expected_parent=None, expected_pgrp=app_pgid, expected_uid=os.getuid()
        )
        os.kill(daemon_pid, signal.SIGKILL)
        deadline = time.monotonic() + 2.0
        while time.monotonic() < deadline and _owned_daemon_alive(daemon_pid, app_pgid):
            time.sleep(0.025)
    if _owned_daemon_alive(daemon_pid, app_pgid):
        raise RuntimeError("owned demo daemon survived bounded cleanup")


def close_and_verify_app(
    process: subprocess.Popen,
    window_id: int | None,
    daemon_pid: int | None,
    close_timeout: int,
    log_path: Path,
) -> int:
    if process.poll() is None and window_id is not None:
        try:
            result = subprocess.run(
                ["wmctrl", "-i", "-c", str(window_id)],
                text=True,
                capture_output=True,
                timeout=2.0,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise RuntimeError(f"could not request graceful close of the benchmark app window: {exc}") from exc
        if result.returncode != 0:
            raise RuntimeError(f"window manager rejected graceful close of the benchmark app window: {result.stderr.strip()}")
    try:
        exit_code = process.wait(timeout=close_timeout)
    except subprocess.TimeoutExpired:
        _signal_owned_group(process, daemon_pid, signal.SIGTERM)
        try:
            exit_code = process.wait(timeout=4.0)
        except subprocess.TimeoutExpired:
            _signal_owned_group(process, daemon_pid, signal.SIGKILL)
            exit_code = process.wait(timeout=3.0)
    if daemon_pid is not None:
        _stop_owned_daemon(daemon_pid, process.pid)
    if exit_code != 0:
        raise RuntimeError(f"Qt desktop did not close cleanly ({_diagnostic(process, log_path)})")
    return exit_code


def measure_process_memory(app_pid: int, daemon_pid: int) -> dict[str, Any]:
    started = time.perf_counter()
    app_memory = _read_memory_snapshot(app_pid, APP_BINARY, os.getpid(), app_pid, os.getuid())
    daemon_memory = _read_memory_snapshot(daemon_pid, DAEMON_BINARY, app_pid, app_pid, os.getuid())
    duration_ms = (time.perf_counter() - started) * 1000.0
    return {
        "aggregate_pss_kb": app_memory["pss_kb"] + daemon_memory["pss_kb"],
        "aggregate_rss_kb": app_memory["rss_kb"] + daemon_memory["rss_kb"],
        "snapshot_duration_ms": round(duration_ms, 3),
        "processes": [
            {"role": "qt_desktop_app", "pid": app_pid, **app_memory},
            {"role": "owned_demo_daemon", "pid": daemon_pid, **daemon_memory},
        ],
        "rss_sum_definition": "sum of per-process VmRSS; shared mapped pages are counted once per process",
        "pss_sum_definition": "sum of per-process smaps_rollup PSS; shared pages are proportionally attributed across all system mappings",
    }


def run_one(
    run_index: int,
    args: argparse.Namespace,
    temp_root: Path,
) -> dict[str, Any]:
    with tempfile.TemporaryDirectory(prefix="wfd-", dir=temp_root) as temp_name:
        workspace = Path(temp_name)
        os.chmod(workspace, 0o700)
        environment = prepare_isolated_environment(workspace, os.environ)
        profile_dir = Path(environment["XDG_DATA_HOME"]) / "whatsfeather" / "demo"
        runtime_dir = Path(environment["XDG_RUNTIME_DIR"])
        log_path = workspace / "app.stderr"
        log_fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        state: dict[str, Any] = {"log_path": log_path, "daemon_pid": None, "window_id": None}
        process: subprocess.Popen | None = None
        try:
            with os.fdopen(log_fd, "wb") as log_file:
                started = time.monotonic()
                process = subprocess.Popen(
                    [str(APP_BINARY), "--demo", "--theme", "dark"],
                    cwd=ROOT,
                    env=environment,
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.DEVNULL,
                    stderr=log_file,
                    close_fds=True,
                    start_new_session=True,
                )
                state["launch_started"] = started
            read_process_identity(process.pid, APP_BINARY, os.getpid(), process.pid, os.getuid())
            print(f"Measured native Qt startup {run_index + 1}/{args.runs}.", file=sys.stderr, flush=True)
            ready_ms, evidence = wait_for_desktop_ready(
                process,
                profile_dir,
                runtime_dir,
                args.startup_timeout,
                args.poll_interval_ms,
                state,
            )
            daemon_pid = int(state["daemon_pid"])
            memory = measure_process_memory(process.pid, daemon_pid)
            close_and_verify_app(process, state.get("window_id"), daemon_pid, args.close_timeout, log_path)
            return {
                "run": run_index + 1,
                "fresh_xdg_profile": True,
                "profile_removed_after_run": True,
                "startup_readiness_ms": round(ready_ms, 3),
                "readiness_evidence": evidence,
                "memory_at_readiness": memory,
                "app_exit_code": process.returncode,
                "shutdown": "requested window-manager close of the exact benchmark PID window; owned daemon exited or was boundedly cleaned",
            }
        except Exception:
            if process is not None and process.poll() is None:
                try:
                    _signal_owned_group(process, state.get("daemon_pid"), signal.SIGTERM)
                    process.wait(timeout=4.0)
                except Exception:
                    try:
                        if process.poll() is None:
                            _signal_owned_group(process, state.get("daemon_pid"), signal.SIGKILL)
                            process.wait(timeout=3.0)
                    except Exception:
                        pass
            if state.get("daemon_pid") is not None and process is not None:
                try:
                    _stop_owned_daemon(int(state["daemon_pid"]), process.pid)
                except Exception:
                    pass
            raise
        finally:
            if process is not None and process.poll() is None:
                try:
                    _signal_owned_group(process, state.get("daemon_pid"), signal.SIGKILL)
                    process.wait(timeout=3.0)
                except Exception:
                    pass


def _distribution_record(values: Sequence[int | float], unit: str) -> dict[str, Any]:
    return {"distribution": distribution(values), "samples": values, "unit": unit}


def run_benchmark(args: argparse.Namespace) -> dict[str, Any]:
    if platform.system() != "Linux":
        raise RuntimeError("desktop benchmark requires Linux /proc for PSS/RSS process snapshots")
    if not os.environ.get("TMPDIR"):
        raise RuntimeError("set TMPDIR to the private benchmark scratch directory before running")
    temp_root = Path(os.environ["TMPDIR"]).resolve(strict=True)
    root_info = temp_root.stat()
    if not temp_root.is_dir() or root_info.st_uid != os.getuid() or root_info.st_mode & 0o077:
        raise RuntimeError("TMPDIR must be a current-user-owned private directory (mode 0700)")
    app_binary = _verify_executable(APP_BINARY, "Qt desktop")
    daemon_binary = _verify_executable(DAEMON_BINARY, "Go daemon")
    if app_binary != APP_BINARY.resolve(strict=True) or daemon_binary != DAEMON_BINARY.resolve(strict=True):
        raise RuntimeError("unexpected executable resolution")
    display = desktop_environment()
    hashes = source_hashes(app_binary, daemon_binary)
    startup_samples: list[float] = []
    memory_values: dict[str, list[int]] = {
        "aggregate_pss_kb": [],
        "aggregate_rss_kb": [],
        "qt_desktop_app_pss_kb": [],
        "qt_desktop_app_rss_kb": [],
        "owned_demo_daemon_pss_kb": [],
        "owned_demo_daemon_rss_kb": [],
    }
    runs: list[dict[str, Any]] = []
    for run_index in range(args.runs):
        result = run_one(run_index, args, temp_root)
        startup_samples.append(result["startup_readiness_ms"])
        memory = result["memory_at_readiness"]
        memory_values["aggregate_pss_kb"].append(memory["aggregate_pss_kb"])
        memory_values["aggregate_rss_kb"].append(memory["aggregate_rss_kb"])
        app_memory = memory["processes"][0]
        daemon_memory = memory["processes"][1]
        memory_values["qt_desktop_app_pss_kb"].append(app_memory["pss_kb"])
        memory_values["qt_desktop_app_rss_kb"].append(app_memory["rss_kb"])
        memory_values["owned_demo_daemon_pss_kb"].append(daemon_memory["pss_kb"])
        memory_values["owned_demo_daemon_rss_kb"].append(daemon_memory["rss_kb"])
        runs.append(result)

    final_hashes = source_hashes(app_binary, daemon_binary)
    if hashes != final_hashes:
        raise RuntimeError("source or binary hashes changed during the benchmark; results are not reproducible")
    return {
        "schema_version": 1,
        "generated_at_utc": datetime.now(timezone.utc).isoformat(),
        "classification": "synthetic-only native Qt desktop startup and owned-process memory benchmark",
        "scope": {
            "real_qt_application_binary_launched": str(app_binary),
            "real_owned_go_daemon_binary_launched": str(daemon_binary),
            "fresh_demo_profile_each_run": True,
            "live_mode_or_account_used": False,
            "external_messages_or_account_actions": False,
            "network_scope": "launcher forces --demo; benchmark RPCs use only the per-run private AF_UNIX socket; network traffic is not system-call audited",
            "browser_or_whatsapp_web_comparison": False,
            "desktop_screenshots_or_captures_taken": False,
            "gpu_frame_scroll_or_rendering_performance_measured": False,
        },
        "configuration": {
            "runs": args.runs,
            "qt_platform": "xcb (X11/XWayland)",
            "theme": "dark, explicitly selected",
            "app_arguments": ["--demo", "--theme", "dark"],
            "app_quit_timer_used": False,
            "readiness_poll_interval_ms": args.poll_interval_ms,
            "startup_timeout_seconds": args.startup_timeout,
            "close_timeout_seconds": args.close_timeout,
            "rpc_timeout_seconds": RPC_TIMEOUT_SECONDS,
            "rpc_max_frame_bytes_including_newline": MAX_RPC_FRAME_BYTES,
            "percentile_method": "nearest rank",
            "temporary_root": str(temp_root),
            "profile_policy": "new owner-only XDG data/config/cache/state/runtime roots per run; removed after clean shutdown",
            "os_page_cache_flushed": False,
        },
        "environment": display,
        "source_hashes": hashes,
        "measurements": {
            "desktop_startup_readiness_ms": {
                "distribution": distribution(startup_samples),
                "samples": [round(value, 3) for value in startup_samples],
                "unit": "ms",
                "definition": (
                    "wall time from immediately before Popen of the Qt binary to the first successful probe where "
                    "the exact app PID owns an xdotool --onlyvisible window titled WhatsFeather — Demo, its one "
                    "direct child is the exact whatsfeatherd executable in --demo mode with this run's private "
                    "socket/profile arguments, app.status reports demo/demo, and chat.list returns the four default "
                    "synthetic demo chats; includes readiness polling/probe overhead"
                ),
                "does_not_claim": "the Qt frontend chat model consumed the response; it is not directly observable without native instrumentation",
                "readiness_evidence_per_run": [run["readiness_evidence"] for run in runs],
            },
            "process_memory_at_readiness": {
                name: _distribution_record(values, "kB") for name, values in memory_values.items()
            },
            "memory_sample_definition": (
                "immediately after the explicit desktop readiness probe; app VmRSS and smaps_rollup PSS are read "
                "from /proc for the benchmark-owned Qt process and its verified direct demo-daemon child only; "
                "PSS is proportional share, summed across these two PIDs; RSS sum double-counts shared pages"
            ),
            "per_run_raw_samples": runs,
        },
        "interpretation": {
            "startup_scope": "native application launch, QML-backed window mapping, and its own local synthetic demo daemon readiness",
            "cold_start": "not measured; each process/profile is fresh but OS page caches and desktop services are shared",
            "frontend_readiness_limit": "window mapping and daemon RPC readiness are explicit; frontend model consumption is not instrumented",
            "comparison": "no browser, WhatsApp Web, GPU, frame-rate, scroll, or cross-device comparison",
            "acceptance": "descriptive measurements only; no performance target is scored and no performance improvement is claimed",
        },
    }


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        report = run_benchmark(args)
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as exc:
        print(f"benchmark_desktop: {exc}", file=sys.stderr)
        return 1
    json.dump(report, sys.stdout, ensure_ascii=False, indent=2, sort_keys=True, allow_nan=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
