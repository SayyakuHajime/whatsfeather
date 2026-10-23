#!/usr/bin/env python3
"""Exercise explicit local-only ButtonsMessage response selection with a private synthetic fixture."""
import os
import select
import shutil
import socket
import struct
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
APP = ROOT / "build" / "bin" / "whatsfeather"
PASS_MARKER = "Native ButtonsMessage PASS:"


def run(args, env, timeout=25):
    return subprocess.run(
        [str(APP), *args], cwd=ROOT, env=env, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        timeout=timeout, check=False,
    )


def main():
    if not APP.is_file():
        raise SystemExit(f"missing native application: {APP}")
    with tempfile.TemporaryDirectory(prefix="wfbtn-") as temporary:
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

        for theme, scale in (("dark", "1"), ("light", "2")):
            captures = {
                "card": root / f"buttons-card-{theme}.png",
                "dialog": root / f"buttons-dialog-{theme}.png",
                "timeline": root / f"buttons-timeline-{theme}.png",
            }
            variant_env = env.copy()
            variant_env["QT_SCALE_FACTOR"] = scale
            variant_env["WHATSFEATHER_BUTTONS_TEST_CAPTURE"] = str(captures["card"])
            variant_env["WHATSFEATHER_BUTTONS_DIALOG_CAPTURE"] = str(captures["dialog"])
            variant_env["WHATSFEATHER_BUTTONS_TIMELINE_CAPTURE"] = str(captures["timeline"])
            result = run([
                "--demo", "--buttons-message-test", "--theme", theme,
                "--data-dir", str(root / f"buttons-profile-{theme}"),
            ], variant_env)
            if result.returncode != 0 or PASS_MARKER not in result.stdout:
                raise SystemExit(
                    f"native ButtonsMessage integration failed in {theme} at scale {scale} (requires PASS marker and exit 0):\n"
                    f"exit={result.returncode}\n{result.stdout}"
                )
            if "qml:" in result.stdout.lower() or "QQmlApplicationEngine failed" in result.stdout:
                raise SystemExit("native ButtonsMessage integration emitted Qt/QML errors:\n" + result.stdout)
            for stage, capture in captures.items():
                with capture.open("rb") as image:
                    header = image.read(24)
                if header[:8] != b"\x89PNG\r\n\x1a\n" or len(header) != 24:
                    raise SystemExit(f"native {theme} ButtonsMessage {stage} capture is not a complete PNG")
                width, height = struct.unpack(">II", header[16:24])
                if width <= 0 or height <= 0:
                    raise SystemExit(f"native {theme} ButtonsMessage {stage} capture has invalid dimensions: {width}x{height}")
            capture_dir = os.environ.get("WHATSFEATHER_BUTTONS_CAPTURE_DIR")
            if capture_dir:
                destination = Path(capture_dir)
                destination.mkdir(mode=0o700, parents=True, exist_ok=True)
                for stage, capture in captures.items():
                    shutil.copyfile(capture, destination / f"buttons-{stage}-{theme}.png")

        sentinel_path = root / "sentinel.sock"
        if len(os.fsencode(sentinel_path)) >= 100:
            raise SystemExit("sentinel socket path exceeds the short AF_UNIX test budget")
        sentinel = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            sentinel.bind(str(sentinel_path))
            sentinel.listen(1)
            rejected_profile = root / "rejected-profile"
            rejected = run([
                "--demo", "--buttons-message-test", "--socket", str(sentinel_path),
                "--data-dir", str(rejected_profile),
            ], env)
            if rejected.returncode != 2 or "rejects --socket" not in rejected.stdout:
                raise SystemExit(
                    "buttons-message-test did not reject a caller socket before startup:\n"
                    f"exit={rejected.returncode}\n{rejected.stdout}"
                )
            readable, _, _ = select.select([sentinel], [], [], 0.1)
            if readable or rejected_profile.exists():
                raise SystemExit("buttons-message-test contacted the sentinel or initialized the rejected profile")
        finally:
            sentinel.close()

        live_profile = root / "live-profile"
        rejected_live = run([
            "--live", "--buttons-message-test", "--data-dir", str(live_profile),
        ], env)
        if (rejected_live.returncode != 2 or
                "Capture/test flags are demo-only" not in rejected_live.stdout or
                live_profile.exists()):
            raise SystemExit(
                "buttons-message-test was not rejected before live profile initialization:\n"
                f"exit={rejected_live.returncode}\n{rejected_live.stdout}"
            )
        print("Native ButtonsMessage integration PASS: explicit local response selection/cancel, private-index RPC, reconciliation, PlainText, no WhatsApp send, caller-socket and live/profile guards")


if __name__ == "__main__":
    main()
