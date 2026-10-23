package whatsapp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func validProtocolList() *waE2E.ListMessage {
	return &waE2E.ListMessage{
		Title:       proto.String("<script>Menu</script> & choices"),
		Description: proto.String("Choose a fixture"),
		ButtonText:  proto.String("<b>Open list</b>"),
		FooterText:  proto.String("Display only"),
		Sections: []*waE2E.ListMessage_Section{{
			Title: proto.String("<i>Options</i>"),
			Rows: []*waE2E.ListMessage_Row{{
				Title:       proto.String("<img src=x onerror=alert(1)>"),
				Description: proto.String("Literal & text"),
				RowID:       proto.String("https://private.invalid/action/row-secret"),
			}},
		}},
	}
}

func TestIncomingListMessageUsesTypedReadOnlyProjection(t *testing.T) {
	protocolList := validProtocolList()
	protocolList.ContextInfo = &waE2E.ContextInfo{
		Participant:   proto.String("Synthetic sender"),
		QuotedMessage: &waE2E.Message{Conversation: proto.String("Quoted preview")},
	}
	content := messageContent(&events.Message{Message: &waE2E.Message{ListMessage: protocolList}})
	if content.kind != "list" || content.body != "<script>Menu</script> & choices" || content.list == nil {
		t.Fatalf("incoming ListMessage was not projected as a typed read-only list: %#v", content)
	}
	want := app.ListProjection{
		Title:       "<script>Menu</script> & choices",
		Description: "Choose a fixture",
		Footer:      "Display only",
		ButtonLabel: "<b>Open list</b>",
		Sections: []app.ListSection{{Title: "<i>Options</i>", Rows: []app.ListRow{{
			Title:       "<img src=x onerror=alert(1)>",
			Description: "Literal & text",
		}}}},
	}
	if got := *content.list; !reflect.DeepEqual(got, want) {
		t.Fatalf("list display projection = %#v, want %#v", got, want)
	}
	encoded, err := json.Marshal(content.list)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"row-secret", "private.invalid", "row_id", "action_id", "url", "ListType", "ProductListInfo"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("list projection retained non-display field %q: %s", forbidden, encoded)
		}
	}
	if content.quotedBody != "Quoted preview" || content.quotedSender != "Synthetic sender" {
		t.Fatalf("list content changed quote-preview behavior: %#v", content)
	}
}

func TestIncomingListActionIDsStaySeparateAndIncompleteMappingsFailClosed(t *testing.T) {
	content := projectedListContent(validProtocolList())
	if len(content.listActionRows) != 1 || content.listActionRows[0] != (app.ListActionRow{SectionIndex: 0, RowIndex: 0, RowID: "https://private.invalid/action/row-secret"}) {
		t.Fatalf("complete list action mapping = %#v", content.listActionRows)
	}
	projectionJSON, err := json.Marshal(content.list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projectionJSON), "row-secret") || strings.Contains(string(projectionJSON), "private.invalid") {
		t.Fatalf("display projection contains protocol action ID: %s", projectionJSON)
	}
	incomplete := validProtocolList()
	incomplete.Sections[0].Rows[0].RowID = nil
	content = projectedListContent(incomplete)
	if content.kind != "list" || content.list == nil || len(content.listActionRows) != 0 {
		t.Fatalf("incomplete action mapping should preserve only inert display content: %#v", content)
	}
}

func TestIncomingListProjectionPersistsPrivateActionMappingsWithoutLeakingIDs(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat, err := types.ParseJID("synthetic-list@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	adapter.record(context.Background(), &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: "valid-list", Timestamp: time.Unix(1, 0)},
		Message: &waE2E.Message{ListMessage: validProtocolList()},
	}, "Synthetic list")
	invalid := validProtocolList()
	invalid.Title = proto.String(string([]byte{0xff}))
	adapter.record(context.Background(), &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: "invalid-list", Timestamp: time.Unix(2, 0)},
		Message: &waE2E.Message{ListMessage: invalid},
	}, "Synthetic list")
	params, err := json.Marshal(map[string]any{"chat_id": chat.String(), "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), "message.list", params)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"row-secret", "private.invalid", "action_id", "row_id"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("message.list leaked list action data %q: %s", forbidden, encoded)
		}
	}
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].ID != "valid-list" || page.Messages[0].List == nil || page.Messages[0].Kind != "list" || !page.Messages[0].ListReplyAvailable {
		t.Fatalf("valid adapter projection did not hydrate through message.list with an actionable mapping: %s", encoded)
	}
	if page.Messages[1].ID != "invalid-list" || page.Messages[1].Kind != "unsupported" || page.Messages[1].List != nil || page.Messages[1].ListReplyAvailable || page.Messages[1].Body != "[Unsupported content — open the official app]" {
		t.Fatalf("invalid adapter projection was not atomically replaced with unsupported content: %s", encoded)
	}
}

func TestInvalidListMessageFailsClosedAtAdapter(t *testing.T) {
	const fallback = "[Unsupported content — open the official app]"
	cases := []struct {
		name string
		edit func(*waE2E.ListMessage)
	}{
		{"invalid-utf8", func(list *waE2E.ListMessage) { list.Title = proto.String(string([]byte{0xff})) }},
		{"unsafe-control", func(list *waE2E.ListMessage) { list.Description = proto.String("line one\nline two") }},
		{"empty-required-button", func(list *waE2E.ListMessage) { list.ButtonText = proto.String("  ") }},
		{"empty-required-row-title", func(list *waE2E.ListMessage) { list.Sections[0].Rows[0].Title = proto.String("") }},
		{"too-many-sections", func(list *waE2E.ListMessage) {
			list.Sections = make([]*waE2E.ListMessage_Section, app.ListSectionMaxCount+1)
			for i := range list.Sections {
				list.Sections[i] = &waE2E.ListMessage_Section{Rows: []*waE2E.ListMessage_Row{{Title: proto.String("row")}}}
			}
		}},
		{"too-many-rows", func(list *waE2E.ListMessage) {
			list.Sections[0].Rows = make([]*waE2E.ListMessage_Row, app.ListRowsPerSectionMax+1)
			for i := range list.Sections[0].Rows {
				list.Sections[0].Rows[i] = &waE2E.ListMessage_Row{Title: proto.String("row")}
			}
		}},
		{"serialized-projection-too-large", func(list *waE2E.ListMessage) {
			list.Description = proto.String(strings.Repeat("🌿", app.ListDescriptionMaxRunes))
			list.Sections[0].Rows[0].Description = proto.String(strings.Repeat("🌿", app.ListRowDescriptionMax))
		}},
		{"unsupported-product-metadata", func(list *waE2E.ListMessage) {
			list.ProductListInfo = &waE2E.ListMessage_ProductListInfo{}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list := validProtocolList()
			tc.edit(list)
			content := messageContent(&events.Message{Message: &waE2E.Message{ListMessage: list}})
			if content.kind != "unsupported" || content.body != fallback || content.list != nil {
				t.Fatalf("invalid list did not fail closed without partial projection: %#v", content)
			}
		})
	}
}
