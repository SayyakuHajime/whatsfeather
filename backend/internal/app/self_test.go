package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type syntheticSelfTransport struct {
	account Account
}

func (syntheticSelfTransport) Connect(context.Context) error { return nil }
func (syntheticSelfTransport) Disconnect()                   {}
func (syntheticSelfTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (t syntheticSelfTransport) Account(context.Context) (Account, error) { return t.account, nil }

func TestChatSelfUsesVerifiedNormalizedAccountAndIsIdempotent(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(syntheticSelfTransport{account: Account{Available: true, State: "connected", JID: "15551234567.7:2@s.whatsapp.net"}})
	a.SetStatus("connected", "synthetic", "")
	before := invoke(t, a, "app.status", nil)["revision"].(float64)
	first := invoke(t, a, "chat.self", map[string]any{})["chat"].(map[string]any)
	if first["id"] != "15551234567@s.whatsapp.net" || first["title"] != "(You)" || first["is_group"] != false {
		t.Fatalf("unexpected self chat: %#v", first)
	}
	var messages int
	if err := a.db.QueryRow("SELECT count(*) FROM messages WHERE chat_id=?", first["id"]).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("self chat inserted messages: %d", messages)
	}
	afterCreate := invoke(t, a, "app.status", nil)["revision"].(float64)
	if afterCreate != before+1 {
		t.Fatalf("metadata creation revision=%v, want %v", afterCreate, before+1)
	}
	second := invoke(t, a, "chat.self", map[string]any{})["chat"].(map[string]any)
	if second["id"] != first["id"] || second["title"] != first["title"] || second["timestamp"] != first["timestamp"] {
		t.Fatalf("repeat changed self metadata: first=%#v second=%#v", first, second)
	}
	if got := invoke(t, a, "app.status", nil)["revision"].(float64); got != afterCreate {
		t.Fatalf("idempotent self chat changed revision: %v -> %v", afterCreate, got)
	}
}

func TestChatSelfRejectsDisconnectedAndNonUserAccounts(t *testing.T) {
	cases := []struct {
		name    string
		state   string
		account Account
		want    string
	}{
		{"disconnected", "disconnected", Account{Available: false, State: "disconnected", JID: "15551234567@s.whatsapp.net"}, "disconnected"},
		{"group", "connected", Account{Available: true, State: "connected", JID: "123@g.us"}, "invalid_account"},
		{"broadcast", "connected", Account{Available: true, State: "connected", JID: "status@broadcast"}, "invalid_account"},
		{"missing", "connected", Account{Available: true, State: "connected", JID: "not-a-jid"}, "invalid_account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			a.SetTransport(syntheticSelfTransport{account: tc.account})
			a.SetStatus(tc.state, "synthetic", "")
			_, err = a.Handle(context.Background(), "chat.self", []byte(`{}`))
			var appErr *Error
			if !errors.As(err, &appErr) || appErr.Code != tc.want {
				t.Fatalf("error=%v, want code %s", err, tc.want)
			}
			var chats int
			if dbErr := a.db.QueryRow("SELECT count(*) FROM chats").Scan(&chats); dbErr != nil {
				t.Fatal(dbErr)
			}
			if chats != 0 {
				t.Fatalf("rejected self chat changed chats: %d", chats)
			}
		})
	}
}

func TestChatSelfRejectsClosingState(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	a.SetTransport(syntheticSelfTransport{account: Account{Available: true, State: "connected", JID: "15551234567@s.whatsapp.net"}})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = a.Handle(context.Background(), "chat.self", []byte(`{}`))
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "app_closing" {
		t.Fatalf("error after close=%v, want app_closing", err)
	}
}
