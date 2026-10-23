#!/usr/bin/env python3
"""Run the native contact-card review/share probe only against its private fake RPC."""
import os
import select
import socket
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
APP = ROOT / "build" / "bin" / "whatsfeather"
PASS_MARKER = "Native contact-card PASS:"


def run(args, env, timeout=25):
    return subprocess.run(
        [str(APP), *args],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=timeout,
        check=False,
    )


def main():
    if not APP.is_file():
        raise SystemExit(f"missing native application: {APP}")
    with tempfile.TemporaryDirectory(prefix="wfcc-") as temporary:
        base = Path(temporary)
        for name in ("home", "tmp", "runtime"):
            (base / name).mkdir(mode=0o700)
        env = os.environ.copy()
        env.update(
            {
                "HOME": str(base / "home"),
                "TMPDIR": str(base / "tmp"),
                "XDG_RUNTIME_DIR": str(base / "runtime"),
                "XDG_CONFIG_HOME": str(base / "config"),
                "XDG_DATA_HOME": str(base / "data"),
                "XDG_CACHE_HOME": str(base / "cache"),
                "QT_QPA_PLATFORM": "offscreen",
                "QT_QUICK_BACKEND": "software",
                "QT_FORCE_STDERR_LOGGING": "1",
            }
        )

        ui = run(
            [
                "--demo",
                "--contact-card-test",
                "--theme",
                "dark",
                "--data-dir",
                str(base / "profile"),
            ],
            env,
        )
        if ui.returncode != 0 or PASS_MARKER not in ui.stdout:
            raise SystemExit(
                "native contact-card probe failed (requires both PASS marker and exit 0):\n"
                f"exit={ui.returncode}\n{ui.stdout}"
            )

        sentinel_path = base / "sentinel.sock"
        if len(os.fsencode(sentinel_path)) >= 100:
            raise SystemExit("sentinel socket path exceeds the short AF_UNIX test budget")
        sentinel = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            sentinel.bind(str(sentinel_path))
            sentinel.listen(1)
            rejected_socket = run(
                [
                    "--demo",
                    "--contact-card-test",
                    "--socket",
                    str(sentinel_path),
                    "--data-dir",
                    str(base / "rejected-profile"),
                ],
                env,
            )
            if rejected_socket.returncode != 2:
                raise SystemExit(
                    "contact-card test flag did not reject a caller socket:\n"
                    f"exit={rejected_socket.returncode}\n{rejected_socket.stdout}"
                )
            readable, _, _ = select.select([sentinel], [], [], 0.1)
            if readable or (base / "rejected-profile").exists():
                raise SystemExit("test flag contacted the sentinel or initialized the rejected profile")
        finally:
            sentinel.close()

        rejected_live = run(
            [
                "--live",
                "--contact-card-test",
                "--data-dir",
                str(base / "live-profile"),
            ],
            env,
        )
        if rejected_live.returncode != 2 or (base / "live-profile").exists():
            raise SystemExit(
                "contact-card test flag was not rejected before live profile initialization:\n"
                f"exit={rejected_live.returncode}\n{rejected_live.stdout}"
            )
        print("Native contact-card integration PASS: private same-UID fake RPC, rendered plain-text incoming card, explicit vCard picker/review/send, live/socket guards")


if __name__ == "__main__":
    main()
