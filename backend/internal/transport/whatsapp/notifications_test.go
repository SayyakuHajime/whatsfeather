package whatsapp

import (
	"context"
	"encoding/json"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"path/filepath"
	"testing"
	"time"
)

func TestNotificationEventOriginFiltersHistoryOfflineAndUnavailable(t *testing.T) {
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	read := func(cursor string) (string, []map[string]any) {
		t.Helper()
		p, _ := json.Marshal(map[string]any{"cursor": cursor})
		r, e := a.Handle(context.Background(), "notification.poll", p)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(r)
		var result struct {
			Cursor string           `json:"cursor"`
			Events []map[string]any `json:"events"`
		}
		if e = json.Unmarshal(b, &result); e != nil {
			t.Fatal(e)
		}
		return result.Cursor, result.Events
	}
	cursor, _ := read("")
	chat, _ := types.ParseJID("15550000001@s.whatsapp.net")
	adapter := New(a, t.TempDir())
	makeEvent := func(id string) *events.Message {
		return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: id, Timestamp: time.Now(), PushName: "Synthetic"}, Message: &waE2E.Message{Conversation: proto.String("private test body")}}
	}
	history := makeEvent("history")
	adapter.record(context.Background(), history, "Synthetic")
	source := makeEvent("source-web")
	source.SourceWebMsg = &waWeb.WebMessageInfo{}
	adapter.event(context.Background(), nil, source)
	unavailable := makeEvent("unavailable")
	unavailable.UnavailableRequestID = "request"
	adapter.event(context.Background(), nil, unavailable)
	old := makeEvent("offline-old")
	old.Info.Timestamp = time.Now().Add(-time.Hour)
	adapter.event(context.Background(), nil, old)
	future := makeEvent("future")
	future.Info.Timestamp = time.Now().Add(time.Hour)
	adapter.event(context.Background(), nil, future)
	fresh := makeEvent("fresh")
	adapter.event(context.Background(), nil, fresh)
	adapter.event(context.Background(), nil, fresh)
	_, got := read(cursor)
	if len(got) != 1 || got[0]["message_id"] != "fresh" {
		t.Fatalf("expected fresh live event only, got %#v", got)
	}
	if _, ok := got[0]["body"]; ok {
		t.Fatal("preview leaked")
	}
}
