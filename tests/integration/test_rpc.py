"""Black-box checks against the real demo daemon; no WhatsApp network traffic."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]
BINARY = ROOT / "build/bin/whatsfeatherd"


class DemoRPC(unittest.TestCase):
    def setUp(self):
        self.assertTrue(BINARY.is_file(), "Build the real daemon before integration tests")
        self.temp = tempfile.TemporaryDirectory(prefix="wf-rpc-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.path = self.root / "s.sock"
        self.data = self.root / "data"
        self.proc = None
        self.start()
        self.addCleanup(self.stop)

    def start(self):
        self.proc = subprocess.Popen(
            [str(BINARY), "--demo", "--socket", str(self.path), "--data-dir", str(self.data)],
            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
        )
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                self.fail("Daemon exited: " + (self.proc.stderr.read().decode() if self.proc.stderr else "no diagnostics"))
            if self.path.exists():
                try:
                    self.rpc("app.status")
                    return
                except OSError:
                    pass
            time.sleep(0.03)
        self.fail("Daemon did not become ready")

    def stop(self):
        if self.proc is not None:
            if self.proc.poll() is None:
                self.proc.terminate()
                try:
                    self.proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    self.proc.kill()
                    self.proc.wait(timeout=5)
            if self.proc.stderr:
                self.proc.stderr.close()
            self.proc = None

    def raw(self, request):
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
            conn.settimeout(4)
            conn.connect(str(self.path))
            conn.sendall(json.dumps(request).encode() + b"\n")
            with conn.makefile("rb") as stream:
                data = stream.readline(262145)
        self.assertTrue(data, "Daemon must reply")
        self.assertLessEqual(len(data), 262144)
        return json.loads(data)

    def rpc(self, method, params=None):
        response = self.raw({"v": 1, "id": "integration", "method": method, "params": params or {}})
        self.assertEqual(response.get("id"), "integration")
        self.assertNotIn("error", response, response)
        return response["result"]

    def test_account_get_reports_explicit_synthetic_identity(self):
        account = self.rpc("account.get")
        self.assertTrue(account["available"])
        self.assertEqual(account["display_name"], "Demo Account")
        self.assertEqual(account["username"], "demo")
        self.assertEqual(account["state"], "demo")
        self.assertEqual(account["avatar_url"], "")

    def test_status_uses_real_demo_backend(self):
        status = self.rpc("app.status")
        self.assertEqual(status["mode"], "demo")
        self.assertEqual(status["state"], "demo")
        self.assertFalse(status["qr_png"])
        chats = self.rpc("chat.list")["chats"]
        self.assertGreater(len(chats), 0)
        self.assertTrue(all(c["id"].startswith("demo:") for c in chats))


    def test_private_paths_and_no_live_side_effect_in_demo(self):
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.data.stat().st_mode & 0o777, 0o700)
        databases = list(self.data.rglob("*.db"))
        self.assertTrue(databases, "History must be persisted in a real database")
        for db in databases:
            self.assertEqual(db.stat().st_mode & 0o777, 0o600, str(db))
        response = self.raw({"v": 1, "id": "auth", "method": "auth.connect", "params": {}})
        self.assertIn("error", response)
        self.assertEqual(self.rpc("app.status")["state"], "demo")

    def test_community_directory_is_explicit_synthetic_and_persists_over_wire(self):
        caps = self.rpc("app.capabilities")
        self.assertIn("community.list", caps["methods"])
        self.assertIn("community.refresh", caps["methods"])
        self.assertTrue(caps["features"]["community-folders"]["available"])
        folder_page = self.rpc("community.list", {"limit": 1})
        self.assertEqual(folder_page["source"], "demo_synthetic")
        self.assertLessEqual(len(json.dumps(folder_page, ensure_ascii=False).encode()) + 128, 262144)
        self.assertTrue(folder_page["communities"])
        folder = folder_page["communities"][0]
        self.assertTrue(folder["id"].startswith("demo:"))
        self.assertTrue(folder["title"])
        self.assertTrue(folder["groups"])
        for group in folder["groups"]:
            self.assertTrue(group["id"].startswith("demo:"))
            self.assertTrue(group["community_id"], group)
            self.assertEqual(group["community_title"], folder["title"])
            self.assertNotIn("participants", group)
        chat_page = self.rpc("chat.list")
        self.assertLessEqual(len(json.dumps(chat_page, ensure_ascii=False).encode()) + 128, 262144)
        chats = chat_page["chats"]
        self.assertTrue(any(c.get("community_id") == folder["id"] for c in chats))
        matches = self.rpc("chat.list", {"query": folder["title"]})["chats"]
        self.assertTrue(any(c.get("community_id") == folder["id"] for c in matches))
        member = folder["groups"][0]
        group_matches = self.rpc("chat.list", {"query": member["title"]})["chats"]
        matching_group = next(c for c in group_matches if c["id"] == member["id"])
        self.assertEqual(matching_group["community_id"], folder["id"])
        self.assertEqual(matching_group["community_title"], folder["title"])
        blocked = self.raw({"v": 1, "id": "refresh-demo", "method": "community.refresh", "params": {}})
        self.assertIn("error", blocked, "demo must not imply live WhatsApp metadata")
        self.stop()
        self.start()
        reopened = self.rpc("community.list")
        self.assertEqual(reopened["source"], "demo_synthetic")
        self.assertEqual(reopened["communities"], self.rpc("community.list")["communities"])

    def test_draft_unicode_and_whitespace_survive_restart(self):
        chat = self.rpc("chat.list")["chats"][0]["id"]
        text = "  draft 日本語 🌿\nnext line  "
        self.rpc("draft.set", {"chat_id": chat, "text": text})
        self.assertEqual(self.rpc("draft.get", {"chat_id": chat})["text"], text)
        self.stop()
        self.start()
        self.assertEqual(self.rpc("draft.get", {"chat_id": chat})["text"], text)

    def test_send_is_persisted_idempotently_and_is_not_fake_delivery(self):
        chat = self.rpc("chat.list")["chats"][0]["id"]
        params = {"chat_id": chat, "text": "  Local test 日本語  ", "client_id": "integration-idempotent"}
        first = self.rpc("message.send", params)["message"]
        second = self.rpc("message.send", params)["message"]
        self.assertEqual(first["id"], second["id"])
        self.assertEqual(first["body"], params["text"])
        self.assertEqual(first["status"], "demo_sent")
        self.stop()
        self.start()
        third = self.rpc("message.send", params)["message"]
        self.assertEqual(third["id"], first["id"])
        page = self.rpc("message.list", {"chat_id": chat})["messages"]
        self.assertEqual(sum(m["id"] == first["id"] for m in page), 1)

    def test_demo_message_actions_and_organization_over_real_socket(self):
        caps = self.rpc("app.capabilities")
        methods = set(caps["methods"])
        required = {"message.reply", "message.react", "message.edit", "message.delete",
                    "message.forward", "message.star", "message.pin", "chat.update",
                    "chat.open", "contact.list", "chat.info", "media.list", "media.library",
                    "notification.poll"}
        self.assertTrue(required <= methods, f"missing action routes: {required - methods}")
        for unavailable in ("group.list", "group.info", "privacy.get", "privacy.set",
                            "account.update", "channel.list", "community.info"):
            self.assertNotIn(unavailable, methods, f"unimplemented {unavailable} must not be advertised")
        features = caps["features"]
        self.assertTrue(features["account-profile-view"]["available"])
        self.assertTrue(features["native-media-send"]["available"])
        self.assertTrue(features["chat-info-local"]["available"])
        self.assertTrue(features["media-library-local"]["available"])
        for feature in ("structured-content", "contact-group-info", "giphy-owner-key"):
            self.assertFalse(features[feature]["available"], feature)
            self.assertTrue(features[feature]["reason"], feature)
        self.assertTrue(features["shared-media-library"]["available"])
        self.assertTrue(features["profile-edit"]["available"] is False)
        self.assertTrue(features["groups-management"]["available"])
        self.assertTrue(features["privacy-security-settings"]["available"] is False)

        # These are selected-chat, cache-only projections—not full remote hubs.
        before = self.rpc("message.list", {"chat_id": "demo:alex"})
        info = self.rpc("chat.info", {"chat_id": "demo:alex"})
        self.assertEqual(info["id"], "demo:alex")
        self.assertEqual(info["provenance"], "local_cached")
        self.assertEqual(info["organization_provenance"], "local_only")
        self.assertEqual(info["count_scope"], "local_retained_non_deleted_non_protected")
        self.assertNotIn("participants", info)
        media = self.rpc("media.list", {"chat_id": "demo:alex", "limit": 2})
        self.assertEqual(media["chat_id"], "demo:alex")
        self.assertEqual(media["provenance"], "local_cached")
        self.assertIsInstance(media["messages"], list)
        self.assertLessEqual(len(media["messages"]), 2)
        self.assertIsInstance(media["has_more"], bool)
        for entry in media["messages"]:
            self.assertEqual(entry["chat_id"], "demo:alex")
            self.assertIn(entry["kind"], {"image", "video", "audio", "document", "gif", "sticker"})
        self.assertEqual(before, self.rpc("message.list", {"chat_id": "demo:alex"}))

        source = self.rpc("message.send", {"chat_id": "demo:alex", "text": "wire actions source",
                                             "client_id": "wire-actions-source"})["message"]
        self.assertEqual(source["status"], "demo_sent")
        reply = self.rpc("message.reply", {"chat_id": "demo:alex", "target_id": source["id"],
                                             "text": "wire reply", "client_id": "wire-actions-reply"})["message"]
        self.assertEqual(reply["reply_to_id"], source["id"])
        self.assertEqual(reply["status"], "demo_sent")
        self.rpc("message.react", {"chat_id": "demo:alex", "target_id": source["id"],
                                    "reaction": "👍", "client_id": "wire-actions-react"})
        edited = self.rpc("message.edit", {"chat_id": "demo:alex", "id": source["id"],
                                             "text": "wire actions edited", "client_id": "wire-actions-edit"})["message"]
        self.assertTrue(edited["edited"])
        self.assertEqual(edited["body"], "wire actions edited")
        self.assertEqual(self.rpc("message.star", {"chat_id": "demo:alex", "id": source["id"],
                                                      "starred": True})["provenance"], "local_only")
        self.assertEqual(self.rpc("message.pin", {"chat_id": "demo:alex", "id": source["id"],
                                                     "pinned": True, "duration_seconds": 3600})["provenance"], "local_only")
        forwarded = self.rpc("message.forward", {"chat_id": "demo:alex", "id": source["id"],
                                                    "target_chat_id": "demo:maya", "client_id": "wire-actions-forward"})["message"]
        self.assertTrue(forwarded["forwarded"])
        self.assertEqual(forwarded["body"], "wire actions edited")
        self.assertEqual(forwarded["mutation_provenance"], "local_only")
        self.rpc("message.delete", {"chat_id": "demo:alex", "id": reply["id"], "scope": "local"})
        messages = self.rpc("message.list", {"chat_id": "demo:alex"})["messages"]
        saved = next(m for m in messages if m["id"] == source["id"])
        self.assertTrue(saved["starred"] and saved["pinned"])
        self.assertEqual(saved["reactions"][0]["emoji"], "👍")
        organization = self.rpc("chat.update", {"chat_id": "demo:alex", "unread": True,
                                                    "favorite": True, "list_ids": ["work"]})["chat"]
        self.assertTrue(organization["marked_unread"])
        chats = self.rpc("chat.list")["chats"]
        alex = next(c for c in chats if c["id"] == "demo:alex")
        self.assertIsInstance(alex["unread"], int)
        self.assertTrue(alex["marked_unread"] and alex["favorite"])
        self.assertEqual(alex["list_ids"], ["work"])
        response = self.raw({"v": 1, "id": "not-live", "method": "chat.open",
                             "params": {"recipient": "+15550100"}})
        self.assertIn("error", response, "demo must not fabricate an opened remote conversation")
        contacts = self.rpc("contact.list", {"limit": 20})
        self.assertEqual(contacts["contacts"], [])
        self.assertEqual(contacts["provenance"], "demo_empty")
        blocked = self.raw({"v": 1, "id": "revoke", "method": "message.delete",
                            "params": {"chat_id": "demo:alex", "id": source["id"],
                                       "scope": "everyone", "client_id": "wire-revoke"}})
        self.assertIn("error", blocked, "demo must not claim server revoke")

    def test_global_media_library_over_real_demo_socket_without_selected_chat(self):
        caps = self.rpc("app.capabilities")
        self.assertIn("media.library", caps["methods"])
        self.assertTrue(caps["features"]["shared-media-library"]["available"])
        page = self.rpc("media.library", {"limit": 2})
        self.assertNotIn("chat_id", page)
        self.assertEqual(page["provenance"], "local_cached")
        self.assertIn("not a complete history", page["disclosure"])
        self.assertIsInstance(page["entries"], list)
        self.assertLessEqual(len(page["entries"]), 2)
        for entry in page["entries"]:
            self.assertTrue(entry["chat_id"])
            self.assertTrue(entry["chat_title"])
            self.assertTrue(entry["id"])
            self.assertIn(entry["kind"], {"image", "video", "audio", "document", "gif", "sticker"})
            self.assertEqual(entry["attachment"]["kind"], entry["kind"])
            self.assertFalse(set(entry["attachment"]) & {"url", "media_key", "direct_path", "payload", "path"})

    def test_demo_group_management_over_real_socket(self):
        caps = self.rpc("app.capabilities")
        group_routes = {"group.create", "group.update", "group.participants", "group.join", "group.leave"}
        self.assertTrue(group_routes <= set(caps["methods"]), f"missing demo group routes: {group_routes - set(caps['methods'])}")
        self.assertTrue(caps["features"]["groups-management"]["available"])
        created = self.rpc("group.create", {
            "title": "Synthetic wire group",
            "description": "Wire fixture only",
            "member_ids": ["demo:participant:maya"],
        })
        group_id = created["group_id"]
        self.assertTrue(group_id.startswith("demo:group:"))
        self.assertEqual(created["membership_state"], "joined")
        self.assertEqual(created["provenance"], "demo_synthetic")
        self.assertTrue(created["chat"]["is_group"])
        details = self.rpc("chat.info", {"chat_id": group_id})
        self.assertEqual(details["details_provenance"], "demo_synthetic")
        self.assertEqual(details["group_details"]["description"], "Wire fixture only")
        self.assertEqual(details["group_details"]["participant_count"], 2)
        self.stop()
        self.start()
        restored = self.rpc("chat.info", {"chat_id": group_id})
        self.assertEqual(restored["title"], "Synthetic wire group")
        self.assertEqual(restored["group_details"]["membership_state"], "joined")
        self.assertEqual(restored["group_details"]["participant_count"], 2)

    def test_demo_chat_json_separates_unread_count_and_manual_mark(self):
        self.rpc("chat.update", {"chat_id": "demo:alex", "unread": True})
        alex = next(c for c in self.rpc("chat.list")["chats"] if c["id"] == "demo:alex")
        self.assertIsInstance(alex["unread"], int)
        self.assertTrue(alex["marked_unread"])

    def test_pagination_is_bounded_and_cursors_do_not_repeat(self):
        chat = self.rpc("chat.list")["chats"][0]["id"]
        for i in range(7):
            self.rpc("message.send", {"chat_id": chat, "text": f"page {i}", "client_id": f"page-{i}"})
        first = self.rpc("message.list", {"chat_id": chat, "limit": 2})
        self.assertEqual(len(first["messages"]), 2)
        self.assertTrue(first["has_more"])
        oldest = first["messages"][0]
        second = self.rpc("message.list", {"chat_id": chat, "limit": 2,
            "before": {"timestamp": oldest["timestamp"], "id": oldest["id"]}})
        self.assertEqual(len(second["messages"]), 2)
        self.assertTrue(set(m["id"] for m in first["messages"]).isdisjoint(m["id"] for m in second["messages"]))
        for page in (first, second):
            keys = [(m["timestamp"], m["id"]) for m in page["messages"]]
            self.assertEqual(keys, sorted(keys))
        capped = self.rpc("message.list", {"chat_id": chat, "limit": 10000})
        self.assertLessEqual(len(capped["messages"]), 100)

    def test_invalid_version_empty_message_and_unknown_chat_are_rejected(self):
        self.assertIn("error", self.raw({"v": 99, "id": "bad", "method": "app.status", "params": {}}))
        chat = self.rpc("chat.list")["chats"][0]["id"]
        for method, params in [
            ("message.send", {"chat_id": chat, "text": " \n\t ", "client_id": "empty"}),
            ("message.send", {"chat_id": chat, "text": "x" * 16001, "client_id": "too-long"}),
            ("message.list", {"chat_id": "not-a-chat"}),
            ("draft.set", {"chat_id": "not-a-chat", "text": "orphan"}),
        ]:
            with self.subTest(method=method, size=len(params.get("text", ""))):
                self.assertIn("error", self.raw({"v": 1, "id": "bad", "method": method, "params": params}))
        self.assertGreater(len(self.rpc("chat.list", {"query": ""})["chats"]), 0)
        self.assertEqual(self.rpc("chat.list", {"query": "' OR 1=1 --"})["chats"], [])

    def test_long_unicode_pages_fit_rpc_frame(self):
        chat = self.rpc("chat.list")["chats"][0]["id"]
        for i in range(8):
            self.rpc("message.send", {"chat_id": chat, "text": "🌿" * 16000, "client_id": f"long-{i}"})
        page = self.rpc("message.list", {"chat_id": chat, "limit": 50})
        self.assertTrue(page["messages"])
        self.assertTrue(page["has_more"])
        first = page["messages"][0]
        older = self.rpc("message.list", {"chat_id": chat, "before": {"timestamp": first["timestamp"], "id": first["id"]}})
        self.assertTrue(older["messages"])

    def test_search_context_and_local_attachment_over_real_socket(self):
        import base64
        from urllib.parse import urlparse, unquote
        chat = "demo:alex"
        hits = self.rpc("message.search", {"chat_id": chat, "query": "Morning!"})["messages"]
        self.assertTrue(hits)
        target = hits[-1]["id"]
        context = self.rpc("message.context", {"chat_id": chat, "message_id": target})["messages"]
        self.assertTrue(any(m["id"] == target for m in context))
        self.assertTrue(all(m["chat_id"] == chat for m in context))
        fixture = self.root / "local.gif"
        fixture.write_bytes(base64.b64decode("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"))
        self.assertEqual(self.rpc("attachment.inspect", {"path": str(fixture)})["kind"], "gif")
        result = self.rpc("message.attach", {"chat_id": chat, "path": str(fixture), "text": "GIF fixture", "client_id": "wire-gif"})["message"]
        self.assertEqual(result["status"], "demo_sent")
        stored = Path(unquote(urlparse(result["attachment"]["url"]).path))
        self.assertTrue(stored.is_file())
        self.assertEqual(stored.stat().st_mode & 0o777, 0o600)
        self.assertEqual(stored.read_bytes(), fixture.read_bytes())
        fixture.unlink()
        self.stop()
        self.start()
        messages = self.rpc("message.list", {"chat_id": chat})["messages"]
        self.assertTrue(any(m.get("attachment", {}).get("kind") == "gif" for m in messages))

    def test_video_prepare_and_persist_over_real_rpc(self):
        import urllib.parse
        with tempfile.TemporaryDirectory(prefix="wf-video-") as temp:
            fixture = Path(temp) / "synthetic.mp4"
            command = [
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1",
                "-f", "lavfi", "-i", "testsrc=size=96x64:rate=12:duration=1",
                "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=1",
                "-shortest", "-t", "1", "-c:v", "libx264", "-preset", "ultrafast",
                "-profile:v", "baseline", "-pix_fmt", "yuv420p", "-c:a", "aac",
                "-profile:a", "aac_low", "-b:a", "64k", "-movflags", "+faststart",
                "-y", str(fixture),
            ]
            subprocess.run(command, check=True, capture_output=True)
            original = fixture.read_bytes()
            feature = self.rpc("app.capabilities")["features"]["video"]
            self.assertTrue(feature["available"], feature)
            prepared = self.rpc("attachment.prepare", {"path": str(fixture), "mode": "video"})
            self.assertEqual((prepared["kind"], prepared["mime"], prepared["video_codec"], prepared["audio_codec"]),
                             ("video", "video/mp4", "h264", "aac"))
            self.assertEqual((prepared["width"], prepared["height"]), (96, 64))
            self.assertGreater(prepared["duration"], 0)
            staged = Path(prepared["prepared_path"])
            self.assertEqual(staged.read_bytes(), original)
            self.assertEqual(staged.stat().st_mode & 0o777, 0o600)
            result = self.rpc("message.attach", {
                "chat_id": "demo:alex", "prepared_path": str(staged), "mode": "video",
                "text": "synthetic video caption", "client_id": "video-rpc-persist",
            })["message"]
            self.assertEqual((result["kind"], result["body"], result["status"]),
                             ("video", "synthetic video caption", "demo_sent"))
            media = result["attachment"]
            self.assertEqual((media["kind"], media["mime"], media["video_codec"], media["audio_codec"]),
                             ("video", "video/mp4", "h264", "aac"))
            stored = Path(urllib.parse.unquote(urllib.parse.urlparse(media["url"]).path))
            self.assertEqual(stored.read_bytes(), original)
        self.stop()
        self.start()
        persisted = self.rpc("message.list", {"chat_id": "demo:alex", "limit": 100})["messages"]
        message = next(row for row in persisted if row["id"] == "video-rpc-persist")
        self.assertEqual(message["body"], "synthetic video caption")
        self.assertEqual(message["status"], "demo_sent")
        self.assertEqual(message["attachment"]["kind"], "video")

    def test_hydrated_attachment_pages_fit_private_rpc_budget(self):
        """Synthetic local GIF/caption fixture through the real daemon socket."""
        import base64
        filename = "a" * 175 + '\\"日本語'
        self.assertEqual(len(filename), 180)
        fixture = self.root / filename
        fixture.write_bytes(base64.b64decode("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"))
        chat = "demo:alex"
        expected = {f"budget-rpc-{i:03d}" for i in range(50)}
        for i in range(50):
            result = self.rpc("message.attach", {
                "chat_id": chat,
                "path": str(fixture),
                "text": "caption-" + "x" * 3512,
                "client_id": f"budget-rpc-{i:03d}",
            })["message"]
            self.assertEqual(result["attachment"]["name"], filename)

        seen = set()
        before = None
        while seen != expected:
            params = {"chat_id": chat, "limit": 100}
            if before is not None:
                params["before"] = before
            response = self.raw({"v": 1, "id": "budget-list", "method": "message.list", "params": params})
            self.assertNotIn("error", response, response)
            self.assertLessEqual(len(json.dumps(response, ensure_ascii=False).encode() + b"\n"), 262144)
            page = response["result"]
            rows = page["messages"]
            self.assertTrue(rows)
            for row in rows:
                if row["id"] in expected:
                    seen.add(row["id"])
                    self.assertEqual(row["attachment"]["name"], filename)
            oldest = rows[0]
            before = {"timestamp": oldest["timestamp"], "id": oldest["id"]}
            if not page["has_more"] and seen != expected:
                self.fail("stable cursor stopped before all hydrated fixtures were visible")

        context_response = self.raw({
            "v": 1,
            "id": "budget-context",
            "method": "message.context",
            "params": {"chat_id": chat, "message_id": "budget-rpc-024"},
        })
        self.assertNotIn("error", context_response, context_response)
        self.assertLessEqual(len(json.dumps(context_response, ensure_ascii=False).encode() + b"\n"), 262144)
        context_rows = context_response["result"]["messages"]
        self.assertTrue(any(row["id"] == "budget-rpc-024" for row in context_rows))
        self.assertTrue(all(row.get("attachment", {}).get("name") == filename for row in context_rows if row["id"] in expected))

    def test_second_daemon_cannot_use_same_profile(self):
        duplicate = subprocess.Popen(
            [str(BINARY), "--demo", "--socket", str(self.root / "second.sock"), "--data-dir", str(self.data)],
            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
        )
        try:
            try:
                code = duplicate.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.fail("A second daemon must not open an already owned profile")
            self.assertNotEqual(code, 0)
        finally:
            if duplicate.poll() is None:
                duplicate.terminate()
                duplicate.wait(timeout=5)
            if duplicate.stderr:
                duplicate.stderr.close()

    def test_second_daemon_cannot_replace_an_active_socket(self):
        duplicate = subprocess.run(
            [str(BINARY), "--demo", "--socket", str(self.path), "--data-dir", str(self.root / "other")],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5,
        )
        self.assertNotEqual(duplicate.returncode, 0)
        self.assertEqual(self.rpc("app.status")["state"], "demo")


if __name__ == "__main__":
    unittest.main(verbosity=2)
