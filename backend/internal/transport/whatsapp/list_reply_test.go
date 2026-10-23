package whatsapp

import (
	"testing"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestBuildListResponseMessageUsesSelectedRowAndOriginalGroupContext(t *testing.T) {
	target := app.Message{
		ID:       "incoming-list-1",
		ChatID:   "12345-67890@g.us",
		Kind:     "list",
		SenderID: "11111:7@s.whatsapp.net",
	}
	message, err := buildListResponseMessage(target, "private-row-2", "Second option", "Details")
	if err != nil {
		t.Fatal(err)
	}
	response := message.GetListResponseMessage()
	if response == nil {
		t.Fatalf("message envelope is not ListResponseMessage: %s", message)
	}
	if response.GetTitle() != "Second option" || response.GetDescription() != "Details" {
		t.Fatalf("response labels = (%q,%q), want selected row labels", response.GetTitle(), response.GetDescription())
	}
	if response.GetListType() != waE2E.ListResponseMessage_SINGLE_SELECT {
		t.Fatalf("list type = %v, want SINGLE_SELECT", response.GetListType())
	}
	if response.GetSingleSelectReply().GetSelectedRowID() != "private-row-2" {
		t.Fatalf("selected row ID = %q", response.GetSingleSelectReply().GetSelectedRowID())
	}
	context := response.GetContextInfo()
	if context.GetStanzaID() != target.ID || context.GetParticipant() != "11111@s.whatsapp.net" {
		t.Fatalf("group response context = stanza %q participant %q", context.GetStanzaID(), context.GetParticipant())
	}
	if _, err = proto.Marshal(message); err != nil {
		t.Fatalf("typed list response does not marshal: %v", err)
	}
}

func TestBuildListResponseMessageRejectsInvalidTargetAndSelection(t *testing.T) {
	valid := app.Message{ID: "list-id", ChatID: "12345@s.whatsapp.net", Kind: "list", SenderID: "12345@s.whatsapp.net"}
	cases := []struct {
		name   string
		target app.Message
		rowID  string
		title  string
	}{
		{"missing-target", app.Message{ChatID: valid.ChatID, Kind: "list", SenderID: valid.SenderID}, "row-1", "Row"},
		{"wrong-kind", app.Message{ID: valid.ID, ChatID: valid.ChatID, Kind: "text", SenderID: valid.SenderID}, "row-1", "Row"},
		{"outgoing-list", app.Message{ID: valid.ID, ChatID: valid.ChatID, Kind: "list", Outgoing: true, SenderID: valid.SenderID}, "row-1", "Row"},
		{"missing-row-id", valid, "", "Row"},
		{"control-row-id", valid, "row\n1", "Row"},
		{"empty-title", valid, "row-1", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildListResponseMessage(tc.target, tc.rowID, tc.title, ""); err == nil {
				t.Fatal("expected invalid list response input to be rejected")
			}
		})
	}
}
