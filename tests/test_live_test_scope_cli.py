#!/usr/bin/env python3
"""CLI regression: scoped live mode must reject an external backend socket."""
from pathlib import Path
from datetime import datetime, timedelta, timezone
import os
import select
import signal
import socket as unix_socket
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "build" / "bin" / "whatsfeather"
DAEMON = ROOT / "build" / "bin" / "whatsfeatherd"
if not BINARY.is_file() or not DAEMON.is_file():
    raise SystemExit("build/bin/whatsfeather is missing; run scripts/build.sh first")

with tempfile.TemporaryDirectory(prefix="wf-live-scope-cli-") as temp:
    root = Path(temp)
    scope = root / "scope.json"
    scope.write_text(
        '{"since":"2026-10-20T00:00:00Z",'
        '"chats":["15550123456@s.whatsapp.net"],"include_self":false}\n',
        encoding="utf-8",
    )
    scope.chmod(0o600)
    socket_path = root / "external.sock"
    sentinel = unix_socket.socket(unix_socket.AF_UNIX, unix_socket.SOCK_STREAM)
    sentinel.bind(str(socket_path))
    sentinel.listen(1)
    sentinel.setblocking(False)
    profile = root / "profile"
    result = subprocess.run(
        [
            str(BINARY),
            "--live",
            "--live-test-scope",
            str(scope),
            "--socket",
            str(socket_path),
            "--data-dir",
            str(profile),
        ],
        capture_output=True,
        text=True,
        timeout=10,
        env={**os.environ, "QT_QPA_PLATFORM": "offscreen"},
    )
    ready, _, _ = select.select([sentinel], [], [], 0.1)
    sentinel.close()
    assert result.returncode == 2, (
        f"scoped live mode must reject an external socket before startup; "
        f"status={result.returncode}, stderr={result.stderr!r}"
    )
    assert str(scope) not in result.stdout + result.stderr, "native scope diagnostics leaked the file path"
    assert not ready, "scope misuse contacted the external backend socket"
    assert not profile.exists(), "scope misuse must fail before opening/creating the profile"

    unscoped_profile = root / "unscoped-profile"
    unscoped = subprocess.run(
        [str(BINARY), "--live-test-scope", str(scope), "--data-dir", str(unscoped_profile)],
        capture_output=True,
        text=True,
        timeout=10,
        env={**os.environ, "QT_QPA_PLATFORM": "offscreen"},
    )
    assert unscoped.returncode == 2, unscoped.stderr
    assert not unscoped_profile.exists(), "scope without explicit live mode touched the profile"

    # The flag cannot silently select demo mode or bypass daemon preflight.
    demo_profile = root / "demo-profile"
    demo = subprocess.run(
        [str(DAEMON), "--demo", "--live-test-scope", str(scope), "--socket", str(root / "demo.sock"), "--data-dir", str(demo_profile)],
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert demo.returncode == 1 and "requires explicit --live" in demo.stderr, demo.stderr
    assert not demo_profile.exists(), "demo/scope misuse touched the profile"

    # Malformed config must fail closed before the daemon creates profile or socket.
    malformed = root / "malformed.json"
    malformed.write_text(
        '{"since":"2026-10-22T00:00:00Z","chats":["15550123456@s.whatsapp.net"],'
        '"include_self":false,"include_self":true}',
        encoding="utf-8",
    )
    malformed.chmod(0o600)
    invalid_profile = root / "invalid-profile"
    invalid_socket = root / "invalid.sock"
    invalid = subprocess.run(
        [str(DAEMON), "--live", "--live-test-scope", str(malformed), "--socket", str(invalid_socket), "--data-dir", str(invalid_profile)],
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert invalid.returncode == 1 and "invalid live-test scope" in invalid.stderr, invalid.stderr
    assert str(malformed) not in invalid.stderr, "scope diagnostics leaked the config path"
    assert not invalid_profile.exists() and not invalid_socket.exists(), "invalid scope touched profile/socket"

    # The native Controller must forward the path, and defer profile-local
    # preferences until the daemon has validated it.
    runtime = root / "runtime"
    runtime.mkdir(mode=0o700)
    runtime.chmod(0o700)
    native_env = {
        **os.environ,
        "QT_QPA_PLATFORM": "offscreen",
        "QT_QUICK_BACKEND": "software",
        "QT_FORCE_STDERR_LOGGING": "1",
        "XDG_RUNTIME_DIR": str(runtime),
        "XDG_CONFIG_HOME": str(root / "config"),
        "XDG_DATA_HOME": str(root / "data"),
    }
    native_invalid_profile = root / "native-invalid-profile"
    native_invalid = subprocess.run(
        [str(BINARY), "--live", "--live-test-scope", str(malformed), "--data-dir", str(native_invalid_profile), "--quit-after", "1200"],
        capture_output=True,
        text=True,
        timeout=10,
        env=native_env,
    )
    assert native_invalid.returncode == 0, native_invalid.stderr
    assert not native_invalid_profile.exists(), "native startup opened a profile before scope validation"

    # A private valid config reaches the daemon listener without connecting WhatsApp.
    valid_profile = root / "valid-profile"
    valid_socket = root / "valid.sock"
    now = datetime.now(timezone.utc)
    since = (now - timedelta(days=1)).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    valid_scope = root / "valid.json"
    valid_scope.write_text(
        '{"since":"' + since + '","chats":["15550123456@s.whatsapp.net"],"include_self":true}',
        encoding="utf-8",
    )
    valid_scope.chmod(0o600)
    implicit_profile = subprocess.run(
        [str(BINARY), "--live", "--live-test-scope", str(valid_scope), "--quit-after", "1200"],
        capture_output=True,
        text=True,
        timeout=10,
        env=native_env,
    )
    assert implicit_profile.returncode == 2 and "--data-dir" in implicit_profile.stderr, implicit_profile.stderr
    assert not (root / "data" / "WhatsFeather").exists(), "scoped native startup selected a default profile"
    daemon = subprocess.Popen(
        [str(DAEMON), "--live", "--live-test-scope", str(valid_scope), "--socket", str(valid_socket), "--data-dir", str(valid_profile)],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and not valid_socket.exists() and daemon.poll() is None:
            time.sleep(0.05)
        assert daemon.poll() is None and valid_socket.exists(), "valid scoped daemon did not reach its private listener"
        assert valid_profile.is_dir(), "valid scoped daemon did not start the local profile"
        assert not (valid_profile / "device.db").exists(), "startup unexpectedly opened the WhatsApp device store"
    finally:
        if daemon.poll() is None:
            daemon.send_signal(signal.SIGTERM)
        try:
            daemon.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            daemon.kill()
            daemon.communicate(timeout=5)
print("PASS: scoped live mode rejects external backend before profile or socket access")
