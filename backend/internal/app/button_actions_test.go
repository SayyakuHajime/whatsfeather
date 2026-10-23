package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type buttonReplyProbe struct {
	actionProbe
	calls                  int
	buttonID, label        string
	targetID, clientID      string
	fail                   bool
}

func (p *buttonReplyProbe) SendButtonReply(_ context.Context, target Message, buttonID, label, clientID string) (int64, error) {
	p.calls++
	p.buttonID, p.label = buttonID, label
	p.targetID, p.clientID = target.ID, clientID
	if p.fail {
		return 0, errors.New("ambiguous synthetic transport failure")
	}
	return 1234, nil
}

func buttonReplyTarget(id, chat string) Message {
	projection := ButtonsProjection{Body: "Choose one", Buttons: []string{"First", "Second"}}
	rows := []ButtonActionRow{{ButtonIndex: 0, ButtonID: "private-button-first"}, {ButtonIndex: 1, ButtonID: "private-button-second"}}
	return Message{
		ID: id, ChatID: chat, Body: projection.Body, Status: "received", Kind: "buttons",
		SenderID: "15551234567@s.whatsapp.net", Buttons: &projection, ButtonActionRows: &rows,
	}
}

func TestAcceptedButtonReplyHydratesTerminalStateAndBlocksSecondSelection(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	probe := &buttonReplyProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	target := buttonReplyTarget("live-buttons-target", "live-buttons-chat")
	if err = a.Record(context.Background(), target, "Synthetic live buttons"); err != nil {
		t.Fatal(err)
	}

	result, err := a.Handle(context.Background(), "button.reply", []byte(`{"chat_id":"live-buttons-chat","target_id":"live-buttons-target","button_index":1,"client_id":"live-button-reply-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 || probe.buttonID != "private-button-second" || probe.label != "Second" || probe.targetID != target.ID || probe.clientID != "live-button-reply-1" {
		t.Fatalf("typed transport call = %+v", probe)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-button-") {
		t.Fatalf("button reply result leaked private action IDs: %s", encoded)
	}
	var response struct {
		Message Message `json:"message"`
		Target  Message `json:"target"`
		Status  string  `json:"status"`
	}
	if err = json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "accepted" || response.Message.Body != "Second" || response.Message.Status != "accepted" ||
		response.Message.ReplyToID != target.ID || response.Target.ButtonReplyState != "accepted" || response.Target.ButtonReplyAvailable {
		t.Fatalf("accepted button response = %+v", response)
	}

	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	page, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"live-buttons-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	pageJSON, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pageJSON), "private-button-") || !strings.Contains(string(pageJSON), `"button_reply_state":"accepted"`) || strings.Contains(string(pageJSON), `"button_reply_available":true`) {
		t.Fatalf("restart hydration leaked IDs or reopened a completed selection: %s", pageJSON)
	}
	_, err = a.Handle(context.Background(), "button.reply", []byte(`{"chat_id":"live-buttons-chat","target_id":"live-buttons-target","button_index":0,"client_id":"live-button-reply-2"}`))
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "already_replied" || probe.calls != 1 {
		t.Fatalf("second button selection returned %v and transport calls=%d", err, probe.calls)
	}
}

func TestUnknownButtonReplyRemainsBlockedWithoutRetry(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &buttonReplyProbe{fail: true}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	target := buttonReplyTarget("unknown-buttons-target", "unknown-buttons-chat")
	if err = a.Record(context.Background(), target, "Synthetic buttons"); err != nil {
		t.Fatal(err)
	}
	_, err = a.Handle(context.Background(), "button.reply", []byte(`{"chat_id":"unknown-buttons-chat","target_id":"unknown-buttons-target","button_index":0,"client_id":"unknown-button-reply-1"}`))
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" || probe.calls != 1 {
		t.Fatalf("ambiguous button response returned %v and transport calls=%d", err, probe.calls)
	}
	page, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"unknown-buttons-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page)
	if !strings.Contains(string(encoded), `"button_reply_state":"unknown"`) || strings.Contains(string(encoded), `"button_reply_available":true`) {
		t.Fatalf("unknown button response was not durably blocked: %s", encoded)
	}
	_, err = a.Handle(context.Background(), "button.reply", []byte(`{"chat_id":"unknown-buttons-chat","target_id":"unknown-buttons-target","button_index":1,"client_id":"unknown-button-reply-2"}`))
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" || probe.calls != 1 {
		t.Fatalf("new client ID bypassed unknown-outcome guard: err=%v calls=%d", err, probe.calls)
	}
}

func TestDemoButtonReplyIsLocalAndPrivateMappingNeverEntersProjection(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	target := buttonReplyTarget("demo-buttons-target", "demo-buttons-chat")
	if err = a.Record(context.Background(), target, "Synthetic buttons"); err != nil {
		t.Fatal(err)
	}
	var storedID string
	if err = a.db.QueryRow("SELECT button_id FROM message_button_actions WHERE chat_id=? AND message_id=? AND button_index=1", target.ChatID, target.ID).Scan(&storedID); err != nil {
		t.Fatal(err)
	}
	if storedID != "private-button-second" {
		t.Fatalf("private button mapping = %q", storedID)
	}
	var projection string
	if err = a.db.QueryRow("SELECT projection FROM message_buttons WHERE chat_id=? AND message_id=?", target.ChatID, target.ID).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(projection, "private-button-") {
		t.Fatalf("display projection contains private action IDs: %s", projection)
	}
	result, err := a.Handle(context.Background(), "button.reply", []byte(`{"chat_id":"demo-buttons-chat","target_id":"demo-buttons-target","button_index":1,"client_id":"demo-button-reply-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-button-") {
		t.Fatalf("demo reply leaked private action IDs: %s", encoded)
	}
	var response struct {
		Message Message `json:"message"`
		Target  Message `json:"target"`
		Status  string  `json:"status"`
	}
	if err = json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "demo_sent" || response.Message.Status != "demo_sent" || response.Message.Body != "Second" ||
		response.Message.ReplyToID != target.ID || response.Target.ButtonReplyState != "local" || response.Target.ButtonReplyAvailable {
		t.Fatalf("demo button response claimed remote acceptance or stayed actionable: %+v", response)
	}
	var actionStatus string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "demo-button-reply-1").Scan(&actionStatus); err != nil {
		t.Fatal(err)
	}
	if actionStatus != "demo_local" {
		t.Fatalf("demo action status = %q, want demo_local", actionStatus)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	page, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"demo-buttons-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	pageJSON, _ := json.Marshal(page)
	if !strings.Contains(string(pageJSON), `"button_reply_state":"local"`) || strings.Contains(string(pageJSON), `"button_reply_available":true`) || strings.Contains(string(pageJSON), "private-button-") {
		t.Fatalf("local state was not durably and privately hydrated: %s", pageJSON)
	}
}

func TestButtonReplyRejectsInvalidIndexAndIncompleteMappings(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	target := buttonReplyTarget("demo-buttons-target", "demo-buttons-chat")
	if err = a.Record(context.Background(), target, "Synthetic buttons"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"chat_id":"demo-buttons-chat","target_id":"demo-buttons-target","button_index":2,"client_id":"bad-button-index"}`,
		`{"chat_id":"demo-buttons-chat","target_id":"demo-buttons-target","button_index":-1,"client_id":"negative-button-index"}`,
		`{"chat_id":"demo-buttons-chat","target_id":"demo-buttons-target","button_index":1.5,"client_id":"fractional-button-index"}`,
	} {
		_, err = a.Handle(context.Background(), "button.reply", []byte(raw))
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Code != "invalid_params" {
			t.Fatalf("invalid button index request %s returned %v", raw, err)
		}
	}
	badRows := []ButtonActionRow{{ButtonIndex: 0, ButtonID: "private-only-first"}}
	incomplete := Message{
		ID: "incomplete-buttons", ChatID: "demo-buttons-chat", Body: "Pick", Status: "received", Kind: "buttons",
		Buttons: target.Buttons, ButtonActionRows: &badRows,
	}
	if err = a.Record(context.Background(), incomplete, "Synthetic buttons"); err == nil {
		t.Fatal("Record accepted an incomplete button action mapping")
	}
	var partial int
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE chat_id=? AND id=?", incomplete.ChatID, incomplete.ID).Scan(&partial); err != nil {
		t.Fatal(err)
	}
	if partial != 0 {
		t.Fatalf("incomplete button mapping partially wrote %d message rows", partial)
	}
	var sent int
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE client_id IN ('bad-button-index','negative-button-index','fractional-button-index')").Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("invalid button indices persisted %d outgoing responses", sent)
	}
}
