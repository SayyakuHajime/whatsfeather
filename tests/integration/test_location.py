#!/usr/bin/env python3
"""Synthetic native location rendering and socket/live-flag safety checks."""
import os
import select
import socket
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
APP = ROOT / "build" / "bin" / "whatsfeather"
PASS_MARKER = "Native location PASS:"


def run(args, env, timeout=25):
    return subprocess.run(
        [str(APP), *args], cwd=ROOT, env=env, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        timeout=timeout, check=False,
    )


def main():
    if not APP.is_file():
        raise SystemExit(f"missing native application: {APP}")
    with tempfile.TemporaryDirectory(prefix="wfloc-") as temporary:
        root = Path(temporary)
        for name in ("home", "tmp", "runtime", "config", "data", "cache"):
            (root / name).mkdir(mode=0o700)
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
        })
        profile = root / "location-profile"
        result = run(["--demo", "--location-test", "--data-dir", str(profile)], env)
        if result.returncode != 0 or PASS_MARKER not in result.stdout:
            raise SystemExit(
                "native location integration failed (requires PASS marker and exit 0):\n"
                f"exit={result.returncode}\n{result.stdout}"
            )
        if "qml:" in result.stdout.lower() or "QQmlApplicationEngine failed" in result.stdout:
            raise SystemExit("native location integration emitted Qt/QML errors:\n" + result.stdout)

        sentinel_path = root / "sentinel.sock"
        if len(os.fsencode(sentinel_path)) >= 100:
            raise SystemExit("sentinel socket path exceeds the short AF_UNIX test budget")
        sentinel = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            sentinel.bind(str(sentinel_path))
            sentinel.listen(1)
            rejected_profile = root / "rejected-profile"
            rejected = run([
                "--demo", "--location-test", "--socket", str(sentinel_path),
                "--data-dir", str(rejected_profile),
            ], env)
            if rejected.returncode != 2 or "rejects --socket" not in rejected.stdout:
                raise SystemExit(
                    "location-test did not reject a caller socket before startup:\n"
                    f"exit={rejected.returncode}\n{rejected.stdout}"
                )
            readable, _, _ = select.select([sentinel], [], [], 0.1)
            if readable or rejected_profile.exists():
                raise SystemExit("location-test contacted sentinel or initialized the rejected profile")
        finally:
            sentinel.close()

        live_profile = root / "live-profile"
        rejected_live = run(["--live", "--location-test", "--data-dir", str(live_profile)], env)
        if rejected_live.returncode != 2 or "Capture/test flags are demo-only" not in rejected_live.stdout or live_profile.exists():
            raise SystemExit(
                "location-test was not rejected before live profile initialization:\n"
                f"exit={rejected_live.returncode}\n{rejected_live.stdout}"
            )
        print("Native location integration PASS: private same-UID RPC snapshot card, manual reviewed static send, PlainText labels, no map action, caller-socket sentinel untouched, live/profile guards")


if __name__ == "__main__":
    main()
