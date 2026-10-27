#!/usr/bin/env python3
"""Fail-closed CLI checks for scoped live profiles without OS keyring access."""
from datetime import datetime, timedelta, timezone
from pathlib import Path
import subprocess
import tempfile

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


def run(scope: Path | None, profile: Path, sock: Path, mode: str = "--live"):
    argv = [str(DAEMON), mode]
    if scope is not None:
        argv += ["--live-test-scope", str(scope)]
    argv += ["--socket", str(sock), "--data-dir", str(profile)]
    return subprocess.run(argv, capture_output=True, text=True, timeout=10)


if not DAEMON.is_file():
    raise SystemExit("build/bin/whatsfeatherd is missing; run scripts/build.sh first")

with tempfile.TemporaryDirectory(prefix="wf-scope-profile-") as temp:
    root = Path(temp)
    runtime = root / "runtime"
    runtime.mkdir(mode=0o700)
    scope = scope_file(root / "scope.json")
    profile = root / "legacy-profile"
    profile.mkdir(mode=0o700)
    history = profile / "history.db"
    original = b"synthetic-legacy-profile-data"
    history.write_bytes(original)
    socket_path = runtime / "legacy.sock"

    # Valid scope input is accepted, but it may not cause implicit migration,
    # key creation, app startup, or listener creation for a plaintext root.
    legacy = run(scope, profile, socket_path)
    assert legacy.returncode == 1 and "legacy plaintext" in legacy.stderr, legacy.stderr
    assert history.read_bytes() == original, "live startup modified legacy history"
    assert not socket_path.exists(), "legacy profile reached the daemon listener"
    assert sorted(item.name for item in profile.iterdir()) == ["history.db"], (
        "legacy rejection created vault metadata, a lock, or other files"
    )

    # A scoped-profile marker in a plaintext demo root is also refused before
    # app.Open; an unscoped live launch still rejects that root as legacy.
    marked = root / "marked-profile"
    marked.mkdir(mode=0o700)
    marker = marked / MARKER
    marker.write_bytes(b"synthetic-only-scope-marker")
    marker.chmod(0o600)
    marked_history = marked / "history.db"
    marked_history.write_bytes(original)
    before = {p.name: p.read_bytes() for p in marked.iterdir()}

    demo_socket = runtime / "demo.sock"
    demo = run(None, marked, demo_socket, mode="--demo")
    assert demo.returncode == 1 and "scoped profile cannot be opened in ordinary mode" in demo.stderr, demo.stderr
    assert not demo_socket.exists(), "ordinary demo mode reached the listener for a scoped profile"
    assert {p.name: p.read_bytes() for p in marked.iterdir()} == before, "demo rejection mutated the profile"

    live_socket = runtime / "unscoped-live.sock"
    ordinary_live = run(None, marked, live_socket, mode="--live")
    assert ordinary_live.returncode == 1 and "legacy plaintext" in ordinary_live.stderr, ordinary_live.stderr
    assert not live_socket.exists(), "unscoped live mode reached the listener for a plaintext root"
    assert {p.name: p.read_bytes() for p in marked.iterdir()} == before, "live rejection mutated the profile"

print("PASS: scoped profile CLI rejects plaintext roots without migration, keyring access, or listener startup")
