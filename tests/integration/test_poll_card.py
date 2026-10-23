#!/usr/bin/env python3
"""Exercise the native read-only poll card with a private synthetic RPC fixture."""
import os
import select
import socket
import struct
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
APP = ROOT / "build" / "bin" / "whatsfeather"
PASS_MARKER = "Native poll results PASS:"


def run(args, env, timeout=25):
    return subprocess.run(
        [str(APP), *args], cwd=ROOT, env=env, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        timeout=timeout, check=False,
    )


def main():
    if not APP.is_file():
        raise SystemExit(f"missing native application: {APP}")
    with tempfile.TemporaryDirectory(prefix="wfpl-") as temporary:
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

        variants = [("dark", "1", False, ""), ("light", "2", False, ""),
                    ("dark", "1", True, ""), ("light", "2", True, ""),
                    ("dark", "1", True, "unknown"), ("light", "2", True, "pending")]
        evidence = Path(os.environ.get("WHATSFEATHER_QA_ROOT", root)) / "poll-cards"
        evidence.mkdir(parents=True, mode=0o700, exist_ok=True)
        for theme, scale, voting, blocked in variants:
            capture = evidence / f"poll-{theme}-{voting}-{blocked or 'ready'}.png"
            variant_env = env.copy()
            variant_env["QT_SCALE_FACTOR"] = scale
            variant_env["WHATSFEATHER_POLL_TEST_VOTING"] = "1" if voting else "0"
            variant_env["WHATSFEATHER_POLL_TEST_BLOCKED"] = blocked
            variant_env["WHATSFEATHER_POLL_TEST_CAPTURE"] = str(capture)
            result = run([
                "--demo", "--poll-test", "--theme", theme,
                "--data-dir", str(root / f"poll-profile-{theme}"),
            ], variant_env)
            marker = "Native poll blocked PASS:" if blocked else "Native poll voting PASS:" if voting else PASS_MARKER
            if result.returncode != 0 or marker not in result.stdout:
                raise SystemExit(
                    f"native poll integration failed in {theme} at scale {scale} (requires PASS marker and exit 0):\n"
                    f"exit={result.returncode}\n{result.stdout}"
                )
            if "qml:" in result.stdout.lower() or "QQmlApplicationEngine failed" in result.stdout:
                raise SystemExit("native poll integration emitted Qt/QML errors:\n" + result.stdout)
            with capture.open("rb") as image:
                header = image.read(24)
            if header[:8] != b"\x89PNG\r\n\x1a\n" or len(header) != 24:
                raise SystemExit(f"native {theme} poll capture is not a complete PNG")
            width, height = struct.unpack(">II", header[16:24])
            if width <= 0 or height <= 0:
                raise SystemExit(f"native {theme} poll capture has invalid dimensions: {width}x{height}")

        sentinel_path = root / "sentinel.sock"
        if len(os.fsencode(sentinel_path)) >= 100:
            raise SystemExit("sentinel socket path exceeds the short AF_UNIX test budget")
        sentinel = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            sentinel.bind(str(sentinel_path))
            sentinel.listen(1)
            rejected_profile = root / "rejected-profile"
            rejected = run([
                "--demo", "--poll-test", "--socket", str(sentinel_path),
                "--data-dir", str(rejected_profile),
            ], env)
            if rejected.returncode != 2 or "rejects --socket" not in rejected.stdout:
                raise SystemExit(
                    "poll-test did not reject a caller socket before startup:\n"
                    f"exit={rejected.returncode}\n{rejected.stdout}"
                )
            readable, _, _ = select.select([sentinel], [], [], 0.1)
            if readable or rejected_profile.exists():
                raise SystemExit("poll-test contacted the sentinel or initialized the rejected profile")
        finally:
            sentinel.close()

        live_profile = root / "live-profile"
        rejected_live = run([
            "--live", "--poll-test", "--data-dir", str(live_profile),
        ], env)
        if (rejected_live.returncode != 2 or
                "Capture/test flags are demo-only" not in rejected_live.stdout or
                live_profile.exists()):
            raise SystemExit(
                "poll-test was not rejected before live profile initialization:\n"
                f"exit={rejected_live.returncode}\n{rejected_live.stdout}"
            )
        print("Native poll integration PASS: private same-UID RPC, read-only results and reviewed single-vote keyboard flow, caller-socket sentinel untouched, live/profile guards")


if __name__ == "__main__":
    main()
