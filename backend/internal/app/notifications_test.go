package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func notificationRead(t *testing.T, a *App, cursor string, preview bool) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"cursor": cursor, "include_preview": preview})
	value, err := a.Handle(context.Background(), "notification.poll", raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	var result map[string]any
	if err = json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func notificationLiveRecord(t *testing.T, a *App, m Message) error {
	t.Helper()
	recorder, ok := any(a).(interface {
		RecordLive(context.Context, Message, string) error
	})
	if !ok {
		t.Fatal("positive live-inbound recording path missing")
	}
	return recorder.RecordLive(context.Background(), m, "Synthetic sender")
}
func TestNotificationsOnlyNewCommittedLiveInbound(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first := notificationRead(t, a, "", false)
	cursor := first["cursor"].(string)
	if len(first["events"].([]any)) != 0 {
		t.Fatal("initial subscription must not replay")
	}
	old := Message{ID: "historical", ChatID: "fixture:chat", Body: "old private text", Timestamp: 1, Status: "received", Kind: "text"}
	if err = a.Record(context.Background(), old, "Synthetic"); err != nil {
		t.Fatal(err)
	}
	if err = notificationLiveRecord(t, a, old); err != nil {
		t.Fatal(err)
	}
	out := old
	out.ID = "outgoing"
	out.Outgoing = true
	if err = notificationLiveRecord(t, a, out); err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.ID = "new"
	fresh.Body = "sensitive preview"
	if err = notificationLiveRecord(t, a, fresh); err != nil {
		t.Fatal(err)
	}
	if err = notificationLiveRecord(t, a, fresh); err != nil {
		t.Fatal(err)
	}
	result := notificationRead(t, a, cursor, false)
	events := result["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("wanted one fresh inbound event, got %d", len(events))
	}
	event := events[0].(map[string]any)
	if event["message_id"] != "new" || event["chat_id"] != "fixture:chat" {
		t.Fatal("wrong identity")
	}
	if _, exists := event["body"]; exists {
		t.Fatal("preview exposed without explicit opt-in")
	}
	preview := notificationRead(t, a, cursor, true)["events"].([]any)[0].(map[string]any)
	if preview["body"] != "sensitive preview" {
		t.Fatal("explicit preview not projected")
	}
	if len(notificationRead(t, a, result["cursor"].(string), false)["events"].([]any)) != 0 {
		t.Fatal("cursor did not advance")
	}
}
func TestNotificationsFailClosedAcrossRestartOverflowAndCancel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	cursor := notificationRead(t, a, "", false)["cursor"].(string)
	recorder, ok := any(a).(interface {
		RecordLive(context.Context, Message, string) error
	})
	if !ok {
		t.Fatal("live recorder missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = recorder.RecordLive(ctx, Message{ID: "cancel", ChatID: "fixture:chat", Body: "secret"}, "Synthetic"); err == nil {
		t.Fatal("canceled record accepted")
	}
	for i := 0; i < 140; i++ {
		if err = notificationLiveRecord(t, a, Message{ID: fmt.Sprintf("new-%d", i), ChatID: "fixture:chat", Body: "secret", Timestamp: int64(i + 1), Status: "received", Kind: "text"}); err != nil {
			t.Fatal(err)
		}
	}
	overflow := notificationRead(t, a, cursor, false)
	if len(overflow["events"].([]any)) != 0 || overflow["reset"] != true {
		t.Fatal("overflow must resubscribe without stale notification storm")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	restarted := notificationRead(t, a, overflow["cursor"].(string), true)
	if len(restarted["events"].([]any)) != 0 || restarted["reset"] != true {
		t.Fatal("restart replayed history")
	}
}
func TestNotificationsDemoNeverEmitsAndMutedChatSuppressed(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "demo"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cursor := notificationRead(t, a, "", false)["cursor"].(string)
	if err = notificationLiveRecord(t, a, Message{ID: "demo-new", ChatID: "demo:alex", Body: "local", Timestamp: 1, Kind: "text", Status: "received"}); err != nil {
		t.Fatal(err)
	}
	if len(notificationRead(t, a, cursor, true)["events"].([]any)) != 0 {
		t.Fatal("demo generated notification")
	}
	live, err := Open(filepath.Join(t.TempDir(), "live"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	cursor = notificationRead(t, live, "", false)["cursor"].(string)
	if err = live.Record(context.Background(), Message{ID: "seed", ChatID: "fixture:chat", Body: "seed", Timestamp: 1}, "Synthetic"); err != nil {
		t.Fatal(err)
	}
	if _, err = live.Handle(context.Background(), "chat.update", json.RawMessage(`{"chat_id":"fixture:chat","muted":true}`)); err != nil {
		t.Fatal(err)
	}
	if err = notificationLiveRecord(t, live, Message{ID: "muted-new", ChatID: "fixture:chat", Body: "secret", Timestamp: 2}); err != nil {
		t.Fatal(err)
	}
	if len(notificationRead(t, live, cursor, true)["events"].([]any)) != 0 {
		t.Fatal("muted chat generated notification")
	}
}
