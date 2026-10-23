package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type listReplyProbe struct {
	actionProbe
	calls                                         int
	rowID, title, description, targetID, clientID string
	fail                                          bool
}

func (p *listReplyProbe) SendListReply(_ context.Context, target Message, rowID, title, description, clientID string) (int64, error) {
	p.calls++
	p.rowID, p.title, p.description = rowID, title, description
	p.targetID, p.clientID = target.ID, clientID
	if p.fail {
		return 0, errors.New("ambiguous synthetic transport failure")
	}
	return 1234, nil
}

func TestAcceptedListReplyHydratesTerminalStateAndBlocksSecondSelection(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &listReplyProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	projection := ListProjection{Title: "Pick one", ButtonLabel: "Choose", Sections: []ListSection{{Rows: []ListRow{{Title: "First"}, {Title: "Second"}}}}}
	target := Message{ID: "live-list-target", ChatID: "live-list-chat", Body: projection.Title, Status: "received", Kind: "list", SenderID: "15551234567@s.whatsapp.net", List: &projection,
		ListActionRows: &[]ListActionRow{{SectionIndex: 0, RowIndex: 0, RowID: "private-first"}, {SectionIndex: 0, RowIndex: 1, RowID: "private-second"}}}
	if err = a.Record(context.Background(), target, "Synthetic live list"); err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), "list.reply", []byte(`{"chat_id":"live-list-chat","target_id":"live-list-target","section_index":0,"row_index":0,"client_id":"live-list-reply-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 || probe.rowID != "private-first" || probe.title != "First" || probe.targetID != target.ID || probe.clientID != "live-list-reply-1" {
		t.Fatalf("typed transport call = %+v", probe)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-first") || strings.Contains(string(encoded), "private-second") {
		t.Fatalf("list reply result leaked protocol action IDs: %s", encoded)
	}
	page, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"live-list-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	pageJSON, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var messages struct {
		Messages []Message `json:"messages"`
	}
	if err = json.Unmarshal(pageJSON, &messages); err != nil {
		t.Fatal(err)
	}
	var hydratedTarget *Message
	for i := range messages.Messages {
		if messages.Messages[i].ID == target.ID {
			hydratedTarget = &messages.Messages[i]
		}
	}
	if hydratedTarget == nil || hydratedTarget.ListReplyState != "accepted" || hydratedTarget.ListReplyAvailable {
		t.Fatalf("accepted list reply did not hydrate as terminal: %+v; page=%s", hydratedTarget, pageJSON)
	}
	_, err = a.Handle(context.Background(), "list.reply", []byte(`{"chat_id":"live-list-chat","target_id":"live-list-target","section_index":0,"row_index":1,"client_id":"live-list-reply-2"}`))
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "already_replied" || probe.calls != 1 {
		t.Fatalf("second list selection returned %v and transport calls=%d", err, probe.calls)
	}
}

func TestListReplyInDemoUsesPrivateRowMappingAndKeepsIDsOutOfRPC(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	projection := ListProjection{
		Title: "Pick one", ButtonLabel: "Choose",
		Sections: []ListSection{{Title: "Options", Rows: []ListRow{{Title: "First"}, {Title: "Second", Description: "Private display copy"}}}},
	}
	target := Message{
		ID: "synthetic-list-target", ChatID: "synthetic-list-reply-chat", Body: projection.Title,
		Status: "received", Kind: "list", SenderID: "15551234567@s.whatsapp.net", List: &projection,
		ListActionRows: &[]ListActionRow{{SectionIndex: 0, RowIndex: 0, RowID: "private-row-first"}, {SectionIndex: 0, RowIndex: 1, RowID: "private-row-second"}},
	}
	if err = a.Record(context.Background(), target, "Synthetic list reply"); err != nil {
		t.Fatal(err)
	}
	var storedRowID string
	if err = a.db.QueryRow("SELECT row_id FROM message_list_actions WHERE chat_id=? AND message_id=? AND section_index=0 AND row_index=1", target.ChatID, target.ID).Scan(&storedRowID); err != nil {
		t.Fatal(err)
	}
	if storedRowID != "private-row-second" {
		t.Fatalf("private row mapping = %q", storedRowID)
	}
	var projectionJSON string
	if err = a.db.QueryRow("SELECT projection FROM message_lists WHERE chat_id=? AND message_id=?", target.ChatID, target.ID).Scan(&projectionJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(projectionJSON, "private-row-") {
		t.Fatalf("display projection contains action IDs: %s", projectionJSON)
	}

	result, err := a.Handle(context.Background(), "list.reply", []byte(`{"chat_id":"synthetic-list-reply-chat","target_id":"synthetic-list-target","section_index":0,"row_index":1,"client_id":"demo-list-reply-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-row-") {
		t.Fatalf("list reply RPC result leaked an action ID: %s", encoded)
	}
	var response struct {
		Message Message `json:"message"`
		Target  Message `json:"target"`
		Status  string  `json:"status"`
	}
	if err = json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "demo_sent" || response.Message.Body != "Second" ||
		response.Message.Status != "demo_sent" || response.Message.ChatID != target.ChatID ||
		response.Message.ReplyToID != target.ID || response.Target.ID != target.ID {
		t.Fatalf("demo response = %+v", response)
	}
	var actionRows int
	if err = a.db.QueryRow("SELECT count(*) FROM message_actions WHERE client_id=?", "demo-list-reply-1").Scan(&actionRows); err != nil {
		t.Fatal(err)
	}
	if actionRows != 0 {
		t.Fatalf("demo selection created %d remote action rows", actionRows)
	}
	page, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-list-reply-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	pageJSON, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pageJSON), "private-row-") || !strings.Contains(string(pageJSON), `"list_reply_available":true`) {
		t.Fatalf("hydrated page leaked IDs or omitted action availability: %s", pageJSON)
	}
}

func TestListReplyRejectsOutOfRangeIndicesWithoutLocalSend(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	projection := ListProjection{Title: "Pick", ButtonLabel: "Choose", Sections: []ListSection{{Rows: []ListRow{{Title: "One"}}}}}
	if err = a.Record(context.Background(), Message{
		ID: "synthetic-list-target", ChatID: "synthetic-list-reply-chat", Body: projection.Title,
		Status: "received", Kind: "list", List: &projection,
		ListActionRows: &[]ListActionRow{{SectionIndex: 0, RowIndex: 0, RowID: "private-one"}},
	}, "Synthetic list reply"); err != nil {
		t.Fatal(err)
	}
	_, err = a.Handle(context.Background(), "list.reply", []byte(`{"chat_id":"synthetic-list-reply-chat","target_id":"synthetic-list-target","section_index":0,"row_index":1,"client_id":"demo-list-reply-bad"}`))
	var appErr *Error
	if err == nil || !errors.As(err, &appErr) || appErr.Code != "invalid_params" {
		t.Fatalf("out-of-range list index returned %v, want invalid_params", err)
	}
	var replies int
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE client_id=?", "demo-list-reply-bad").Scan(&replies); err != nil {
		t.Fatal(err)
	}
	if replies != 0 {
		t.Fatalf("invalid list selection persisted %d replies", replies)
	}
}
