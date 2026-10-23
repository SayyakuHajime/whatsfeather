package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestMessageListRejectsInvalidStoredLocationProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES(?,?,?,?,0,'received','location')", "invalid-stored-location", "demo:alex", "Location", int64(9223372036854770000)); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO message_locations(chat_id,message_id,latitude,longitude,is_live,caption,name,address) VALUES(?,?,?,?,?,?,?,?)", "demo:alex", "invalid-stored-location", 91.0, 1.0, false, "", "", strings.Repeat("x", LocationTextMaxRunes+1)); err != nil {
		t.Fatal(err)
	}
	_, err = a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"demo:alex","limit":1}`))
	if errorCode(err) != "invalid_location" {
		t.Fatalf("invalid stored projection was returned by message.list: error=%v", err)
	}
}

func TestHydratedLocationPagesStayWithinFinalJSONBudget(t *testing.T) {
	const count = 100
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const chat = "location-budget-chat"
	if _, err = a.db.Exec("INSERT INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,?,0)", chat, "Synthetic budget", "SB", "#54656f"); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("🌿", LocationTextMaxRunes)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("location-budget-%03d", i)
		if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES(?,?,?, ?,0,'received','location')", id, chat, "Location", i+1); err != nil {
			t.Fatal(err)
		}
		if _, err = a.db.Exec("INSERT INTO message_locations(chat_id,message_id,latitude,longitude,is_live,caption,name,address) VALUES(?,?,?,?,?,?,?,?)", chat, id, 47.6, -122.3, false, text, text, text); err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[string]bool, count)
	var before any
	pageNumber := 0
	for len(seen) < count {
		pageNumber++
		params := map[string]any{"chat_id": chat, "limit": count}
		if before != nil {
			params["before"] = before
		}
		value, callErr := a.Handle(context.Background(), "message.list", mustJSON(t, params))
		if callErr != nil {
			t.Fatal(callErr)
		}
		result := value.(map[string]any)
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if len(encoded) > pageResultBudget {
			t.Fatalf("hydrated location page %d exceeded final JSON budget: %d > %d", pageNumber, len(encoded), pageResultBudget)
		}
		rows := result["messages"].([]Message)
		if len(rows) == 0 {
			t.Fatal("location cursor made no progress")
		}
		if pageNumber == 1 && len(rows) == count {
			t.Fatal("page did not account for hydrated location text when applying its final byte budget")
		}
		for i, message := range rows {
			if seen[message.ID] {
				t.Fatalf("duplicate location row %s", message.ID)
			}
			seen[message.ID] = true
			if message.Location == nil || len([]rune(message.Location.Caption)) != LocationTextMaxRunes || !LocationProjectionValid(*message.Location) {
				t.Fatalf("location hydration was not included in the bounded page: %#v", message)
			}
			if i > 0 && (rows[i-1].Timestamp > message.Timestamp || (rows[i-1].Timestamp == message.Timestamp && rows[i-1].ID >= message.ID)) {
				t.Fatalf("page %d is not chronologically ordered: %#v", pageNumber, rows)
			}
		}
		oldest := rows[0]
		before = map[string]any{"timestamp": oldest.Timestamp, "id": oldest.ID}
		if !result["has_more"].(bool) && len(seen) < count {
			t.Fatalf("bounded page stopped after %d of %d location rows", len(seen), count)
		}
	}
	if len(seen) != count {
		t.Fatalf("paged %d unique location messages, want %d", len(seen), count)
	}
}
