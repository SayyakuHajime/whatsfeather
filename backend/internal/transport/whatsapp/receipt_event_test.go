package whatsapp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestOutgoingReceiptEventsPersistMonotonicallyAcrossReopen(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const id = "synthetic-receipt-event"
	if err = a.Record(ctx, app.Message{ID: id, ChatID: chat.String(), Body: "synthetic outgoing text", Timestamp: 1700000000000, Outgoing: true, Status: "accepted", Kind: "text"}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	assertStatus := func(want string) {
		t.Helper()
		result, callErr := a.Handle(ctx, "message.list", []byte(`{"chat_id":"12345@s.whatsapp.net","limit":10}`))
		if callErr != nil {
			t.Fatal(callErr)
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var page struct {
			Messages []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"messages"`
		}
		if err = json.Unmarshal(encoded, &page); err != nil {
			t.Fatal(err)
		}
		for _, message := range page.Messages {
			if message.ID == id {
				if message.Status != want {
					t.Fatalf("receipt projection status=%q, want %q", message.Status, want)
				}
				return
			}
		}
		t.Fatalf("outgoing message %q missing from projection", id)
	}
	assertStatus("accepted")
	adapter.event(ctx, nil, &events.Receipt{MessageSource: types.MessageSource{Chat: chat}, MessageIDs: []string{id}, Type: types.ReceiptTypeDelivered})
	assertStatus("delivered")
	adapter.event(ctx, nil, &events.Receipt{MessageSource: types.MessageSource{Chat: chat}, MessageIDs: []string{id}, Type: types.ReceiptTypeRead})
	assertStatus("read")
	adapter.event(ctx, nil, &events.Receipt{MessageSource: types.MessageSource{Chat: chat}, MessageIDs: []string{id}, Type: types.ReceiptTypeDelivered})
	assertStatus("read")
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	assertStatus("read")
}
