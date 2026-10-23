package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type buttonsTestProjection struct {
	header  string
	body    string
	footer  string
	buttons []string
}

func messageWithButtonsProjection(t *testing.T, id, chat string, projection buttonsTestProjection) Message {
	t.Helper()
	message := Message{ID: id, ChatID: chat, Body: firstButtonsText(projection.body, projection.header), Kind: "buttons", Status: "received"}
	field := reflect.ValueOf(&message).Elem().FieldByName("Buttons")
	if !field.IsValid() || field.Kind() != reflect.Pointer {
		t.Fatalf("Message lacks a typed Buttons projection field")
	}
	value := reflect.New(field.Type().Elem()).Elem()
	for name, text := range map[string]string{"Header": projection.header, "Body": projection.body, "Footer": projection.footer} {
		part := value.FieldByName(name)
		if !part.IsValid() || part.Kind() != reflect.String || !part.CanSet() {
			t.Fatalf("Buttons projection lacks writable %s text field", name)
		}
		part.SetString(text)
	}
	labels := value.FieldByName("Buttons")
	if !labels.IsValid() || labels.Kind() != reflect.Slice || labels.Type().Elem().Kind() != reflect.String || !labels.CanSet() {
		t.Fatalf("Buttons projection lacks a string-label slice")
	}
	items := reflect.MakeSlice(labels.Type(), len(projection.buttons), len(projection.buttons))
	for i, label := range projection.buttons {
		items.Index(i).SetString(label)
	}
	labels.Set(items)
	field.Set(value.Addr())
	return message
}

func firstButtonsText(body, header string) string {
	if body != "" {
		return body
	}
	return header
}

func TestButtonsProjectionPersistsHydratesAndCascades(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	projection := buttonsTestProjection{
		header:  "<b>Choose one</b>",
		body:    "<script>literal body</script>",
		footer:  "Display context only",
		buttons: []string{"<img src=x>", "A & B", "Third label"},
	}
	message := messageWithButtonsProjection(t, "synthetic-buttons", "synthetic-buttons-chat", projection)
	if err = a.Record(context.Background(), message, "Synthetic buttons"); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-buttons-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || string(page.Messages[0]["kind"]) != `"buttons"` {
		t.Fatalf("message.list did not return the typed ButtonsMessage: %s", encoded)
	}
	var projected map[string]json.RawMessage
	if err = json.Unmarshal(page.Messages[0]["buttons"], &projected); err != nil {
		t.Fatalf("message.list omitted the ButtonsMessage projection: %s", encoded)
	}
	var fields struct {
		Header  string   `json:"header"`
		Body    string   `json:"body"`
		Footer  string   `json:"footer"`
		Buttons []string `json:"buttons"`
	}
	if err = json.Unmarshal(page.Messages[0]["buttons"], &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Header != projection.header || fields.Body != projection.body || fields.Footer != projection.footer || !reflect.DeepEqual(fields.Buttons, projection.buttons) {
		t.Fatalf("hydrated ButtonsMessage projection changed: %#v", fields)
	}
	for _, forbidden := range []string{"button-id-secret", "native-flow-secret", "params-secret", "private.invalid", "context-secret", "ButtonID", "NativeFlowInfo"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("message.list leaked non-display button data %q: %s", forbidden, encoded)
		}
	}
	var stored string
	if err = a.db.QueryRow("SELECT projection FROM message_buttons WHERE chat_id=? AND message_id=?", message.ChatID, message.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || len(stored) > 4<<10 || strings.Contains(stored, "button-id-secret") {
		t.Fatalf("stored ButtonsMessage projection is invalid: %d bytes %s", len(stored), stored)
	}
	columns, err := a.db.Query("SELECT name FROM pragma_table_info('message_buttons') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for columns.Next() {
		var name string
		if err = columns.Scan(&name); err != nil {
			columns.Close()
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err = columns.Err(); err != nil {
		columns.Close()
		t.Fatal(err)
	}
	columns.Close()
	if !reflect.DeepEqual(names, []string{"chat_id", "message_id", "projection"}) {
		t.Fatalf("ButtonsMessage table has non-projection columns: %v", names)
	}
	if _, err = a.db.Exec("DELETE FROM messages WHERE id=? AND chat_id=?", message.ID, message.ChatID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err = a.db.QueryRow("SELECT count(*) FROM message_buttons WHERE chat_id=? AND message_id=?", message.ChatID, message.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("ButtonsMessage projection did not cascade with its message: count=%d err=%v", remaining, err)
	}
}

func TestButtonsRecordRejectsInvalidProjectionAtomically(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cases := []struct {
		name       string
		projection buttonsTestProjection
	}{
		{"invalid-utf8", buttonsTestProjection{body: string([]byte{0xff}), buttons: []string{"One"}}},
		{"control-character", buttonsTestProjection{body: "line one\nline two", buttons: []string{"One"}}},
		{"empty-content", buttonsTestProjection{footer: "Footer only", buttons: []string{"One"}}},
		{"empty-label", buttonsTestProjection{body: "Body", buttons: []string{"  "}}},
		{"too-many-labels", buttonsTestProjection{body: "Body", buttons: []string{"1", "2", "3", "4"}}},
		{"overlong-field", buttonsTestProjection{body: strings.Repeat("x", 2049), buttons: []string{"One"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := messageWithButtonsProjection(t, "invalid-"+tc.name, "invalid-"+tc.name, tc.projection)
			if err = a.Record(context.Background(), message, "Synthetic buttons"); err == nil {
				t.Fatal("Record accepted an invalid ButtonsMessage projection")
			}
			var messages, chats int
			if scanErr := a.db.QueryRow("SELECT count(*) FROM messages WHERE id=?", message.ID).Scan(&messages); scanErr != nil {
				t.Fatal(scanErr)
			}
			if scanErr := a.db.QueryRow("SELECT count(*) FROM chats WHERE id=?", message.ChatID).Scan(&chats); scanErr != nil {
				t.Fatal(scanErr)
			}
			if messages != 0 || chats != 0 {
				t.Fatalf("invalid ButtonsMessage partially changed history: messages=%d chats=%d", messages, chats)
			}
		})
	}
}

func TestMessageListRejectsMalformedStoredButtonsProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	projection := buttonsTestProjection{body: "Body", buttons: []string{"One"}}
	message := messageWithButtonsProjection(t, "corrupted-buttons", "corrupted-buttons-chat", projection)
	if err = a.Record(context.Background(), message, "Synthetic buttons"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("PRAGMA ignore_check_constraints=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("UPDATE message_buttons SET projection=? WHERE chat_id=? AND message_id=?", `{"header":"Body","body":"Body","footer":"","buttons":["One"],"button_id":"secret"}`, message.ChatID, message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"corrupted-buttons-chat","limit":10}`)); err == nil {
		t.Fatal("message.list returned a malformed stored ButtonsMessage projection")
	}
}
