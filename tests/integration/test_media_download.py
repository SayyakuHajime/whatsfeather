"""Native incoming-media UI acceptance and socket-isolation regression."""
import os
from pathlib import Path
import socket
import shutil
import stat
import struct
import subprocess
import tempfile
import threading
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]
BINARY = ROOT / "build/bin/whatsfeather"
CHAT_ID = "fixture:incoming-chat"
MESSAGE_ID = "fixture:incoming-document"
IMAGE_ID = "fixture:incoming-image"


class IncomingMediaUI(unittest.TestCase):
    def test_media_ui_rejects_caller_socket_before_any_connection(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable before integration tests")
        with tempfile.TemporaryDirectory(prefix="wf-sentinel-") as temp:
            root = Path(temp)
            sentinel_path = str(root / "sentinel.sock")
            sentinel = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            sentinel.bind(sentinel_path)
            os.chmod(sentinel_path, 0o600)
            sentinel.listen(4)
            sentinel.settimeout(0.05)
            stop = threading.Event()
            accepted = []
            lock = threading.Lock()

            def accept_connections():
                while not stop.is_set():
                    try:
                        connection, _ = sentinel.accept()
                    except (socket.timeout, OSError):
                        continue
                    with lock:
                        accepted.append(connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i")))
                    connection.close()

            accept_thread = threading.Thread(target=accept_connections, daemon=True)
            accept_thread.start()
            state = root / "state"
            command = root / "command"
            state.write_text("")
            command.write_text("")
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root), WHATSFEATHER_MEDIA_TEST_STATE=str(state), WHATSFEATHER_MEDIA_TEST_COMMAND=str(command))
            process = subprocess.Popen([str(BINARY), "--demo", "--media-download-test", "--socket", sentinel_path, "--data-dir", str(root / "profile")], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            try:
                try:
                    process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    self.fail("media UI test mode did not reject the caller socket promptly")
                output, _ = process.communicate()
                with lock:
                    connection_count = len(accepted)
                self.assertEqual(connection_count, 0, "media UI test mode connected to the caller-supplied socket")
                self.assertEqual(process.returncode, 2, output)
                self.assertIn("media-download-test requires --demo, no --socket", output)
            finally:
                if process.poll() is None:
                    process.terminate()
                    process.wait(timeout=3)
                if process.stdout:
                    process.stdout.close()
                stop.set()
                sentinel.close()
                accept_thread.join(timeout=1)

    def wait_state(self, process, state_path, expected, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            lines = state_path.read_text().splitlines()
            if lines and lines[0] == expected:
                return lines
            if process.poll() is not None:
                output = process.communicate()[0]
                self.fail(f"native UI exited before {expected}: {output}")
            time.sleep(0.02)
        self.fail(f"native UI did not reach {expected}; state={state_path.read_text()!r}")

    def test_rendered_download_and_retry_use_only_private_in_process_rpc(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable before integration tests")
        with tempfile.TemporaryDirectory(prefix="wf-media-ui-") as temp:
            root = Path(temp)
            for name in ("config", "data", "cache"):
                (root / name).mkdir(mode=0o700)
            state = root / "media-state"
            command = root / "media-command"
            state.write_text("")
            command.write_text("")
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software", QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root), XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"), XDG_CACHE_HOME=str(root / "cache"), WHATSFEATHER_MEDIA_TEST_STATE=str(state), WHATSFEATHER_MEDIA_TEST_COMMAND=str(command))
            process = subprocess.Popen([str(BINARY), "--demo", "--data-dir", str(root / "profile"), "--media-download-test"], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            try:
                initial = self.wait_state(process, state, "await-download")
                self.assertEqual(initial[1:], ["0"], "remote attachment load must issue no download request")
                command.write_text("activate-download")

                failed = self.wait_state(process, state, "await-retry")
                self.assertEqual(failed[1:], ["1", CHAT_ID, MESSAGE_ID], "first keyboard activation must send exactly one correctly scoped request")
                time.sleep(0.4)
                self.assertIsNone(process.poll(), "failed download auto-retried instead of waiting for explicit Retry")
                self.assertEqual(state.read_text().splitlines()[1:], ["1", CHAT_ID, MESSAGE_ID])
                command.write_text("activate-retry")

                ready = self.wait_state(process, state, "verify-media")
                self.assertEqual(ready[2:], ["2", CHAT_ID, MESSAGE_ID], "explicit Retry must produce exactly the second scoped request")
                media = Path(ready[1])
                self.assertTrue(media.is_file(), "ready response must reference the private synthetic media file")
                self.assertEqual(stat.S_IMODE(media.stat().st_mode), 0o600)
                command.write_text("permissions-verified")
                awaiting_open = self.wait_state(process, state, "await-document-open")
                self.assertEqual(awaiting_open[2:], ["2", CHAT_ID, MESSAGE_ID])
                command.write_text("open-document")
                output, _ = process.communicate(timeout=20)
                self.assertEqual(process.returncode, 0, output)
                self.assertEqual(state.read_text().splitlines()[0], "complete")
                self.assertIn("Native incoming document UI PASS", output)
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)

    def test_video_media_fixture_must_be_inside_private_runtime_dir(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable before integration tests")
        with tempfile.TemporaryDirectory(prefix="wf-video-fixture-boundary-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            runtime = root / "runtime"
            runtime.mkdir(mode=0o700)
            for name in ("config", "data", "cache"):
                (root / name).mkdir(mode=0o700)
            fixture = root / "outside-runtime.mp4"
            subprocess.run([
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1",
                "-f", "lavfi", "-i", "testsrc=size=96x64:rate=1:duration=1",
                "-t", "1", "-c:v", "libx264", "-profile:v", "baseline",
                "-pix_fmt", "yuv420p", "-y", str(fixture),
            ], check=True, capture_output=True)
            state = root / "media-state"
            command = root / "media-command"
            state.write_text("")
            command.write_text("")
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_FORCE_STDERR_LOGGING="1",
                       XDG_RUNTIME_DIR=str(runtime), XDG_CONFIG_HOME=str(root / "config"),
                       XDG_DATA_HOME=str(root / "data"), XDG_CACHE_HOME=str(root / "cache"),
                       WHATSFEATHER_MEDIA_TEST_STATE=str(state),
                       WHATSFEATHER_MEDIA_TEST_COMMAND=str(command),
                       WHATSFEATHER_MEDIA_TEST_KIND="video",
                       WHATSFEATHER_MEDIA_TEST_FIXTURE=str(fixture))
            result = subprocess.run([
                str(BINARY), "--demo", "--media-download-test", "--quit-after", "1000",
                "--data-dir", str(root / "profile"),
            ], env=env, capture_output=True, text=True, timeout=8)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 1, output)
            self.assertIn("inside a private XDG runtime directory", output)

    def test_rendered_incoming_video_download_opens_and_plays_locally(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable before integration tests")
        with tempfile.TemporaryDirectory(prefix="wf-incoming-video-ui-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            for name in ("config", "data", "cache"):
                (root / name).mkdir(mode=0o700)
            fixture = root / "synthetic-incoming.mp4"
            subprocess.run([
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1",
                "-f", "lavfi", "-i", "testsrc=size=96x64:rate=12:duration=2",
                "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=2",
                "-shortest", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast",
                "-profile:v", "baseline", "-pix_fmt", "yuv420p", "-c:a", "aac",
                "-profile:a", "aac_low", "-b:a", "64k", "-movflags", "+faststart",
                "-y", str(fixture),
            ], check=True, capture_output=True)
            state = root / "media-state"
            command = root / "media-command"
            state.write_text("")
            command.write_text("")
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root),
                       XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
                       XDG_CACHE_HOME=str(root / "cache"),
                       WHATSFEATHER_MEDIA_TEST_STATE=str(state),
                       WHATSFEATHER_MEDIA_TEST_COMMAND=str(command),
                       WHATSFEATHER_MEDIA_TEST_KIND="video",
                       WHATSFEATHER_MEDIA_TEST_FIXTURE=str(fixture))
            process = subprocess.Popen([
                str(BINARY), "--demo", "--data-dir", str(root / "profile"),
                "--media-download-test",
            ], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            try:
                initial = self.wait_state(process, state, "await-download")
                self.assertEqual(initial[1:], ["0"])
                command.write_text("activate-download")
                failed = self.wait_state(process, state, "await-retry")
                self.assertEqual(failed[1:], ["1", CHAT_ID, "fixture:incoming-video"])
                time.sleep(0.4)
                self.assertIsNone(process.poll(), "failed video download auto-retried")
                self.assertEqual(state.read_text().splitlines()[1:], ["1", CHAT_ID, "fixture:incoming-video"])
                command.write_text("activate-retry")
                ready = self.wait_state(process, state, "verify-media")
                self.assertEqual(ready[2:], ["2", CHAT_ID, "fixture:incoming-video"])
                media = Path(ready[1])
                self.assertTrue(media.is_file(), "ready video must reference the private synthetic file")
                self.assertEqual(media.suffix, ".mp4")
                self.assertEqual(stat.S_IMODE(media.stat().st_mode), 0o600)
                command.write_text("permissions-verified")
                viewer = self.wait_state(process, state, "await-viewer", timeout=30)
                self.assertEqual(viewer[2:], ["2", CHAT_ID, "fixture:incoming-video"])
                command.write_text("activate-play")
                playing = self.wait_state(process, state, "await-pause", timeout=20)
                self.assertEqual(playing[2:], ["2", CHAT_ID, "fixture:incoming-video"])
                command.write_text("activate-pause")
                paused = self.wait_state(process, state, "await-close")
                self.assertEqual(paused[2:], ["2", CHAT_ID, "fixture:incoming-video"])
                command.write_text("close-viewer")
                output, _ = process.communicate(timeout=20)
                self.assertEqual(process.returncode, 0, output)
                self.assertEqual(state.read_text().splitlines()[0], "complete")
                self.assertIn("Native incoming video UI PASS", output)
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)
                if process.stdout and not process.stdout.closed:
                    process.stdout.close()

    def test_sticker_media_fixture_must_be_inside_private_runtime_dir(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable before integration tests")
        with tempfile.TemporaryDirectory(prefix="wf-sticker-fixture-boundary-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            runtime = root / "runtime"
            runtime.mkdir(mode=0o700)
            for name in ("config", "data", "cache"):
                (root / name).mkdir(mode=0o700)
            fixture = root / "outside-runtime.webp"
            shutil.copyfile(ROOT / "backend/internal/app/testdata/sticker-static.webp", fixture)
            fixture.chmod(0o600)
            state = root / "media-state"
            command = root / "media-command"
            state.write_text("")
            command.write_text("")
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_FORCE_STDERR_LOGGING="1",
                       XDG_RUNTIME_DIR=str(runtime), XDG_CONFIG_HOME=str(root / "config"),
                       XDG_DATA_HOME=str(root / "data"), XDG_CACHE_HOME=str(root / "cache"),
                       WHATSFEATHER_MEDIA_TEST_STATE=str(state),
                       WHATSFEATHER_MEDIA_TEST_COMMAND=str(command),
                       WHATSFEATHER_MEDIA_TEST_KIND="sticker",
                       WHATSFEATHER_MEDIA_TEST_FIXTURE=str(fixture))
            result = subprocess.run([
                str(BINARY), "--demo", "--media-download-test", "--quit-after", "1000",
                "--data-dir", str(root / "profile"),
            ], env=env, capture_output=True, text=True, timeout=8)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 1, output)
            self.assertIn("inside a private XDG runtime directory", output)

    def test_rendered_incoming_sticker_download_opens_locally(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable before integration tests")
        with tempfile.TemporaryDirectory(prefix="wf-incoming-sticker-ui-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            runtime = root / "runtime"
            runtime.mkdir(mode=0o700)
            for name in ("config", "data", "cache"):
                (root / name).mkdir(mode=0o700)
            fixture = runtime / "synthetic-incoming-sticker.webp"
            shutil.copyfile(ROOT / "backend/internal/app/testdata/sticker-static.webp", fixture)
            fixture.chmod(0o600)
            state = root / "media-state"
            command = root / "media-command"
            state.write_text("")
            command.write_text("")
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(runtime),
                       XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
                       XDG_CACHE_HOME=str(root / "cache"),
                       WHATSFEATHER_MEDIA_TEST_STATE=str(state),
                       WHATSFEATHER_MEDIA_TEST_COMMAND=str(command),
                       WHATSFEATHER_MEDIA_TEST_KIND="sticker",
                       WHATSFEATHER_MEDIA_TEST_FIXTURE=str(fixture))
            process = subprocess.Popen([
                str(BINARY), "--demo", "--data-dir", str(root / "profile"),
                "--media-download-test",
            ], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            try:
                initial = self.wait_state(process, state, "await-download")
                self.assertEqual(initial[1:], ["0"])
                command.write_text("activate-download")
                failed = self.wait_state(process, state, "await-retry")
                self.assertEqual(failed[1:], ["1", CHAT_ID, "fixture:incoming-sticker"])
                time.sleep(0.4)
                self.assertIsNone(process.poll(), "failed sticker download auto-retried")
                self.assertEqual(state.read_text().splitlines()[1:], ["1", CHAT_ID, "fixture:incoming-sticker"])
                command.write_text("activate-retry")
                ready = self.wait_state(process, state, "verify-media")
                self.assertEqual(ready[2:], ["2", CHAT_ID, "fixture:incoming-sticker"])
                media = Path(ready[1])
                self.assertTrue(media.is_file(), "ready sticker must reference the private synthetic file")
                self.assertEqual(media.suffix, ".webp")
                self.assertEqual(stat.S_IMODE(media.stat().st_mode), 0o600)
                command.write_text("permissions-verified")
                viewer = self.wait_state(process, state, "await-viewer")
                self.assertEqual(viewer[2:], ["2", CHAT_ID, "fixture:incoming-sticker"])
                command.write_text("open-sticker")
                output, _ = process.communicate(timeout=20)
                self.assertEqual(process.returncode, 0, output)
                self.assertEqual(state.read_text().splitlines()[0], "complete")
                self.assertIn("Native incoming sticker UI PASS", output)
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)
                if process.stdout and not process.stdout.closed:
                    process.stdout.close()

    def test_rendered_incoming_image_download_opens_locally(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable before integration tests")
        with tempfile.TemporaryDirectory(prefix="wf-incoming-image-ui-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            runtime = root / "runtime"
            runtime.mkdir(mode=0o700)
            for name in ("config", "data", "cache"):
                (root / name).mkdir(mode=0o700)
            fixture = runtime / "synthetic-incoming-image.png"
            subprocess.run([
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1",
                "-f", "lavfi", "-i", "color=c=red:s=64x48:d=1", "-frames:v", "1",
                "-y", str(fixture),
            ], check=True, capture_output=True)
            fixture.chmod(0o600)
            state = root / "media-state"
            command = root / "media-command"
            state.write_text("")
            command.write_text("")
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(runtime),
                       XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
                       XDG_CACHE_HOME=str(root / "cache"),
                       WHATSFEATHER_MEDIA_TEST_STATE=str(state),
                       WHATSFEATHER_MEDIA_TEST_COMMAND=str(command),
                       WHATSFEATHER_MEDIA_TEST_KIND="image",
                       WHATSFEATHER_MEDIA_TEST_FIXTURE=str(fixture))
            process = subprocess.Popen([
                str(BINARY), "--demo", "--data-dir", str(root / "profile"),
                "--media-download-test",
            ], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            try:
                initial = self.wait_state(process, state, "await-download")
                self.assertEqual(initial[1:], ["0"])
                command.write_text("activate-download")
                failed = self.wait_state(process, state, "await-retry")
                self.assertEqual(failed[1:], ["1", CHAT_ID, IMAGE_ID])
                time.sleep(0.4)
                self.assertIsNone(process.poll(), "failed image download auto-retried")
                self.assertEqual(state.read_text().splitlines()[1:], ["1", CHAT_ID, IMAGE_ID])
                command.write_text("activate-retry")
                ready = self.wait_state(process, state, "verify-media")
                self.assertEqual(ready[2:], ["2", CHAT_ID, IMAGE_ID])
                media = Path(ready[1])
                self.assertTrue(media.is_file(), "ready image must reference the private synthetic file")
                self.assertEqual(media.suffix, ".png")
                self.assertEqual(stat.S_IMODE(media.stat().st_mode), 0o600)
                command.write_text("permissions-verified")
                viewer = self.wait_state(process, state, "await-viewer")
                self.assertEqual(viewer[2:], ["2", CHAT_ID, IMAGE_ID])
                command.write_text("open-image")
                output, _ = process.communicate(timeout=20)
                self.assertEqual(process.returncode, 0, output)
                self.assertEqual(state.read_text().splitlines()[0], "complete")
                self.assertIn("Native incoming image UI PASS", output)
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)
                if process.stdout and not process.stdout.closed:
                    process.stdout.close()

    def test_image_media_fixture_must_be_inside_private_runtime_dir(self):
        self.assertTrue(BINARY.is_file(), "Build the real Qt executable before integration tests")
        with tempfile.TemporaryDirectory(prefix="wf-image-fixture-boundary-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            runtime = root / "runtime"
            runtime.mkdir(mode=0o700)
            for name in ("config", "data", "cache"):
                (root / name).mkdir(mode=0o700)
            fixture = root / "outside-runtime.png"
            subprocess.run([
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1",
                "-f", "lavfi", "-i", "color=c=red:s=64x48:d=1", "-frames:v", "1",
                "-y", str(fixture),
            ], check=True, capture_output=True)
            fixture.chmod(0o600)
            state = root / "media-state"
            command = root / "media-command"
            state.write_text("")
            command.write_text("")
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_FORCE_STDERR_LOGGING="1",
                       XDG_RUNTIME_DIR=str(runtime), XDG_CONFIG_HOME=str(root / "config"),
                       XDG_DATA_HOME=str(root / "data"), XDG_CACHE_HOME=str(root / "cache"),
                       WHATSFEATHER_MEDIA_TEST_STATE=str(state),
                       WHATSFEATHER_MEDIA_TEST_COMMAND=str(command),
                       WHATSFEATHER_MEDIA_TEST_KIND="image",
                       WHATSFEATHER_MEDIA_TEST_FIXTURE=str(fixture))
            result = subprocess.run([
                str(BINARY), "--demo", "--media-download-test", "--quit-after", "1000",
                "--data-dir", str(root / "profile"),
            ], env=env, capture_output=True, text=True, timeout=8)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 1, output)
            self.assertIn("inside a private XDG runtime directory", output)

    def test_native_media_test_seam_is_rejected_in_live_mode(self):
        result = subprocess.run([str(BINARY), "--live", "--media-download-test"], env=dict(os.environ, QT_QPA_PLATFORM="offscreen"), capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
