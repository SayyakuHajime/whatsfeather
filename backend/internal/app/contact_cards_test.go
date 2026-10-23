package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestContactPrepareReturnsProjectionWithoutStoringRawVCard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chosen.vcf")
	raw := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Grace Hopper\r\nTEL;waid=15551234567:+1 555 0100\r\nEND:VCARD\r\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(filepath.Join(dir, "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "contact.prepare", mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("explicit local vCard review failed: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "BEGIN:VCARD") || strings.Contains(string(encoded), "waid") || strings.Contains(string(encoded), "@s.whatsapp.net") {
		t.Fatalf("raw vCard or protocol metadata escaped review projection: %s", encoded)
	}
	var response struct {
		Contacts []ContactCard `json:"contacts"`
	}
	if err = json.Unmarshal(encoded, &response); err != nil || len(response.Contacts) != 1 || response.Contacts[0].Name != "Grace Hopper" {
		t.Fatalf("selected file was not parsed into a review projection: %#v (%v)", response, err)
	}
	var saved int
	if err = a.db.QueryRow("SELECT count(*) FROM messages").Scan(&saved); err != nil || saved != 0 {
		t.Fatalf("review stored a message before explicit send: rows=%d err=%v", saved, err)
	}
}

type contactShareProbe struct {
	calls int
	chat  string
	cards []ContactCard
	id    string
	err   error
}

func (p *contactShareProbe) Connect(context.Context) error { return nil }
func (p *contactShareProbe) Disconnect()                   {}
func (p *contactShareProbe) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (p *contactShareProbe) SendContacts(_ context.Context, chat string, cards []ContactCard, id string) (int64, error) {
	p.calls++
	p.chat, p.id = chat, id
	p.cards = append([]ContactCard(nil), cards...)
	return 4321, p.err
}

func TestContactSendUsesTypedStableEnvelopeOnlyOnce(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat := "synthetic-contact-share-chat"
	if err = a.Record(context.Background(), Message{ID: "seed", ChatID: chat, Body: "seed", Kind: "text", Timestamp: 1, Status: "received"}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	transport := &contactShareProbe{}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic transport", "")
	cards := []ContactCard{{Name: "Ada Lovelace", Phones: []string{"+1 555 0100"}, Emails: []string{"ada@example.test"}}}
	params := map[string]any{"chat_id": chat, "contacts": cards, "client_id": "contact-share-stable"}
	first := invoke(t, a, "message.contact.send", params)["message"].(map[string]any)
	if first["id"] != "contact-share-stable" || first["kind"] != "contact" || first["status"] != "accepted" ||
		transport.calls != 1 || transport.chat != chat || transport.id != "contact-share-stable" || !reflect.DeepEqual(transport.cards, cards) {
		t.Fatalf("typed contact send did not complete through fake transport: message=%#v transport=%#v", first, transport)
	}
	second := invoke(t, a, "message.contact.send", params)["message"].(map[string]any)
	if second["id"] != first["id"] || transport.calls != 1 {
		t.Fatalf("same client ID replayed the send: first=%#v second=%#v calls=%d", first, second, transport.calls)
	}
	listed := invoke(t, a, "message.list", map[string]any{"chat_id": chat, "limit": 10})["messages"].([]any)
	if len(listed) != 2 {
		t.Fatalf("message.list omitted the sent projection: %#v", listed)
	}
	found := false
	for _, value := range listed {
		message := value.(map[string]any)
		if message["id"] == "contact-share-stable" {
			found = true
			if strings.Contains(string(mustJSON(t, message)), "BEGIN:VCARD") {
				t.Fatalf("raw vCard was stored or exposed: %#v", message)
			}
		}
	}
	if !found {
		t.Fatal("sent contact was not present in bounded message.list results")
	}
}

func TestContactSendPersistsUnknownAndNeverReplaysSameClientID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	chat := "synthetic-contact-unknown-chat"
	if err = a.Record(context.Background(), Message{ID: "seed", ChatID: chat, Body: "seed", Kind: "text", Timestamp: 1, Status: "received"}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	transport := &contactShareProbe{err: errors.New("ambiguous synthetic transport failure")}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic transport", "")
	params := map[string]any{"chat_id": chat, "contacts": []ContactCard{{Name: "Grace Hopper"}}, "client_id": "contact-unknown-stable"}
	_, err = a.Handle(context.Background(), "message.contact.send", mustJSON(t, params))
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" || transport.calls != 1 {
		t.Fatalf("ambiguous send error=%v calls=%d, want durable unknown", err, transport.calls)
	}
	if err = a.db.QueryRow("SELECT status FROM messages WHERE id=? AND chat_id=?", "contact-unknown-stable", chat).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = a.db.QueryRow("SELECT status FROM messages WHERE id=? AND chat_id=?", "contact-unknown-stable", chat).Scan(&status); err != nil || status != "unknown" {
		t.Fatalf("ambiguous contact status=%q err=%v", status, err)
	}
	var actionStatus string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "contact-unknown-stable").Scan(&actionStatus); err != nil || actionStatus != "unknown" {
		t.Fatalf("durable action status=%q err=%v", actionStatus, err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	replayProbe := &contactShareProbe{}
	a.SetTransport(replayProbe)
	a.SetStatus("connected", "synthetic transport", "")
	message := invoke(t, a, "message.contact.send", params)["message"].(map[string]any)
	if message["status"] != "unknown" || replayProbe.calls != 0 {
		t.Fatalf("profile reopen replayed an ambiguous share: message=%#v calls=%d", message, replayProbe.calls)
	}
}

func TestRecordPersistsContactProjectionAcrossProfileReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	projection := []ContactCard{{
		Name: "Ada Lovelace", Phones: []string{"+1 555 0100"},
		Emails: []string{"ada@example.test"}, Organization: "Analytical Engines",
	}}
	if err = a.Record(context.Background(), Message{
		ID: "synthetic-contact", ChatID: "synthetic-contact-chat", Kind: "contact",
		Timestamp: 1234, Status: "received", ContactCards: &projection,
	}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-contact-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "BEGIN:VCARD") || strings.Contains(string(encoded), "waid") {
		t.Fatalf("raw vCard/protocol metadata leaked from message.list: %s", encoded)
	}
	var page struct {
		Messages []Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Kind != "contact" || (page.Messages[0].ContactCards == nil || !reflect.DeepEqual(*page.Messages[0].ContactCards, projection)) {
		t.Fatalf("contact projection did not survive profile reopen: %#v", page.Messages)
	}
}

func TestParseVCardUnfoldsEscapesAndProjectsOnlySafeFields(t *testing.T) {
	input := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Dr. Ada\\, Lovelace\\nAnalytical\\; Engine\r\nTEL;TYPE=CELL;waid=15551234567:+1 (555) 123-4567\r\nEMAIL;TYPE=INTERNET:ada@example.test\r\nORG:Analytical\\, Engines\r\nX-WAID:15551234567\r\nX-JID:15551234567@s.whatsapp.net\r\nNOTE:discard this\\nprivate metadata\r\nEND:VCARD\r\n")
	got, err := ParseVCard(input)
	if err != nil {
		t.Fatalf("ParseVCard() error = %v", err)
	}
	want := []ContactCard{{
		Name:         "Dr. Ada, Lovelace\nAnalytical; Engine",
		Phones:       []string{"+1 (555) 123-4567"},
		Emails:       []string{"ada@example.test"},
		Organization: "Analytical, Engines",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseVCard() = %#v, want %#v", got, want)
	}
}
