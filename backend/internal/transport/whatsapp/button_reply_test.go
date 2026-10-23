package whatsapp

import (
	"testing"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestBuildButtonsResponseMessageUsesSelectedButtonAndGroupContext(t *testing.T) {
	target := app.Message{
		ID:       "incoming-buttons-1",
		ChatID:   "12345-67890@g.us",
		Kind:     "buttons",
		SenderID: "11111:7@s.whatsapp.net",
	}
	message, err := buildButtonsResponseMessage(target, "private-button-2", "Continue & review")
	if err != nil {
		t.Fatal(err)
	}
	response := message.GetButtonsResponseMessage()
	if response == nil {
		t.Fatalf("message envelope is not ButtonsResponseMessage: %s", message)
	}
	if response.GetSelectedButtonID() != "private-button-2" ||
		response.GetSelectedDisplayText() != "Continue & review" ||
		response.GetType() != waE2E.ButtonsResponseMessage_DISPLAY_TEXT {
		t.Fatalf("response = id %q, label %q, type %v", response.GetSelectedButtonID(), response.GetSelectedDisplayText(), response.GetType())
	}
	context := response.GetContextInfo()
	if context.GetStanzaID() != target.ID || context.GetParticipant() != "11111@s.whatsapp.net" {
		t.Fatalf("group response context = stanza %q participant %q", context.GetStanzaID(), context.GetParticipant())
	}
	if _, err = proto.Marshal(message); err != nil {
		t.Fatalf("typed ButtonsResponseMessage does not marshal: %v", err)
	}
}

func TestBuildButtonsResponseMessageRejectsInvalidTargetAndSelection(t *testing.T) {
	valid := app.Message{ID: "buttons-id", ChatID: "12345@s.whatsapp.net", Kind: "buttons", SenderID: "12345@s.whatsapp.net"}
	cases := []struct {
		name   string
		target app.Message
		id     string
		label  string
	}{
		{"missing-target", app.Message{ChatID: valid.ChatID, Kind: "buttons", SenderID: valid.SenderID}, "button-1", "Continue"},
		{"wrong-kind", app.Message{ID: valid.ID, ChatID: valid.ChatID, Kind: "text", SenderID: valid.SenderID}, "button-1", "Continue"},
		{"outgoing-target", app.Message{ID: valid.ID, ChatID: valid.ChatID, Kind: "buttons", Outgoing: true, SenderID: valid.SenderID}, "button-1", "Continue"},
		{"deleted-target", app.Message{ID: valid.ID, ChatID: valid.ChatID, Kind: "buttons", Deleted: true, SenderID: valid.SenderID}, "button-1", "Continue"},
		{"missing-button-id", valid, "", "Continue"},
		{"control-button-id", valid, "button\n1", "Continue"},
		{"empty-label", valid, "button-1", "  "},
		{"control-label", valid, "button-1", "Continue\nnow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildButtonsResponseMessage(tc.target, tc.id, tc.label); err == nil {
				t.Fatal("expected invalid ButtonsMessage response input to be rejected")
			}
		})
	}
}
