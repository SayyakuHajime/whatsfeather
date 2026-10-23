"""Exercise real Go demo RPC through Controller/QML with a synthetic poll snapshot."""
import json
import os
from pathlib import Path
import sqlite3
import socket
import struct
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
APP = ROOT / "build" / "bin" / "whatsfeather"
DAEMON = ROOT / "build" / "bin" / "whatsfeatherd"
MESSAGE_ID = "fixture-go-daemon-poll-results"
CHAT_ID = "demo:alex"
QUESTION = "Weekend plan?"


def rpc(socket_path, method):
    request = {"v": 1, "id": "poll-daemon-ui", "method": method, "params": {}}
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(2)
        conn.connect(str(socket_path))
        conn.sendall(json.dumps(request).encode("utf-8") + b"\n")
        with conn.makefile("rb") as stream:
            response = stream.readline(262145)
    if not response or len(response) > 262144:
        raise RuntimeError(f"invalid {method} response from synthetic daemon")
    value = json.loads(response)
    if value.get("id") != request["id"] or "error" in value:
        raise RuntimeError(f"synthetic demo RPC {method} failed: {value}")
    return value["result"]


def stop_process(proc):
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)
    if proc.stderr:
        proc.stderr.close()


def create_demo_profile(daemon_env, root, profile):
    socket_path = root / "initializer.sock"
    proc = subprocess.Popen(
        [str(DAEMON), "--demo", "--socket", str(socket_path), "--data-dir", str(profile)],
        cwd=ROOT, env=daemon_env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
    )
    try:
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                detail = proc.stderr.read().decode("utf-8", errors="replace") if proc.stderr else ""
                raise RuntimeError("synthetic demo initializer exited: " + detail)
            if socket_path.exists():
                try:
                    status = rpc(socket_path, "app.status")
                    if status.get("mode") == "demo" and status.get("state") == "demo":
                        return
                    raise RuntimeError("initializer did not report explicit demo state")
                except (OSError, RuntimeError):
                    pass
            time.sleep(0.03)
        raise RuntimeError("synthetic demo initializer did not become ready")
    finally:
        stop_process(proc)


def seed_poll_results(profile):
    projection = {
        "question": QUESTION,
        "options": [
            {"name": "Beach", "vote_count": 2},
            {"name": "Hiking", "vote_count": 0},
        ],
    }
    with sqlite3.connect(profile / "history.db", timeout=5) as db:
        mode = db.execute("SELECT value FROM meta WHERE key='mode'").fetchone()
        chat = db.execute("SELECT 1 FROM chats WHERE id=?", (CHAT_ID,)).fetchone()
        if mode != ("demo",) or not chat:
            raise RuntimeError("fixture profile is not the isolated synthetic demo profile")
        timestamp = db.execute(
            "SELECT COALESCE(MAX(timestamp), 0) + 2000 FROM messages WHERE chat_id=?",
            (CHAT_ID,),
        ).fetchone()[0]
        db.execute(
            "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES(?,?,?,?,?,?,?)",
            (MESSAGE_ID, CHAT_ID, QUESTION, timestamp, 0, "demo_received", "poll_results"),
        )
        db.execute(
            "INSERT INTO message_poll_results(chat_id,message_id,projection) VALUES(?,?,?)",
            (CHAT_ID, MESSAGE_ID, json.dumps(projection, separators=(",", ":"))),
        )


def check_capture(path):
    with path.open("rb") as image:
        header = image.read(24)
    if header[:8] != b"\x89PNG\r\n\x1a\n" or len(header) != 24:
        raise RuntimeError("native Go-daemon poll capture is not a complete PNG")
    width, height = struct.unpack(">II", header[16:24])
    if width <= 0 or height <= 0:
        raise RuntimeError(f"invalid native capture dimensions: {width}x{height}")


def assert_test_guards(env, root):
    cases = [
        (
            ["--demo", "--poll-daemon-test", "--socket", str(root / "untouched.sock"),
             "--data-dir", str(root / "rejected-socket-profile")],
            "poll-daemon-test requires --demo and rejects --socket",
            root / "rejected-socket-profile",
        ),
        (
            ["--live", "--poll-daemon-test", "--data-dir", str(root / "rejected-live-profile")],
            "Capture/test flags are demo-only",
            root / "rejected-live-profile",
        ),
    ]
    for args, expected, profile in cases:
        try:
            result = subprocess.run(
                [str(APP), *args], cwd=ROOT, env=env, text=True,
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                timeout=4, check=False,
            )
        except subprocess.TimeoutExpired as exc:
            raise RuntimeError("poll-daemon test guard failed to reject before startup") from exc
        if result.returncode != 2 or expected not in result.stdout:
            raise RuntimeError(
                f"unsafe poll-daemon test arguments were not rejected: {args}\n{result.stdout}"
            )
        if profile.exists():
            raise RuntimeError(f"guard rejection initialized profile data: {profile}")


def main():
    if not APP.is_file() or not DAEMON.is_file():
        raise SystemExit("build the native app and actual Go daemon before this integration test")
    with tempfile.TemporaryDirectory(prefix="wf-poll-daemon-ui-") as temporary:
        root = Path(temporary)
        for name in ("home", "tmp", "runtime", "config", "data", "cache"):
            (root / name).mkdir(mode=0o700)
        profile = root / "poll-profile"
        env = os.environ.copy()
        env.update({
            "HOME": str(root / "home"),
            "TMPDIR": str(root / "tmp"),
            "XDG_RUNTIME_DIR": str(root / "runtime"),
            "XDG_CONFIG_HOME": str(root / "config"),
            "XDG_DATA_HOME": str(root / "data"),
            "XDG_CACHE_HOME": str(root / "cache"),
            "QT_QPA_PLATFORM": "offscreen",
            "QT_QUICK_BACKEND": "software",
            "QT_FORCE_STDERR_LOGGING": "1",
            "WHATSFEATHER_BACKEND": str(DAEMON),
        })
        assert_test_guards(env, root)
        create_demo_profile(env, root, profile)
        seed_poll_results(profile)

        capture_root = Path(os.environ.get("WHATSFEATHER_QA_ROOT", root)) / "poll-daemon-ui"
        capture_root.mkdir(parents=True, mode=0o700, exist_ok=True)
        for theme, scale in (("dark", "1"), ("light", "2")):
            capture = capture_root / f"poll-{theme}-{scale}x.png"
            variant_env = env.copy()
            variant_env["QT_SCALE_FACTOR"] = scale
            variant_env["WHATSFEATHER_POLL_TEST_CAPTURE"] = str(capture)
            result = subprocess.run(
                [str(APP), "--demo", "--poll-daemon-test", "--theme", theme,
                 "--data-dir", str(profile)],
                cwd=ROOT, env=variant_env, text=True,
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                timeout=35, check=False,
            )
            if result.returncode != 0 or "Native Go-daemon poll UI PASS:" not in result.stdout:
                raise SystemExit(
                    f"actual Go-daemon → native poll UI test failed ({theme} {scale}x):\n"
                    f"exit={result.returncode}\n{result.stdout}"
                )
            if "qml:" in result.stdout.lower() or "QQmlApplicationEngine failed" in result.stdout:
                raise SystemExit("Go-daemon poll UI test emitted Qt/QML errors:\n" + result.stdout)
            check_capture(capture)
        print("Native Go-daemon poll UI PASS: real demo backend/RPC → Controller/model → QML read-only results, isolated synthetic fixture, dark/light captures")


if __name__ == "__main__":
    main()
