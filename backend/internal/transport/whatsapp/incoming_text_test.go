package whatsapp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestIncomingTextEventPersistsAcrossReopen(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := types.ParseJID("12345-67890@g.us")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	adapter := New(a, t.TempDir())
	adapter.event(ctx, nil, &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender},
			ID:            "synthetic-incoming-text",
			Timestamp:     time.UnixMilli(1700000000000),
			PushName:      "Synthetic sender",
		},
		Message: &waE2E.Message{Conversation: proto.String("synthetic incoming text")},
	})
	assertStored := func() {
		t.Helper()
		result, callErr := a.Handle(ctx, "message.list", []byte(`{"chat_id":"12345-67890@g.us","limit":10}`))
		if callErr != nil {
			t.Fatal(callErr)
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var page struct {
			Messages []struct {
				ID         string `json:"id"`
				Body       string `json:"body"`
				Timestamp  int64  `json:"timestamp"`
				Outgoing   bool   `json:"outgoing"`
				Status     string `json:"status"`
				Kind       string `json:"kind"`
				SenderName string `json:"sender_name"`
			} `json:"messages"`
		}
		if err = json.Unmarshal(encoded, &page); err != nil {
			t.Fatal(err)
		}
		for _, message := range page.Messages {
			if message.ID == "synthetic-incoming-text" {
				if message.Body != "synthetic incoming text" || message.Timestamp != 1700000000000 || message.Outgoing || message.Status != "received" || message.Kind != "text" || message.SenderName != "Synthetic sender" {
					t.Fatalf("incoming message projection mismatch: %#v", message)
				}
				return
			}
		}
		t.Fatalf("incoming text event was not persisted: %#v", page.Messages)
	}
	assertStored()
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	assertStored()
}
