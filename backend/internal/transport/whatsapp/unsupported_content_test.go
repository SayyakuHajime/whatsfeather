package whatsapp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestUnknownProtobufContentPersistsSafeFallbackWithoutPayloadLeak(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	chat, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "SYNTHETIC_UNKNOWN_PRIVATE_PAYLOAD"
	unknown := protowire.AppendTag(nil, 536870910, protowire.BytesType)
	unknown = protowire.AppendString(unknown, sentinel)
	message := &waE2E.Message{}
	message.ProtoReflect().SetUnknown(unknown)
	encoded, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var decoded waE2E.Message
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("synthetic future/unknown field did not survive protobuf decoding")
	}

	adapter := New(a, t.TempDir())
	adapter.record(context.Background(), &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat},
			ID:            "synthetic-unknown-fallback",
			Timestamp:     time.Unix(42, 0),
		},
		Message: &decoded,
	}, "Synthetic contact")

	readBack := func() app.Message {
		t.Helper()
		value, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345@s.whatsapp.net","limit":10}`))
		if err != nil {
			t.Fatal(err)
		}
		projection, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(projection), sentinel) {
			t.Fatalf("unknown protobuf payload leaked through RPC projection: %s", projection)
		}
		var page struct {
			Messages []app.Message `json:"messages"`
		}
		if err := json.Unmarshal(projection, &page); err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Messages {
			if row.ID == "synthetic-unknown-fallback" {
				return row
			}
		}
		t.Fatal("unknown-content message was not returned by message.list")
		return app.Message{}
	}
	assertFallback := func(row app.Message) {
		t.Helper()
		if row.Kind != "unsupported" || row.Body != "[Unsupported content — open the official app]" || row.Attachment != nil {
			t.Fatalf("unexpected safe fallback projection: %#v", row)
		}
	}
	assertFallback(readBack())

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	assertFallback(readBack())
}
