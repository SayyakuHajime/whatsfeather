package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestDemoMessageActionsAreDurableAndExplicitlyLocal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = a.Handle(context.Background(), "message.send", json.RawMessage(`{"chat_id":"demo:alex","text":"hello","client_id":"action-source"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		method string
		params string
	}{
		{"message.react", `{"chat_id":"demo:alex","target_id":"action-source","reaction":"👍","client_id":"reaction-1"}`},
		{"message.star", `{"chat_id":"demo:alex","id":"action-source","starred":true}`},
		{"message.pin", `{"chat_id":"demo:alex","id":"action-source","pinned":true,"duration_seconds":3600}`},
		{"chat.update", `{"chat_id":"demo:alex","archived":true,"favorite":true,"list_ids":["work"]}`},
	} {
		result, callErr := a.Handle(context.Background(), request.method, json.RawMessage(request.params))
		if callErr != nil {
			t.Fatalf("%s: %v", request.method, callErr)
		}
		encoded, _ := json.Marshal(result)
		if string(encoded) == "null" {
			t.Fatalf("%s returned no result", request.method)
		}
	}
	page, err := a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:alex","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page)
	var decoded struct {
		Messages []Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	var source *Message
	for i := range decoded.Messages {
		if decoded.Messages[i].ID == "action-source" {
			source = &decoded.Messages[i]
			break
		}
	}
	if source == nil || !source.Starred || !source.Pinned || source.MutationProvenance != "local_only" || source.Reactions == nil || len(*source.Reactions) != 1 || (*source.Reactions)[0].Emoji != "👍" {
		t.Fatalf("local action state not projected truthfully: %#v", source)
	}
	if (*source.Reactions)[0].Provenance != "local_only" {
		t.Fatalf("demo reaction claimed remote sync: %#v", source.Reactions)
	}
	chatResult, err := a.Handle(context.Background(), "chat.list", json.RawMessage(`{"query":"Alex"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(chatResult)
	var chats struct {
		Chats []Chat `json:"chats"`
	}
	if err = json.Unmarshal(encoded, &chats); err != nil {
		t.Fatal(err)
	}
	if len(chats.Chats) != 1 || !chats.Chats[0].Archived || !chats.Chats[0].Favorite || chats.Chats[0].OrganizationProvenance != "local_only" || len(chats.Chats[0].ListIDs) != 1 {
		t.Fatalf("chat organization state missing or overstated: %#v", chats.Chats)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	page, err = a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:alex","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(page)
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range decoded.Messages {
		if message.ID == "action-source" {
			found = true
			if !message.Starred || !message.Pinned || message.Reactions == nil || len(*message.Reactions) != 1 {
				t.Fatalf("reopened message state missing: %#v", message)
			}
		}
	}
	if !found {
		t.Fatal("durable action message did not survive reopening SQLite")
	}
}

func TestDemoReactionReturnsUpdatedMessageProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "message.react", json.RawMessage(`{"chat_id":"demo:alex","target_id":"seed-0-000","reaction":"🔥","client_id":"reaction-projection"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Message Message `json:"message"`
	}
	if err = json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response.Message.ID != "seed-0-000" || response.Message.ChatID != "demo:alex" ||
		response.Message.Reactions == nil || len(*response.Message.Reactions) != 1 ||
		(*response.Message.Reactions)[0].Emoji != "🔥" ||
		(*response.Message.Reactions)[0].Provenance != "local_only" {
		t.Fatalf("reaction response omitted the authoritative updated message: %s", encoded)
	}
}

func TestDemoMessageFlagsReturnAuthoritativeLocalState(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	for _, request := range []struct {
		method string
		params string
	}{
		{"message.star", `{"chat_id":"demo:alex","id":"seed-0-000","starred":true}`},
		{"message.pin", `{"chat_id":"demo:alex","id":"seed-0-000","pinned":true,"duration_seconds":3600}`},
	} {
		result, callErr := a.Handle(context.Background(), request.method, json.RawMessage(request.params))
		if callErr != nil {
			t.Fatalf("%s: %v", request.method, callErr)
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var response struct {
			ChatID     string `json:"chat_id"`
			ID         string `json:"id"`
			Value      bool   `json:"value"`
			Provenance string `json:"provenance"`
		}
		if err = json.Unmarshal(encoded, &response); err != nil {
			t.Fatal(err)
		}
		if response.ID != "seed-0-000" || response.ChatID != "demo:alex" ||
			!response.Value || response.Provenance != "local_only" {
			t.Fatalf("%s response omitted authoritative local-only flag state: %s", request.method, encoded)
		}
	}
}

func TestDemoLocalDeleteReturnsUpdatedMessageProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "message.delete", json.RawMessage(`{"chat_id":"demo:alex","id":"seed-0-000","scope":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Message Message `json:"message"`
	}
	if err = json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response.Message.ID != "seed-0-000" || response.Message.ChatID != "demo:alex" ||
		!response.Message.Deleted || response.Message.Body != "[Message deleted]" ||
		response.Message.MutationProvenance != "local_only" || response.Message.Attachment != nil {
		t.Fatalf("local delete response omitted the authoritative redacted projection: %s", encoded)
	}
}

func TestLocalDeleteRemovesReactionProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = a.Handle(context.Background(), "message.react", json.RawMessage(`{"chat_id":"demo:alex","target_id":"seed-0-000","reaction":"🔥","client_id":"reaction-before-delete"}`)); err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), "message.delete", json.RawMessage(`{"chat_id":"demo:alex","id":"seed-0-000","scope":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Message Message `json:"message"`
	}
	if err = json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if !response.Message.Deleted || (response.Message.Reactions != nil && len(*response.Message.Reactions) != 0) {
		t.Fatalf("local delete left active reactions visible: %s", encoded)
	}
}

func TestIncomingRevokeRemovesReactionProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	const chat, id, actor = "demo:alex", "incoming-revoked", "15551234567@s.whatsapp.net"
	if err = a.Record(ctx, Message{ID: id, ChatID: chat, Body: "incoming text", Timestamp: 1700000000000,
		Kind: "text", Status: "demo_received", SenderID: actor}, "Verified peer"); err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyIncomingReaction(ctx, chat, id, actor, "🔥", 1700000001000); err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyIncomingRevoke(ctx, chat, id, actor); err != nil {
		t.Fatal(err)
	}
	message, err := a.loadTarget(ctx, chat, id)
	if err != nil {
		t.Fatal(err)
	}
	if !message.Deleted || (message.Reactions != nil && len(*message.Reactions) != 0) {
		t.Fatalf("incoming revoke left active reactions visible: %#v", message)
	}
}

func TestMessageDeletionRollsBackOnLocalStorageFailure(t *testing.T) {
	for _, operation := range []string{"local", "incoming-revoke"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			const chatID = "synthetic-delete-chat"
			const messageID = "synthetic-delete-target"
			const actor = "synthetic-actor"
			if err = a.Record(ctx, Message{
				ID: messageID, ChatID: chatID, Body: "before", Timestamp: 1700000000000,
				Kind: "text", Status: "demo_received", SenderID: actor,
				Attachment:   &Attachment{Name: "fixture.png", MIME: "image/png", Kind: "image", URL: "fixture://synthetic", State: "ready", Size: 1},
				MediaPayload: "x",
			}, "Synthetic chat"); err != nil {
				t.Fatal(err)
			}
			if err = a.ApplyIncomingReaction(ctx, chatID, messageID, actor, "🔥", 1700000001000); err != nil {
				t.Fatal(err)
			}
			var attachmentCount int
			if err = a.db.QueryRowContext(ctx, "SELECT count(*) FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&attachmentCount); err != nil {
				t.Fatal(err)
			}
			if attachmentCount != 1 {
				t.Fatalf("fixture attachment count=%d, want 1", attachmentCount)
			}
			if _, err = a.db.Exec(`CREATE TRIGGER reject_attachment_delete BEFORE DELETE ON attachments BEGIN SELECT RAISE(ABORT, 'synthetic storage failure'); END`); err != nil {
				t.Fatal(err)
			}
			if operation == "local" {
				_, err = a.Handle(ctx, "message.delete", json.RawMessage(`{"chat_id":"synthetic-delete-chat","id":"synthetic-delete-target","scope":"local"}`))
			} else {
				err = a.ApplyIncomingRevoke(ctx, chatID, messageID, actor)
			}
			if err == nil {
				t.Fatal("delete succeeded despite injected attachment storage failure")
			}
			message, loadErr := a.loadTarget(ctx, chatID, messageID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			var reactionCount int
			if err = a.db.QueryRowContext(ctx, "SELECT count(*) FROM message_reactions WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&reactionCount); err != nil {
				t.Fatal(err)
			}
			if err = a.db.QueryRowContext(ctx, "SELECT count(*) FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&attachmentCount); err != nil {
				t.Fatal(err)
			}
			if message.Body != "before" || message.Deleted || attachmentCount != 1 || reactionCount != 1 {
				t.Fatalf("failed deletion left a partial projection: message=%#v attachments=%d reactions=%d", message, attachmentCount, reactionCount)
			}
		})
	}
}

func TestLocalDeleteIsScopedToChatAndExplicit(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = a.Handle(context.Background(), "message.delete", json.RawMessage(`{"chat_id":"demo:alex","id":"seed-0-000","scope":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:maya","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(other)
	if string(encoded) == "" || !json.Valid(encoded) {
		t.Fatal("invalid other-chat result")
	}
	_, err = a.Handle(context.Background(), "message.delete", json.RawMessage(`{"chat_id":"demo:alex","id":"seed-0-001","scope":"everyone"}`))
	if err == nil {
		t.Fatal("demo must not claim server-side revoke")
	}
	_, err = a.Handle(context.Background(), "message.delete", json.RawMessage(`{"chat_id":"demo:maya","id":"seed-0-000","scope":"local"}`))
	if err == nil {
		t.Fatal("message ID from another chat must not be deleted")
	}
}

func TestCapabilitiesDistinguishLocalAndUnavailableTransportFeatures(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "app.capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var manifest struct {
		Version  int      `json:"version"`
		Methods  []string `json:"methods"`
		Features map[string]struct {
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		} `json:"features"`
	}
	if err = json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || !containsString(manifest.Methods, "message.reply") || !containsString(manifest.Methods, "message.react") ||
		!containsString(manifest.Methods, "attachment.album.prepare") || !containsString(manifest.Methods, "attachment.album.release") || !containsString(manifest.Methods, "message.album.send") {
		t.Fatalf("capability manifest omits implemented methods: %s", encoded)
	}
	if feature := manifest.Features["multi-attachment-albums"]; !feature.Available || feature.Reason == "" {
		t.Fatalf("demo album feature must declare its bounded local-only availability: %#v", feature)
	}
	if f := manifest.Features["voice-video-calls"]; f.Available || f.Reason == "" {
		t.Fatalf("calls must stay explicitly unavailable: %#v", f)
	}
	if f := manifest.Features["reactions"]; !f.Available || f.Reason == "" {
		t.Fatalf("available reaction feature needs truthful scope: %#v", f)
	}

	live, err := Open(filepath.Join(t.TempDir(), "live-profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	liveResult, err := live.Handle(context.Background(), "app.capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	liveEncoded, _ := json.Marshal(liveResult)
	if err = json.Unmarshal(liveEncoded, &manifest); err != nil {
		t.Fatal(err)
	}
	if feature := manifest.Features["multi-attachment-albums"]; feature.Available || feature.Reason == "" {
		t.Fatalf("live album feature must remain explicitly unavailable: %#v", feature)
	}
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func TestChatJSONKeepsUnreadCountDistinctFromMarkedUnread(t *testing.T) {
	encoded, err := json.Marshal(Chat{ID: "chat-1", Unread: 7, UnreadMark: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := json.Unmarshal(fields["unread"], &count); err != nil || count != 7 {
		t.Fatalf("unread count must remain the numeric unread field, JSON=%s, count=%d, err=%v", encoded, count, err)
	}
	var marked bool
	if err := json.Unmarshal(fields["marked_unread"], &marked); err != nil || !marked {
		t.Fatalf("marked-unread state must be serialized separately, JSON=%s, marked=%t, err=%v", encoded, marked, err)
	}
}

func TestReplyAndEditValidationRejectCrossChatAndNonOwner(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, request := range []struct{ method, params string }{
		{"message.reply", `{"chat_id":"demo:alex","target_id":"missing","text":"reply","client_id":"reply-id"}`},
		{"message.edit", `{"chat_id":"demo:alex","id":"seed-0-000","text":"change","client_id":"edit-id"}`},
	} {
		if _, err := a.Handle(context.Background(), request.method, json.RawMessage(request.params)); err == nil {
			t.Fatalf("%s accepted invalid target/owner", request.method)
		}
	}
}
