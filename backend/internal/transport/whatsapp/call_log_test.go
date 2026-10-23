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
	"google.golang.org/protobuf/proto"
)

func TestCallLogMessageProjectsInertSummary(t *testing.T) {
	unknownOutcome := waE2E.CallLogMessage_CallOutcome(99)
	unknownType := waE2E.CallLogMessage_CallType(99)
	cases := []struct {
		name    string
		message *waE2E.CallLogMessage
		want    string
	}{
		{"missed-video", &waE2E.CallLogMessage{IsVideo: proto.Bool(true), CallOutcome: waE2E.CallLogMessage_MISSED.Enum(), Participants: []*waE2E.CallLogMessage_CallParticipant{{JID: proto.String("private-participant-sentinel")}}}, "Missed video call"},
		{"connected-duration", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_CONNECTED.Enum(), DurationSecs: proto.Int64(65)}, "Voice call · 1:05"},
		{"scheduled-failed", &waE2E.CallLogMessage{IsVideo: proto.Bool(true), CallOutcome: waE2E.CallLogMessage_FAILED.Enum(), CallType: waE2E.CallLogMessage_SCHEDULED_CALL.Enum()}, "Failed scheduled video call"},
		{"voice-chat-duration", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_CONNECTED.Enum(), CallType: waE2E.CallLogMessage_VOICE_CHAT.Enum(), DurationSecs: proto.Int64(3661)}, "Voice chat · 1:01:01"},
		{"accepted-elsewhere", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_ACCEPTED_ELSEWHERE.Enum()}, "Answered elsewhere"},
		{"dnd", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_SILENCED_BY_DND.Enum()}, "Silenced by Do Not Disturb"},
		{"rejected", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_REJECTED.Enum()}, "Declined voice call"},
		{"ongoing-video", &waE2E.CallLogMessage{IsVideo: proto.Bool(true), CallOutcome: waE2E.CallLogMessage_ONGOING.Enum()}, "Ongoing video call"},
		{"silenced-unknown-caller", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_SILENCED_UNKNOWN_CALLER.Enum()}, "Silenced unknown caller"},
		{"negative-duration-omitted", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_CONNECTED.Enum(), DurationSecs: proto.Int64(-1)}, "Voice call"},
		{"oversized-duration-omitted", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_CONNECTED.Enum(), DurationSecs: proto.Int64((24 * 60 * 60) + 1)}, "Voice call"},
		{"missing-outcome", &waE2E.CallLogMessage{}, "Call details unavailable"},
		{"unknown-outcome", &waE2E.CallLogMessage{CallOutcome: unknownOutcome.Enum()}, "Call details unavailable"},
		{"unknown-type", &waE2E.CallLogMessage{CallOutcome: waE2E.CallLogMessage_CONNECTED.Enum(), CallType: unknownType.Enum()}, "Call details unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := messageContent(&events.Message{Message: &waE2E.Message{CallLogMesssage: tc.message}})
			if content.kind != "call" || content.body != tc.want {
				t.Fatalf("call log projection = kind %q body %q, want kind call body %q", content.kind, content.body, tc.want)
			}
			if strings.Contains(content.body, "private-participant-sentinel") {
				t.Fatal("call log summary exposed participant metadata")
			}
		})
	}
}

func TestCallLogSummaryPersistsWithoutParticipantMetadata(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat, err := types.ParseJID("synthetic-call@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	adapter.record(context.Background(), &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: chat},
			ID:            "synthetic-call-log",
			Timestamp:     time.Unix(1, 0),
		},
		Message: &waE2E.Message{CallLogMesssage: &waE2E.CallLogMessage{
			CallOutcome:  waE2E.CallLogMessage_MISSED.Enum(),
			Participants: []*waE2E.CallLogMessage_CallParticipant{{JID: proto.String("private-participant-sentinel")}},
		}},
	}, "Synthetic call")
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-call@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "Missed voice call") {
		t.Fatalf("call summary was not persisted through message.list: %s", encoded)
	}
	if strings.Contains(string(encoded), "private-participant-sentinel") {
		t.Fatalf("participant metadata was persisted or returned: %s", encoded)
	}
}
