package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestListProjectionPersistsAcrossReopenAndMessageList(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	projection := ListProjection{
		Title:       "<b>Synthetic menu</b>",
		Description: "Choose a fixture",
		Footer:      "Display only",
		ButtonLabel: "<i>Open list</i>",
		Sections: []ListSection{{Title: "<u>Options</u>", Rows: []ListRow{{
			Title:       "<script>literal row</script>",
			Description: "Plain display text",
		}}}},
	}
	message := Message{ID: "synthetic-list", ChatID: "synthetic-list-chat", Body: projection.Title, Kind: "list", Status: "received", List: &projection}
	if err = a.Record(context.Background(), message, "Synthetic list"); err != nil {
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
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-list-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Kind != "list" || page.Messages[0].List == nil || !reflect.DeepEqual(*page.Messages[0].List, projection) {
		t.Fatalf("message.list did not hydrate the persisted display-only projection: %s", encoded)
	}
	var stored string
	if err = a.db.QueryRow("SELECT projection FROM message_lists WHERE chat_id=? AND message_id=?", message.ChatID, message.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || len(stored) > ListProjectionMaxBytes {
		t.Fatalf("stored list projection has invalid size %d", len(stored))
	}
	for _, forbidden := range []string{"row_id", "action_id", "private.invalid", "url"} {
		if strings.Contains(stored, forbidden) {
			t.Fatalf("stored list projection contains non-display field %q: %s", forbidden, stored)
		}
	}
	columns, err := a.db.Query("SELECT name FROM pragma_table_info('message_lists') ORDER BY cid")
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
		t.Fatalf("list table contains unexpected persisted fields: %v", names)
	}
}

func TestListRecordRejectsInvalidProjectionAtomically(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	valid := func() ListProjection {
		return ListProjection{Title: "Menu", ButtonLabel: "Open", Sections: []ListSection{{Rows: []ListRow{{Title: "row"}}}}}
	}
	cases := []struct {
		name string
		edit func(*ListProjection)
	}{
		{"invalid-utf8", func(list *ListProjection) { list.Title = string([]byte{0xff}) }},
		{"unsafe-control", func(list *ListProjection) { list.Footer = "line one\nline two" }},
		{"empty-required-title", func(list *ListProjection) { list.Title = "  " }},
		{"empty-required-button", func(list *ListProjection) { list.ButtonLabel = "" }},
		{"empty-required-row-title", func(list *ListProjection) { list.Sections[0].Rows[0].Title = "" }},
		{"too-many-sections", func(list *ListProjection) {
			list.Sections = make([]ListSection, ListSectionMaxCount+1)
			for i := range list.Sections {
				list.Sections[i] = ListSection{Rows: []ListRow{{Title: "row"}}}
			}
		}},
		{"too-many-rows", func(list *ListProjection) {
			list.Sections[0].Rows = make([]ListRow, ListRowsPerSectionMax+1)
			for i := range list.Sections[0].Rows {
				list.Sections[0].Rows[i].Title = "row"
			}
		}},
		{"serialized-projection-too-large", func(list *ListProjection) {
			list.Sections[0].Rows = make([]ListRow, ListRowsPerSectionMax)
			for i := range list.Sections[0].Rows {
				list.Sections[0].Rows[i] = ListRow{Title: "row", Description: strings.Repeat("🌿", ListRowDescriptionMax)}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projection := valid()
			tc.edit(&projection)
			message := Message{ID: "invalid-" + tc.name, ChatID: "invalid-" + tc.name, Body: "Menu", Kind: "list", Status: "received", List: &projection}
			if err = a.Record(context.Background(), message, "Synthetic list"); err == nil {
				t.Fatal("Record accepted an invalid list projection")
			}
			var messages, chats int
			if scanErr := a.db.QueryRow("SELECT count(*) FROM messages WHERE id=?", message.ID).Scan(&messages); scanErr != nil {
				t.Fatal(scanErr)
			}
			if scanErr := a.db.QueryRow("SELECT count(*) FROM chats WHERE id=?", message.ChatID).Scan(&chats); scanErr != nil {
				t.Fatal(scanErr)
			}
			if messages != 0 || chats != 0 {
				t.Fatalf("invalid list partially changed history: messages=%d chats=%d", messages, chats)
			}
		})
	}
}

func TestMessageListRejectsMalformedStoredListProjection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		projection string
	}{
		{"unknown-field", `{"title":"Menu","button_label":"Open","sections":[{"rows":[{"title":"row"}]}],"row_id":"secret"}`},
		{"oversized", strings.Repeat("x", ListProjectionMaxBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			projection := ListProjection{Title: "Menu", ButtonLabel: "Open", Sections: []ListSection{{Rows: []ListRow{{Title: "row"}}}}}
			message := Message{ID: "corrupted-list", ChatID: "stored-list-chat", Body: "Menu", Kind: "list", Status: "received", List: &projection}
			if err = a.Record(context.Background(), message, "Synthetic list"); err != nil {
				t.Fatal(err)
			}
			if _, err = a.db.Exec("PRAGMA ignore_check_constraints=ON"); err != nil {
				t.Fatal(err)
			}
			if _, err = a.db.Exec("UPDATE message_lists SET projection=? WHERE chat_id=? AND message_id=?", tc.projection, message.ChatID, message.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"stored-list-chat","limit":10}`)); err == nil {
				t.Fatal("message.list returned a malformed stored list projection")
			}
		})
	}
}
