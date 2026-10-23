#!/usr/bin/env python3
"""Synthetic-only profile binding regressions for scoped live-test mode."""
from datetime import datetime, timedelta, timezone
from pathlib import Path
import os
import signal
import socket
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
DAEMON = ROOT / "build" / "bin" / "whatsfeatherd"
MARKER = ".live-test-scope"


def scope_file(path: Path, day_offset: int = 1) -> Path:
    since = (datetime.now(timezone.utc) - timedelta(days=day_offset)).replace(microsecond=0)
    path.write_text(
        '{"since":"' + since.isoformat().replace("+00:00", "Z")
        + '","chats":["15550123456@s.whatsapp.net"],"include_self":false}',
        encoding="utf-8",
    )
    path.chmod(0o600)
    return path


def launch(scope: Path | None, profile: Path, sock: Path, mode: str = "--live"):
    argv = [str(DAEMON), mode]
    if scope is not None:
        argv += ["--live-test-scope", str(scope)]
    argv += ["--socket", str(sock), "--data-dir", str(profile)]
    return subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)


def stop(process: subprocess.Popen):
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
    try:
        return process.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        return process.communicate(timeout=5)


def reject(process: subprocess.Popen):
    try:
        return process.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        stdout, stderr = process.communicate(timeout=5)
        raise AssertionError(f"expected preflight rejection but process stayed live: {stdout}{stderr}")


def wait_ready(process: subprocess.Popen, sock: Path):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline and process.poll() is None and not sock.exists():
        time.sleep(0.025)
    assert process.poll() is None and sock.exists(), "private synthetic daemon did not start"


if not DAEMON.is_file():
    raise SystemExit("build/bin/whatsfeatherd is missing; run scripts/build.sh first")

with tempfile.TemporaryDirectory(prefix="wf-scope-profile-") as temp:
    root = Path(temp)
    scope = scope_file(root / "scope.json")
    profile = root / "profile"
    sock = root / "first.sock"
    daemon = launch(scope, profile, sock)
    try:
        wait_ready(daemon, sock)
    finally:
        stdout, stderr = stop(daemon)
    marker = profile / MARKER
    assert marker.is_file(), "first scoped startup did not atomically bind a profile marker"
    assert marker.stat().st_mode & 0o777 == 0o600, "scope marker is not owner-only"
    assert marker.stat().st_uid == os.getuid(), "scope marker owner differs from current UID"

    same = launch(scope, profile, root / "same.sock")
    try:
        wait_ready(same, root / "same.sock")
    finally:
        stop(same)

    changed = scope_file(root / "changed.json", day_offset=2)
    mismatch = launch(changed, profile, root / "mismatch.sock")
    _, mismatch_err = reject(mismatch)
    assert mismatch.returncode != 0 and "scope profile mismatch" in mismatch_err, mismatch_err
    assert not (root / "mismatch.sock").exists(), "scope mismatch reached the daemon listener"

    legacy = root / "legacy"
    legacy.mkdir(mode=0o700)
    sentinel = legacy / "existing-data"
    sentinel.write_bytes(b"synthetic-only-preserve")
    rejected = launch(scope, legacy, root / "legacy.sock")
    _, legacy_err = reject(rejected)
    assert rejected.returncode != 0 and "scoped profile must be new, empty or already bound" in legacy_err, legacy_err
    assert sentinel.read_bytes() == b"synthetic-only-preserve", "legacy profile preflight changed its content"

    for mode in ("--demo", "--live"):
        ordinary = launch(None, profile, root / f"ordinary-{mode[2:]}.sock", mode=mode)
        _, ordinary_err = reject(ordinary)
        assert ordinary.returncode != 0 and "scoped profile cannot be opened in ordinary mode" in ordinary_err, ordinary_err
        assert not (root / f"ordinary-{mode[2:]}.sock").exists(), "ordinary mode reached a marked profile listener"

print("PASS: scoped profile marker, same-scope restart, mismatch, legacy isolation, and ordinary-mode rejection")
