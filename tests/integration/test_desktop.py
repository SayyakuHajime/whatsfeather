"""Exercise the actual Qt executable and its owned demo daemon, offscreen."""
import os
from pathlib import Path
import struct
import socket
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]

class NativeDesktop(unittest.TestCase):
    def run_capture(self, theme, interaction=False):
        with tempfile.TemporaryDirectory(prefix="wf-ui-") as temp:
            root = Path(temp)
            for name in ("runtime", "data", "config", "cache"):
                (root / name).mkdir(mode=0o700)
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                QT_FORCE_STDERR_LOGGING="1", QT_SCALE_FACTOR="1")
            for name in ("runtime", "data", "config", "cache"):
                env[f"XDG_{name.upper()}_{'DIR' if name == 'runtime' else 'HOME'}"] = str(root / name)
            # Keep AF_UNIX path below Linux's 108-byte limit.
            env["XDG_RUNTIME_DIR"] = str(root)
            image = root / "capture.png"
            args = [str(ROOT / "build/bin/whatsfeather"), "--demo", "--theme", theme,
                "--screenshot", str(image), "--quit-after", "4500" if interaction else "2600"]
            if interaction:
                args.append("--ui-test")
            result = subprocess.run(args, env=env, capture_output=True, text=True, timeout=18)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertNotIn("qrc:", output, output)
            self.assertNotIn("QQmlApplicationEngine failed", output, output)
            self.assertIn("Saved native demo screenshot", output)
            if interaction:
                self.assertIn("Native UI smoke PASS", output)
            data = image.read_bytes()
            self.assertEqual(data[:8], b"\x89PNG\r\n\x1a\n")
            self.assertEqual(struct.unpack(">II", data[16:24]), (1180, 780))

    def test_native_interaction_with_real_backend_has_no_qml_warnings(self):
        self.run_capture("dark", interaction=True)

    def test_close_saves_last_keystroke_before_debounce(self):
        import sqlite3
        with tempfile.TemporaryDirectory(prefix="wf-close-") as temp:
            root = Path(temp)
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software", QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root), XDG_CONFIG_HOME=str(root / "config"))
            profile = root / "profile"
            result = subprocess.run([str(ROOT / "build/bin/whatsfeather"), "--demo", "--data-dir", str(profile), "--draft-close-test"], env=env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            with sqlite3.connect(profile / "history.db") as db:
                rows = db.execute("SELECT text FROM drafts WHERE chat_id='demo:alex'").fetchall()
            self.assertEqual(rows, [("last keystroke before closing",)])

    def test_reference_polish_controls_and_local_gif(self):
        import base64
        import hashlib
        import json
        import sqlite3
        with tempfile.TemporaryDirectory(prefix="wf-polish-") as temp:
            root = Path(temp)
            fixture = root / "fixture.gif"
            fixture.write_bytes(base64.b64decode("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"))
            sticker_fixture = ROOT / "backend/internal/app/testdata/sticker-static.webp"
            capture = Path(os.environ["WHATSFEATHER_QA_ROOT"]) / "stickers" if os.environ.get("WHATSFEATHER_QA_ROOT") else Path(tempfile.mkdtemp(prefix="wf-sticker-evidence-"))
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software", QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root), XDG_CONFIG_HOME=str(root / "config"), WHATSFEATHER_QA_CAPTURE_DIR=str(capture))
            profile = root / "profile"
            result = subprocess.run([str(ROOT / "build/bin/whatsfeather"), "--demo", "--data-dir", str(profile), "--polish-test", "--polish-fixture", str(fixture), "--polish-sticker-fixture", str(sticker_fixture)], env=env, capture_output=True, text=True, timeout=40)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native polish PASS", output)
            self.assertIn("Native message flags PASS", output)
            self.assertIn("Native forward PASS", output)
            self.assertIn("static WebP sticker", output)
            self.assertIn("Native local sticker packs PASS", output)
            self.assertNotIn("qrc:", output)
            self.assertNotIn("qml:", output.lower())
            with sqlite3.connect(profile / "history.db") as db:
                rows = db.execute("SELECT meta FROM attachments").fetchall()
            metadata = [json.loads(row[0]) for row in rows]
            self.assertCountEqual([item["kind"] for item in metadata], ["gif", "sticker"])
            sticker = next(item for item in metadata if item["kind"] == "sticker")
            self.assertEqual(sticker["mime"], "image/webp")
            self.assertEqual((sticker["width"], sticker["height"]), (512, 512))
            self.assertLessEqual(sticker["size"], 100_000)
            expression_store = profile / "local-expressions" / "preferences.json"
            self.assertTrue(expression_store.is_file(), "emoji preferences were not persisted to the profile")
            preferences = json.loads(expression_store.read_text(encoding="utf-8"))
            self.assertEqual(preferences.get("recent_emojis"), ["☕", "👎"])
            self.assertEqual(preferences.get("favorite_emojis"), ["👎"])
            packs = preferences.get("sticker_packs")
            self.assertEqual(len(packs), 1)
            self.assertEqual(packs[0].get("name"), "Synthetic pack")
            saved_stickers = packs[0].get("stickers")
            self.assertEqual(len(saved_stickers), 1)
            saved_sticker = saved_stickers[0]
            self.assertEqual(saved_sticker.get("name"), "sticker-static.webp")
            saved_path = profile / "local-expressions" / "sticker-packs" / packs[0]["id"] / f"{saved_sticker['id']}.webp"
            self.assertTrue(saved_path.is_file(), "local sticker copy is missing")
            self.assertLessEqual(saved_path.stat().st_size, 100_000)
            self.assertEqual(hashlib.sha256(saved_path.read_bytes()).hexdigest(), saved_sticker["id"])
            for name in ("sticker-panel.png", "sticker-review.png", "sticker-timeline.png", "sticker-pack-panel.png", "sticker-pack-review.png", "sticker-pack-local-review.png"):
                image = capture / name
                self.assertTrue(image.exists(), name)
                self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n")
            restore_env = dict(env, WHATSFEATHER_TEST_EXPECT_PERSISTED_EXPRESSIONS="1")
            restored = subprocess.run([str(ROOT / "build/bin/whatsfeather"), "--demo", "--data-dir", str(profile), "--polish-test", "--polish-fixture", str(fixture), "--polish-sticker-fixture", str(sticker_fixture)], env=restore_env, capture_output=True, text=True, timeout=40)
            restored_output = restored.stdout + restored.stderr
            self.assertEqual(restored.returncode, 0, restored_output)
            self.assertIn("Native expression preferences restored PASS", restored_output)
            restored_preferences = json.loads(expression_store.read_text(encoding="utf-8"))
            self.assertEqual(restored_preferences.get("recent_emojis"), ["☕", "👎"])
            self.assertEqual(restored_preferences.get("favorite_emojis"), ["👎"])
            for name in ("emoji-recent-restored.png", "emoji-favorites-restored.png"):
                image = capture / name
                self.assertTrue(image.exists(), name)
                self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n")

    def test_native_demo_album_requires_review_and_sends_one_bounded_album(self):
        import binascii
        import json
        import sqlite3
        import zlib
        with tempfile.TemporaryDirectory(prefix="wf-album-") as temp:
            root = Path(temp)
            def png(path, rgb):
                def chunk(kind, data):
                    return (struct.pack(">I", len(data)) + kind + data +
                            struct.pack(">I", binascii.crc32(kind + data) & 0xffffffff))
                raw = b"".join(bytes([0]) + bytes(rgb) * 4 for _ in range(3))
                payload = (bytes([137, 80, 78, 71, 13, 10, 26, 10]) +
                           chunk(b"IHDR", struct.pack(">IIBBBBB", 4, 3, 8, 2, 0, 0, 0)) +
                           chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))
                path.write_bytes(payload)
            fixture = root / "fixture.gif"
            fixture.write_bytes(__import__("base64").b64decode("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"))
            first = root / "album-first.png"
            second = root / "album-second.png"
            png(first, (220, 32, 32))
            png(second, (32, 80, 220))
            sticker_fixture = ROOT / "backend/internal/app/testdata/sticker-static.webp"
            capture = Path(os.environ["WHATSFEATHER_QA_ROOT"]) / "albums" if os.environ.get("WHATSFEATHER_QA_ROOT") else root / "qa"
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root),
                       XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
                       XDG_CACHE_HOME=str(root / "cache"), WHATSFEATHER_QA_CAPTURE_DIR=str(capture))
            profile = root / "profile"
            result = subprocess.run([
                str(ROOT / "build/bin/whatsfeather"), "--demo", "--theme", "dark",
                "--data-dir", str(profile), "--polish-test", "--polish-fixture", str(fixture),
                "--polish-sticker-fixture", str(sticker_fixture),
                "--polish-album-fixture", str(first), "--polish-album-fixture", str(second),
            ], env=env, capture_output=True, text=True, timeout=45)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native album attachment PASS", output)
            self.assertIn("Native polish PASS", output)
            self.assertNotIn("qrc:", output)
            self.assertNotIn("qml:", output.lower())
            with sqlite3.connect(profile / "history.db") as db:
                messages = db.execute(
                    "SELECT id,chat_id,body,kind,status FROM messages WHERE kind='album'"
                ).fetchall()
                self.assertEqual(len(messages), 1, "cancel or duplicate activation created an album message")
                message_id, chat_id, body, kind, status = messages[0]
                self.assertEqual((chat_id, body, kind, status),
                                 ("demo:alex", "Synthetic album caption", "album", "demo_sent"))
                rows = db.execute(
                    "SELECT items FROM message_albums WHERE chat_id=? AND message_id=?",
                    (chat_id, message_id),
                ).fetchall()
            self.assertEqual(len(rows), 1)
            items = json.loads(rows[0][0])
            self.assertEqual(len(items), 2)
            self.assertTrue(all(item["kind"] == "image" and item["state"] == "ready" for item in items))
            self.assertTrue(all(item["url"].startswith("file:///") for item in items))
            self.assertLessEqual(sum(item["size"] for item in items), 50 * 1024 * 1024)
            for name in ("album-review.png", "album-after-cancel.png", "album-timeline.png"):
                image = capture / name
                self.assertTrue(image.exists(), name)
                self.assertEqual(image.read_bytes()[:8], bytes([137, 80, 78, 71, 13, 10, 26, 10]))

    def test_polish_probe_rejects_external_socket_before_connecting(self):
        with tempfile.TemporaryDirectory(prefix="wf-polish-socket-") as temp:
            root = Path(temp)
            sentinel = root / "sentinel.sock"
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            listener.bind(str(sentinel))
            listener.listen(1)
            listener.settimeout(0.2)
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root), XDG_CONFIG_HOME=str(root / "config"))
            try:
                try:
                    result = subprocess.run([
                        str(ROOT / "build/bin/whatsfeather"), "--demo", "--socket", str(sentinel), "--polish-test",
                    ], env=env, capture_output=True, text=True, timeout=4)
                except subprocess.TimeoutExpired:
                    try:
                        connection, _ = listener.accept()
                    except socket.timeout:
                        self.fail("polish-test stalled without rejecting its external socket")
                    else:
                        connection.close()
                        self.fail("polish-test connected to a caller-supplied socket")
                output = result.stdout + result.stderr
                self.assertEqual(result.returncode, 2, output)
                self.assertIn("polish-test requires --demo and rejects --socket", output)
                with self.assertRaises(socket.timeout):
                    listener.accept()
            finally:
                listener.close()

    def test_product_probe_rejects_external_socket_before_connecting(self):
        with tempfile.TemporaryDirectory(prefix="wf-product-socket-") as temp:
            root = Path(temp)
            sentinel = root / "sentinel.sock"
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            listener.bind(str(sentinel))
            listener.listen(1)
            listener.settimeout(0.2)
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root), XDG_CONFIG_HOME=str(root / "config"))
            try:
                try:
                    result = subprocess.run([
                        str(ROOT / "build/bin/whatsfeather"), "--demo", "--socket", str(sentinel), "--product-test",
                    ], env=env, capture_output=True, text=True, timeout=4)
                except subprocess.TimeoutExpired:
                    try:
                        connection, _ = listener.accept()
                    except socket.timeout:
                        self.fail("product-test stalled without rejecting its external socket")
                    else:
                        connection.close()
                        self.fail("product-test connected to a caller-supplied socket")
                output = result.stdout + result.stderr
                self.assertEqual(result.returncode, 2, output)
                self.assertIn("product-test requires --demo and rejects --socket", output)
                with self.assertRaises(socket.timeout):
                    listener.accept()
            finally:
                listener.close()

    def test_light_theme_renders_without_qml_warnings(self):
        self.run_capture("light")

    def test_native_video_selection_review_and_local_playback(self):
        with tempfile.TemporaryDirectory(prefix="wf-video-ui-") as temp:
            root = Path(temp)
            fixture = root / "synthetic.mp4"
            subprocess.run([
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1",
                "-f", "lavfi", "-i", "testsrc=size=96x64:rate=12:duration=2",
                "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=2",
                "-shortest", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast",
                "-profile:v", "baseline", "-pix_fmt", "yuv420p", "-c:a", "aac",
                "-profile:a", "aac_low", "-b:a", "64k", "-movflags", "+faststart",
                "-y", str(fixture),
            ], check=True, capture_output=True)
            capture = root / "qa"
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root),
                       XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
                       XDG_CACHE_HOME=str(root / "cache"), WHATSFEATHER_QA_CAPTURE_DIR=str(capture))
            profile = root / "profile"
            result = subprocess.run([
                str(ROOT / "build/bin/whatsfeather"), "--demo", "--theme", "dark",
                "--data-dir", str(profile), "--product-test", "--product-fixture", str(fixture),
            ], env=env, capture_output=True, text=True, timeout=40)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native video PASS", output)
            self.assertNotIn("Unsupported image format", output, output)
            self.assertNotIn("Error decoding", output, output)
            self.assertNotIn("qml:", output.lower())
            for name in ("attachment-video-review.png", "media-viewer.png", "media-viewer-playing.png"):
                image = capture / name
                self.assertTrue(image.exists(), name)
                self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n")

    def test_native_document_selection_review_and_send(self):
        with tempfile.TemporaryDirectory(prefix="wf-document-ui-") as temp:
            root = Path(temp)
            for name in ("config", "cache", "data"):
                (root / name).mkdir(mode=0o700)
            fixture = root / "synthetic-report.pdf"
            fixture.write_bytes(b"%PDF-1.4\nsynthetic document fixture\n")
            capture = root / "qa"
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root),
                       XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
                       XDG_CACHE_HOME=str(root / "cache"), WHATSFEATHER_QA_CAPTURE_DIR=str(capture))
            result = subprocess.run([
                str(ROOT / "build/bin/whatsfeather"), "--demo", "--theme", "dark",
                "--data-dir", str(root / "profile"), "--product-test", "--product-fixture", str(fixture),
            ], env=env, capture_output=True, text=True, timeout=40)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native document PASS", output)
            self.assertNotIn("qml:", output.lower())
            for name in ("attachment-document-review.png", "media.png"):
                image = capture / name
                self.assertTrue(image.exists(), name)
                self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n")

    def test_product_completion_demo_only_captures_native_states(self):
        import base64
        with tempfile.TemporaryDirectory(prefix="wf-product-") as temp:
            root = Path(temp)
            for name in ("config", "cache", "data"):
                (root / name).mkdir(mode=0o700)
            fixture = root / "fixture.gif"
            fixture.write_bytes(base64.b64decode("R0lGODlhAgABAIEAAP8AAAAAAAAAAAAAACH/C05FVFNDQVBFMi4wAwEAAAAh+QQAFAAAACwAAAAAAgABAAAIBQABAAgIACH5BAEUAAEALAAAAAACAAEAgQAA/wAAAAAAAAAAAAgFAAEACAgAOw=="))
            capture = root / "qa"
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root),
                       XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
                       XDG_CACHE_HOME=str(root / "cache"), WHATSFEATHER_QA_CAPTURE_DIR=str(capture))
            result = subprocess.run([
                str(ROOT / "build/bin/whatsfeather"), "--demo", "--theme", "dark",
                "--data-dir", str(root / "profile"), "--product-test",
                "--product-fixture", str(fixture)], env=env, capture_output=True,
                text=True, timeout=40)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native product PASS", output)
            self.assertIn("Native receipt status UI PASS", output)
            self.assertIn("Native disconnect/unlink boundary UI PASS", output)
            self.assertIn("Native compact disconnect/unlink boundary UI PASS", output)
            self.assertIn("Native notification preferences PASS", output)
            self.assertIn("Native local hubs PASS", output)
            self.assertIn("Native group management PASS", output)
            self.assertIn("Native link preview PASS", output)
            self.assertIn("synthetic contact/group presence and typing snapshots", output)
            self.assertIn("Native presence expiry PASS", output)
            self.assertIn("Native demo profile edit PASS", output)
            self.assertIn("typed unsupported-content fallback", output)
            self.assertIn("incoming voice-note label", output)
            self.assertIn("muted native media controls", output)
            self.assertIn("capability-gated contact/media hubs", output)
            self.assertNotIn("qml:", output.lower())
            for name in ("message-layout.png", "unsupported-content.png", "incoming-audio-voice-note.png", "settings.png", "settings-desktop.png", "settings-disconnect-boundary.png", "settings-compact-disconnect-boundary.png", "profile.png", "profile-editing.png", "profile-edited.png", "media.png", "cached-conversation-info.png", "cached-group-info.png", "cached-group-info-expired.png", "managed-group.png", "managed-group-actions.png", "shared-media-library.png", "link-preview-card.png"):
                image = capture / name
                self.assertTrue(image.exists(), name)
                self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n")

    def test_product_voice_note_ogg_opus_decodes_and_play_pause_stays_muted(self):
        with tempfile.TemporaryDirectory(prefix="wf-voice-") as temp:
            root = Path(temp)
            for name in ("config", "cache", "data"):
                (root / name).mkdir(mode=0o700)
            fixture = root / "voice-note.ogg"
            # A silent Opus track exercises native decoding without audible output.
            subprocess.run([
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1",
                "-f", "lavfi", "-i", "anullsrc=channel_layout=mono:sample_rate=48000",
                "-t", "5", "-c:a", "libopus", "-b:a", "16k", "-y", str(fixture),
            ], check=True, capture_output=True)
            capture = root / "qa"
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", XDG_RUNTIME_DIR=str(root),
                       XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
                       XDG_CACHE_HOME=str(root / "cache"), WHATSFEATHER_QA_CAPTURE_DIR=str(capture))
            result = subprocess.run([
                str(ROOT / "build/bin/whatsfeather"), "--demo", "--theme", "dark",
                "--data-dir", str(root / "profile"), "--product-test", "--product-fixture", str(fixture),
            ], env=env, capture_output=True, text=True, timeout=40)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native voice-note playback PASS", output)
            self.assertIn("Native audio send PASS", output)
            self.assertNotIn("Error decoding", output, output)
            self.assertNotIn("Unsupported image format", output, output)
            self.assertNotIn("qml:", output.lower())
            image = capture / "voice-note-playback.png"
            self.assertTrue(image.exists(), image)
            self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n")

    def test_shared_media_library_selects_exact_cross_chat_message(self):
        import base64
        import json
        import sqlite3
        with tempfile.TemporaryDirectory(prefix="wf-global-media-ui-", dir=os.environ.get("TMPDIR")) as temp:
            root = Path(temp)
            for name in ("config", "data", "cache"):
                (root / name).mkdir(mode=0o700)
            profile = root / "profile"
            env = dict(os.environ, QT_QPA_PLATFORM="offscreen", QT_QUICK_BACKEND="software",
                       QT_FORCE_STDERR_LOGGING="1", QT_SCALE_FACTOR="1",
                       XDG_RUNTIME_DIR=str(root), XDG_CONFIG_HOME=str(root / "config"),
                       XDG_DATA_HOME=str(root / "data"), XDG_CACHE_HOME=str(root / "cache"))
            initialized = subprocess.run([
                str(ROOT / "build/bin/whatsfeather"), "--demo", "--data-dir", str(profile),
                "--quit-after", "1200",
            ], env=env, capture_output=True, text=True, timeout=15)
            self.assertEqual(initialized.returncode, 0, initialized.stdout + initialized.stderr)
            media_id = "media-library-collision"
            with sqlite3.connect(profile / "history.db") as db:
                for chat_id, caption in (("demo:alex", "Caption from Alex"),
                                         ("demo:maya", "Caption from Maya")):
                    db.execute(
                        "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES(?,?,?,?,0,'received','image')",
                        (media_id, chat_id, caption, 2_000_000_000_000),
                    )
                    meta = {
                        "name": f"{chat_id} <b>synthetic</b>.png", "size": 12,
                        "mime": "image/png", "kind": "image", "state": "remote",
                        "url": "https://invalid.example/signed?token=PRIVATE_DESCRIPTOR",
                        "media_key": "PRIVATE_DESCRIPTOR", "direct_path": "PRIVATE_DESCRIPTOR",
                    }
                    db.execute(
                        "INSERT INTO attachments(chat_id,message_id,meta,payload) VALUES(?,?,?,NULL)",
                        (chat_id, media_id, json.dumps(meta)),
                    )
                db.commit()
            fixture = root / "fixture.gif"
            fixture.write_bytes(base64.b64decode(
                "R0lGODlhAgABAIEAAP8AAAAAAAAAAAAAACH/C05FVFNDQVBFMi4wAwEAAAAh+QQAFAAAACwAAAAAAgABAAAIBQABAAgIACH5BAEUAAEALAAAAAACAAEAgQAA/wAAAAAAAAAAAAgFAAEACAgAOw=="
            ))
            capture = root / "qa"
            env["WHATSFEATHER_QA_CAPTURE_DIR"] = str(capture)
            env["WHATSFEATHER_TEST_GLOBAL_MEDIA_LIBRARY"] = "1"
            result = subprocess.run([
                str(ROOT / "build/bin/whatsfeather"), "--demo", "--theme", "dark",
                "--data-dir", str(profile), "--product-test", "--product-fixture", str(fixture),
            ], env=env, capture_output=True, text=True, timeout=100)
            output = result.stdout + result.stderr
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Native global media library PASS", output)
            image = capture / "shared-media-library-global.png"
            self.assertTrue(image.is_file(), image)
            self.assertEqual(image.read_bytes()[:8], b"\x89PNG\r\n\x1a\n")
            with sqlite3.connect(profile / "history.db") as db:
                records = db.execute(
                    "SELECT chat_id,meta,payload FROM attachments WHERE message_id=? ORDER BY chat_id",
                    (media_id,),
                ).fetchall()
            self.assertEqual(len(records), 2)
            self.assertTrue(all(json.loads(meta)["state"] == "remote" and payload is None
                                for _, meta, payload in records))

if __name__ == "__main__":
    unittest.main(verbosity=2)
