package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTextCardReopen(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, e := Open(profile, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"interactive", "template"} {
		m := Message{ID: kind, ChatID: "synthetic", Kind: kind, Status: "received"}
		f := reflect.ValueOf(&m).Elem().FieldByName(map[string]string{"interactive": "Interactive", "template": "Template"}[kind])
		if !f.IsValid() {
			t.Fatal("missing typed text card field")
		}
		p := reflect.New(f.Type().Elem())
		p.Elem().FieldByName("Title").SetString("Title")
		p.Elem().FieldByName("Body").SetString("Body")
		f.Set(p)
		if e = a.Record(context.Background(), m, "Synthetic"); e != nil {
			t.Fatal(e)
		}
	}
	a.Close()
	a, e = Open(profile, false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	v, e := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic","limit":10}`))
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(v)
	var page struct {
		Messages []Message `json:"messages"`
	}
	json.Unmarshal(b, &page)
	if len(page.Messages) != 2 {
		t.Fatal(string(b))
	}
	for _, m := range page.Messages {
		if m.Body != "Body" {
			t.Fatal("safe fallback lost", string(b))
		}
	}
}
func TestTextCardValidation(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "profile"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	for _, p := range []TextCardProjection{{}, {Body: "  "}, {Body: string([]byte{255})}, {Body: "a\nb"}, {Body: strings.Repeat("x", 4097)}, {Labels: []string{"1", "2", "3", "4", "5"}}, {Body: "ok", Labels: []string{" "}}, {Body: strings.Repeat("<", 1000)}} {
		if TextCardProjectionValid(p) {
			t.Fatalf("accepted invalid projection: %#v", p)
		}
	}
	for _, m := range []Message{{Kind: "interactive"}, {Kind: "template"}, {Kind: "text", Interactive: &TextCardProjection{Body: "ok"}}, {Kind: "interactive", Interactive: &TextCardProjection{Body: "ok", Labels: []string{"no"}}}, {Kind: "template", Interactive: &TextCardProjection{Body: "ok"}, Template: &TextCardProjection{Body: "ok"}}} {
		m.ID = "bad"
		m.ChatID = "bad"
		if e = a.Record(context.Background(), m, "bad"); e == nil {
			t.Fatal("accepted kind mismatch")
		}
	}
	var n int
	a.db.QueryRow("SELECT count(*) FROM chats WHERE id='bad'").Scan(&n)
	if n != 0 {
		t.Fatal("partial write")
	}
}
func TestTextCardStoredValidationAndCascade(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "profile"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	ctx := context.Background()
	m := Message{ID: "card", ChatID: "synthetic", Kind: "interactive", Interactive: &TextCardProjection{Body: "safe"}}
	for _, raw := range []string{`{"body":"safe","labels":["bad"]}`, `{"body":"safe","url":"secret"}`, `{"body":"safe"} {}`, `{`, strings.Repeat("x", 4097), "{\"body\":\"" + string([]byte{255}) + "\"}"} {
		if e = a.Record(ctx, m, "Synthetic"); e != nil {
			t.Fatal(e)
		}
		a.db.Exec("PRAGMA ignore_check_constraints=ON")
		if _, e = a.db.Exec("UPDATE message_text_cards SET projection=?", raw); e != nil {
			t.Fatal(e)
		}
		if _, e = a.Handle(ctx, "message.list", []byte(`{"chat_id":"synthetic"}`)); e == nil {
			t.Fatalf("accepted corrupt stored JSON %q", raw)
		}
	}
	if e = a.Record(ctx, m, "Synthetic"); e != nil {
		t.Fatal(e)
	}
	a.db.Exec("UPDATE messages SET kind='text' WHERE id='card'")
	if _, e = a.Handle(ctx, "message.list", []byte(`{"chat_id":"synthetic"}`)); e == nil {
		t.Fatal("accepted stored kind mismatch")
	}
	a.db.Exec("DELETE FROM messages WHERE chat_id='synthetic'")
	var n int
	a.db.QueryRow("SELECT count(*) FROM message_text_cards").Scan(&n)
	if n != 0 {
		t.Fatal("cascade failed")
	}
}
func TestTextCardSizeBoundary(t *testing.T) {
	p := TextCardProjection{Body: strings.Repeat("x", 4085)}
	b, _ := json.Marshal(p)
	if len(b) != 4096 || !TextCardProjectionValid(p) {
		t.Fatal("4096 boundary rejected", len(b))
	}
	p.Body += "x"
	if TextCardProjectionValid(p) {
		t.Fatal("4097 accepted")
	}
}
func TestTextCardRejectsAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{`{"body":"one","body":"two"}`, `{"body":"ok","title":null}`, `{"body":"\ud800"}`, `{"body":"ok","labels":null}`, `{"body":"ok","Title":"wrong case"}`} {
		if _, e := decodeStoredTextCardProjection(raw); e == nil {
			t.Fatalf("ambiguous JSON accepted %s", raw)
		}
	}
}
