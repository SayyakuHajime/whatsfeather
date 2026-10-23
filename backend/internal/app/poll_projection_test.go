package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func pollMessageJSON(id, chat, question string, options []string, count *uint32) Message {
	poll := map[string]any{"question": question, "options": options}
	if count != nil {
		poll["selectable_options_count"] = *count
	}
	encoded, err := json.Marshal(map[string]any{
		"id": id, "chat_id": chat, "body": question, "timestamp": 1234,
		"status": "received", "kind": "poll", "poll": poll,
	})
	if err != nil {
		panic(err)
	}
	var message Message
	if err = json.Unmarshal(encoded, &message); err != nil {
		panic(err)
	}
	return message
}

func TestPollProjectionPersistsAcrossReopenAndMessageList(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	count := uint32(1)
	message := pollMessageJSON("synthetic-poll", "synthetic-poll-chat", "Synthetic question", []string{"Option one", "Option two"}, &count)
	if err = a.Record(context.Background(), message, "Synthetic poll"); err != nil {
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
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-poll-chat","limit":10}`))
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
	if len(page.Messages) != 1 {
		t.Fatalf("message.list returned %d synthetic poll rows: %s", len(page.Messages), encoded)
	}
	var poll map[string]json.RawMessage
	if err = json.Unmarshal(page.Messages[0]["poll"], &poll); err != nil {
		t.Fatalf("message.list omitted the normalized poll projection: %s", encoded)
	}
	var question string
	var options []string
	var selectable uint32
	if err = json.Unmarshal(poll["question"], &question); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(poll["options"], &options); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(poll["selectable_options_count"], &selectable); err != nil {
		t.Fatal(err)
	}
	if question != "Synthetic question" || len(options) != 2 || options[0] != "Option one" || options[1] != "Option two" || selectable != 1 {
		t.Fatalf("poll projection changed across persistence/reopen/list: %#v", poll)
	}
}

func TestPollRecordRejectsInvalidStorageProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	countZero := uint32(0)
	cases := []struct {
		name string
		poll PollProjection
	}{
		{"invalid-utf8", PollProjection{Question: string([]byte{0xff}), Options: []string{"One"}}},
		{"question-control", PollProjection{Question: "Unsafe\nquestion", Options: []string{"One"}}},
		{"option-over-limit", PollProjection{Question: "Question", Options: []string{strings.Repeat("x", PollOptionMaxRunes+1)}}},
		{"too-many-options", PollProjection{Question: "Question", Options: make([]string, PollOptionMaxCount+1)}},
		{"invalid-selectable-count", PollProjection{Question: "Question", Options: []string{"One"}, SelectableOptionsCount: &countZero}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := Message{ID: "invalid-" + tc.name, ChatID: "invalid-poll-chat", Body: "Poll", Kind: "poll", Status: "received", Poll: &tc.poll}
			if err = a.Record(context.Background(), message, "Synthetic poll"); err == nil {
				t.Fatal("Record accepted an invalid typed poll projection")
			}
			var rows int
			if scanErr := a.db.QueryRow("SELECT count(*) FROM messages WHERE id=?", message.ID).Scan(&rows); scanErr != nil || rows != 0 {
				t.Fatalf("invalid poll partially changed history: rows=%d err=%v", rows, scanErr)
			}
		})
	}
	largeQuestion := strings.Repeat("🌿", PollQuestionMaxRunes)
	largeOption := strings.Repeat("🌿", PollOptionMaxRunes)
	options := make([]string, PollOptionMaxCount)
	for i := range options {
		options[i] = largeOption
	}
	oversized := PollProjection{Question: largeQuestion, Options: options}
	if PollProjectionValid(oversized) {
		t.Fatal("PollProjectionValid accepted a projection beyond the serialized storage limit")
	}
	if err = a.Record(context.Background(), Message{ID: "oversized-poll", ChatID: "invalid-poll-chat", Body: "Poll", Kind: "poll", Status: "received", Poll: &oversized}, "Synthetic poll"); err == nil {
		t.Fatal("Record accepted a poll projection beyond the serialized storage limit")
	}
}

func TestMessageListRejectsInvalidStoredPollProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	count := uint32(1)
	message := pollMessageJSON("corrupted-poll", "stored-poll-chat", "Question", []string{"One"}, &count)
	if err = a.Record(context.Background(), message, "Synthetic poll"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("PRAGMA ignore_check_constraints=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("UPDATE message_polls SET projection=? WHERE chat_id=? AND message_id=?", strings.Repeat("x", PollProjectionMaxBytes+1), message.ChatID, message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"stored-poll-chat","limit":10}`)); err == nil {
		t.Fatal("message.list returned an oversized stored poll projection")
	}
}

func TestPollProjectionLegacyRowsAndCascade(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	legacy := Message{ID: "legacy-poll", ChatID: "legacy-poll-chat", Body: "Old plain-text poll question", Kind: "poll", Status: "received"}
	if err = a.Record(context.Background(), legacy, "Legacy poll"); err != nil {
		t.Fatal(err)
	}
	count := uint32(1)
	current := pollMessageJSON("typed-poll", "legacy-poll-chat", "Typed question", []string{"One", "Two"}, &count)
	if err = a.Record(context.Background(), current, "Legacy poll"); err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"legacy-poll-chat","limit":10}`))
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
	legacyFound, typedFound := false, false
	for _, message := range page.Messages {
		var id, body string
		_ = json.Unmarshal(message["id"], &id)
		_ = json.Unmarshal(message["body"], &body)
		switch id {
		case "legacy-poll":
			legacyFound = body == legacy.Body && message["poll"] == nil
		case "typed-poll":
			typedFound = len(message["poll"]) > 0
		}
	}
	if !legacyFound || !typedFound {
		t.Fatalf("legacy plain-text poll compatibility failed or typed poll missing: %s", encoded)
	}
	var stored int
	if err = a.db.QueryRow("SELECT count(*) FROM message_polls WHERE message_id=?", current.ID).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("typed poll was not stored in its projection table: count=%d err=%v", stored, err)
	}
	if _, err = a.db.Exec("DELETE FROM messages WHERE id=? AND chat_id=?", current.ID, current.ChatID); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT count(*) FROM message_polls WHERE message_id=?", current.ID).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("poll projection did not cascade with its message: count=%d err=%v", stored, err)
	}

	// Confirm the normalized table has only the composite key and projection,
	// rather than protocol material or poll-update state columns.
	rows, err := a.db.Query("SELECT name FROM pragma_table_info('message_polls') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(columns) != 3 || columns[0] != "chat_id" || columns[1] != "message_id" || columns[2] != "projection" {
		t.Fatalf("poll table contains unexpected persisted fields: %v", columns)
	}
}

func TestHydratedPollPagesRespectFinalJSONBudget(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	question := strings.Repeat("🌿", PollQuestionMaxRunes)
	option := strings.Repeat("🌿", PollOptionMaxRunes)
	options := make([]string, 7)
	for i := range options {
		options[i] = option
	}
	for i := 0; i < 60; i++ {
		message := pollMessageJSON(fmt.Sprintf("budget-poll-%02d", i), "poll-budget-chat", question, options, nil)
		message.Timestamp = int64(i + 1)
		if err = a.Record(context.Background(), message, "Poll budget"); err != nil {
			t.Fatalf("record poll %d: %v", i, err)
		}
	}
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"poll-budget-chat","limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > pageResultBudget {
		t.Fatalf("hydrated poll page exceeded the final JSON budget: %d > %d", len(encoded), pageResultBudget)
	}
	var page struct {
		Messages []json.RawMessage `json:"messages"`
		HasMore  bool              `json:"has_more"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) == 0 || len(page.Messages) >= 60 || !page.HasMore {
		t.Fatalf("large poll projections were not paginated within the byte budget: messages=%d has_more=%v bytes=%d", len(page.Messages), page.HasMore, len(encoded))
	}
}
