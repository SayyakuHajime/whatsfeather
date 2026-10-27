package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestMessageListHydratesValidMaximumPreparedAlbum(t *testing.T) {
	const chat = "hydration:max-album"
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	budgetFixtureChat(t, a, chat)

	// These opaque synthetic bytes model already-validated prepared image files;
	// this path tests hydration integrity and budgeting, not image decoding.
	const each = int64(5 << 20)
	items := make([]Attachment, 0, maxAlbumItems)
	for i := 0; i < maxAlbumItems; i++ {
		meta := hydrationCacheFixture(t, a, fmt.Sprintf("album-%02d.png", i), strings.Repeat(string(rune('a'+i)), int(each)), true)
		items = append(items, meta)
	}
	var total int64
	for _, item := range items {
		total += item.Size
	}
	if total != maxAlbumPrepared || total <= mediaHydrationVerificationBudget/2 {
		t.Fatalf("fixture total=%d want the declared %d-byte album limit and >20 MiB", total, maxAlbumPrepared)
	}
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES(?,?,?, ?,0,'received','album')", "max-album", chat, "synthetic maximum album", 1); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO message_albums(chat_id,message_id,items,source_tokens) VALUES(?,?,?,'[]')", chat, "max-album", string(encoded)); err != nil {
		t.Fatal(err)
	}

	value, err := budgetCall(a, "message.list", map[string]any{"chat_id": chat, "limit": 10})
	if err != nil {
		t.Fatalf("valid %d MiB album should hydrate within the aggregate media budget: %v", maxAlbumPrepared>>20, err)
	}
	messages := value.(map[string]any)["messages"].([]Message)
	if len(messages) != 1 || messages[0].Album == nil || len(*messages[0].Album) != maxAlbumItems {
		t.Fatalf("maximum album hydration returned %#v", messages)
	}
	for index, item := range *messages[0].Album {
		if item.State != "ready" || item.URL == "" || item.SHA256 != items[index].SHA256 {
			t.Fatalf("album item %d did not retain verified media: %#v", index, item)
		}
	}
}
