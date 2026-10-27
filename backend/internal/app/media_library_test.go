package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Every fixture is synthetic and lives below the test process's TMPDIR.
func hubApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	budgetFixtureChat(t, a, "hub:a")
	budgetFixtureChat(t, a, "hub:b")
	return a, dir
}
func hubMedia(t *testing.T, a *App, chat, id, kind, body string, ts int64) {
	t.Helper()
	meta, _ := json.Marshal(Attachment{Name: "fixture", Kind: kind, MIME: "application/octet-stream", State: "remote", URL: "https://invalid.example/descriptor"})
	var storedMeta map[string]any
	if err := json.Unmarshal(meta, &storedMeta); err != nil {
		t.Fatal(err)
	}
	storedMeta["media_key"] = "PRIVATE_DESCRIPTOR_SECRET"
	storedMeta["direct_path"] = "PRIVATE_DESCRIPTOR_SECRET"
	meta, _ = json.Marshal(storedMeta)
	if _, err := a.db.Exec(`INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES(?,?,?,?,0,'received',?)`, id, chat, body, ts, kind); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO attachments(chat_id,message_id,meta,payload) VALUES(?,?,?,?)`, chat, id, string(meta), []byte("PRIVATE_DESCRIPTOR_SECRET")); err != nil {
		t.Fatal(err)
	}
}
func TestMediaLibraryPagesAndFilters(t *testing.T) {
	a, _ := hubApp(t)
	for i := 0; i < 61; i++ {
		kind := "image"
		if i%2 == 1 {
			kind = "gif"
		}
		hubMedia(t, a, "hub:a", fmt.Sprintf("m%03d", i), kind, "caption", 1000)
	}
	hubMedia(t, a, "hub:b", "other", "image", "WRONG_CHAT_SECRET", 1000)
	hubMedia(t, a, "hub:a", "protected", "unsupported", "PROTECTED_SECRET", 1000)
	hubMedia(t, a, "hub:a", "deleted", "image", "DELETED_SECRET", 1000)
	if _, err := a.db.Exec(`INSERT INTO message_state(chat_id,message_id,deleted) VALUES('hub:a','deleted',1)`); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var before any
	for {
		p := map[string]any{"chat_id": "hub:a", "limit": 100}
		if before != nil {
			p["before"] = before
		}
		v, err := budgetCall(a, "media.list", p)
		if err != nil {
			t.Fatal(err)
		}
		r := v.(map[string]any)
		rows := r["messages"].([]Message)
		if len(rows) > 50 || len(rows) == 0 {
			t.Fatalf("bad page size %d", len(rows))
		}
		for i, m := range rows {
			if seen[m.ID] {
				t.Fatal("duplicate")
			}
			seen[m.ID] = true
			if m.ChatID != "hub:a" || m.Attachment == nil || m.Attachment.URL != "" {
				t.Fatal("bad hydration")
			}
			if i > 0 && rows[i-1].ID >= m.ID {
				t.Fatal("unstable order")
			}
		}
		b, _ := json.Marshal(r)
		for _, secret := range []string{"PRIVATE_DESCRIPTOR_SECRET", "WRONG_CHAT_SECRET", "PROTECTED_SECRET", "DELETED_SECRET", "https://"} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("leaked %s", secret)
			}
		}
		if r["provenance"] != "local_cached" {
			t.Fatal("missing provenance")
		}
		if !r["has_more"].(bool) {
			break
		}
		before = r["next_cursor"]
		c := before.(Cursor)
		if c.ID != rows[0].ID {
			t.Fatal("bad cursor")
		}
	}
	if len(seen) != 61 {
		t.Fatalf("lost rows: %d", len(seen))
	}
	for _, kind := range []string{"image", "video", "audio", "document", "gif", "sticker"} {
		v, err := budgetCall(a, "media.list", map[string]any{"chat_id": "hub:a", "kind": kind})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range v.(map[string]any)["messages"].([]Message) {
			if m.Kind != kind {
				t.Fatal("wrong filter")
			}
		}
	}
}
func TestMediaLibraryRejectsInvalidAndWrongChatCursor(t *testing.T) {
	a, _ := hubApp(t)
	hubMedia(t, a, "hub:b", "foreign", "image", "", 7)
	for _, p := range []any{nil, map[string]any{"chat_id": "missing"}, map[string]any{"chat_id": " hub:a"}, map[string]any{"chat_id": "hub:a", "kind": "bogus"}, map[string]any{"chat_id": "hub:a", "limit": -1}, map[string]any{"chat_id": "hub:a", "before": map[string]any{"timestamp": 7, "id": "foreign"}}, map[string]any{"chat_id": "hub:a", "before": map[string]any{"timestamp": 7}}, map[string]any{"chat_id": "hub:a", "extra": true}} {
		if _, err := budgetCall(a, "media.list", p); err == nil {
			t.Fatalf("accepted invalid params: %v", p)
		}
	}
	v, err := budgetCall(a, "media.list", map[string]any{"chat_id": "hub:a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.(map[string]any)["messages"].([]Message)) != 0 || v.(map[string]any)["has_more"] != false {
		t.Fatal("bad empty gallery")
	}
}
func TestMediaLibraryFinalBudgetAndPersistence(t *testing.T) {
	a, dir := hubApp(t)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("m%03d", i)
		hubMedia(t, a, "hub:a", id, "image", strings.Repeat("🌿\"", 1100), 1000)
		meta, _ := json.Marshal(Attachment{Name: strings.Repeat("名", 1000), Kind: "image", URL: "file:///synthetic/fixture.png", State: "ready"})
		if _, err := a.db.Exec(`UPDATE attachments SET meta=? WHERE chat_id='hub:a' AND message_id=?`, string(meta), id); err != nil {
			t.Fatal(err)
		}
	}
	a.Close()
	var err error
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	seen := map[string]bool{}
	var before any
	for {
		p := map[string]any{"chat_id": "hub:a"}
		if before != nil {
			p["before"] = before
		}
		v, err := budgetCall(a, "media.list", p)
		if err != nil {
			t.Fatal(err)
		}
		r := v.(map[string]any)
		b, _ := json.Marshal(r)
		wire, _ := json.Marshal(map[string]any{"v": 1, "id": strings.Repeat("\x01", 128), "result": r})
		if len(b) > pageResultBudget || len(wire)+1 > 262144 {
			t.Fatal("wire budget exceeded")
		}
		rows := r["messages"].([]Message)
		if len(rows) == 0 {
			t.Fatal("no progress")
		}
		for _, m := range rows {
			if seen[m.ID] {
				t.Fatal("duplicate")
			}
			seen[m.ID] = true
			if m.Attachment.URL != "" || m.Attachment.State != "remote" {
				t.Fatal("unowned ready URL was exposed as profile-local media")
			}
		}
		if !r["has_more"].(bool) {
			break
		}
		before = r["next_cursor"]
	}
	if len(seen) != 50 {
		t.Fatal("tie loss")
	}
	hubMedia(t, a, "hub:a", "oversized", "image", strings.Repeat("x", pageResultBudget), 2000)
	if _, err := budgetCall(a, "media.list", map[string]any{"chat_id": "hub:a"}); errorCode(err) != "message_too_large" {
		t.Fatalf("expected bounded error: %v", err)
	}
}

type hubNoTransport struct{ t *testing.T }

func (p *hubNoTransport) Connect(context.Context) error {
	p.t.Error("transport connect called")
	return nil
}
func (p *hubNoTransport) Disconnect() {}
func (p *hubNoTransport) Send(context.Context, string, string, string) (int64, error) {
	p.t.Error("transport send called")
	return 0, nil
}

func TestMediaLibraryCursorShapeAndReadOnly(t *testing.T) {
	a, _ := hubApp(t)
	hubMedia(t, a, "hub:a", "zero", "image", "caption", 0)
	for _, raw := range []string{`{"chat_id":"hub:a","before":{"id":"zero"}}`, `{"chat_id":"hub:a","before":{"id":"zero","timestamp":null}}`, `{"chat_id":"hub:a","before":{"id":"zero","timestamp":0,"timestamp":0}}`, `{"chat_id":"hub:a","chat_id":"hub:b"}`, `{"chat_id":"hub:a","before":null}`} {
		if _, err := a.Handle(context.Background(), "media.list", json.RawMessage(raw)); errorCode(err) != "invalid_params" {
			t.Fatalf("bad cursor accepted: %s err=%v", raw, err)
		}
	}
	var before, after int64
	if err := a.db.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	revision := a.revision
	a.SetTransport(&hubNoTransport{t})
	for _, method := range []string{"media.list", "chat.info"} {
		if _, err := budgetCall(a, method, map[string]any{"chat_id": "hub:a"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.db.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after || a.revision != revision {
		t.Fatal("local read mutated cache")
	}
}

func TestMediaLibraryGlobalStableCrossChatKeyset(t *testing.T) {
	a, _ := hubApp(t)
	if _, err := a.db.Exec(`UPDATE chats SET title=CASE id WHEN 'hub:a' THEN 'Alpha chat' ELSE 'Beta chat' END WHERE id IN ('hub:a','hub:b')`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		chat, id, kind, body string
		ts                   int64
	}{
		{"hub:a", "same", "image", "caption A", 1000},
		{"hub:b", "same", "image", "caption B", 1000},
		{"hub:a", "z-last", "video", "latest in Alpha", 1000},
		{"hub:b", "a-first", "image", "first in Beta", 1000},
	} {
		hubMedia(t, a, row.chat, row.id, row.kind, row.body, row.ts)
	}
	type entry struct {
		ChatID    string `json:"chat_id"`
		ChatTitle string `json:"chat_title"`
		ID        string `json:"id"`
		Timestamp int64  `json:"timestamp"`
		Kind      string `json:"kind"`
		Body      string `json:"body"`
	}
	type cursor struct {
		ChatID    string `json:"chat_id"`
		ID        string `json:"id"`
		Timestamp int64  `json:"timestamp"`
	}
	type page struct {
		Entries    []entry `json:"entries"`
		HasMore    bool    `json:"has_more"`
		NextCursor cursor  `json:"next_cursor"`
		Provenance string  `json:"provenance"`
	}
	var got []entry
	var before any
	for {
		params := map[string]any{"limit": 2}
		if before != nil {
			params["before"] = before
		}
		value, err := budgetCall(a, "media.library", params)
		if err != nil {
			t.Fatalf("global library query without selected chat: %v", err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var p page
		if err = json.Unmarshal(encoded, &p); err != nil {
			t.Fatal(err)
		}
		if p.Provenance != "local_cached" || len(p.Entries) == 0 || len(p.Entries) > 2 {
			t.Fatalf("unexpected bounded global page: %s", encoded)
		}
		got = append(got, p.Entries...)
		if !p.HasMore {
			break
		}
		before = p.NextCursor
		if p.NextCursor == (cursor{}) {
			t.Fatalf("missing composite cursor: %s", encoded)
		}
	}
	want := []entry{
		{"hub:a", "Alpha chat", "z-last", 1000, "video", "latest in Alpha"},
		{"hub:a", "Alpha chat", "same", 1000, "image", "caption A"},
		{"hub:b", "Beta chat", "same", 1000, "image", "caption B"},
		{"hub:b", "Beta chat", "a-first", 1000, "image", "first in Beta"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestMediaLibraryOrderIndexSurvivesExistingProfileReopen(t *testing.T) {
	a, dir := hubApp(t)
	hubMedia(t, a, "hub:b", "migration-media", "image", "preserved", 1234)
	if _, err := a.db.Exec(`DROP INDEX media_library_order`); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var indexCount int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM pragma_index_list('messages') WHERE name='media_library_order'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatal("global media order index was not restored on an existing profile")
	}
	value, err := budgetCall(reopened, "media.library", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	entries := value.(map[string]any)["entries"].([]MediaLibraryEntry)
	found := false
	for _, entry := range entries {
		if entry.ChatID == "hub:b" && entry.ID == "migration-media" && entry.Body == "preserved" {
			found = true
		}
	}
	if !found {
		t.Fatal("existing cached message was lost while restoring the media index")
	}
}

func TestMediaLibraryGlobalCapFilterAndCursorValidation(t *testing.T) {
	a, _ := hubApp(t)
	for i := 0; i < 55; i++ {
		chat := "hub:a"
		if i%2 == 1 {
			chat = "hub:b"
		}
		hubMedia(t, a, chat, fmt.Sprintf("global-%03d", i), "image", "caption", 100)
	}
	hubMedia(t, a, "hub:b", "global-video", "video", "video caption", 100)
	value, err := budgetCall(a, "media.library", map[string]any{"limit": 500})
	if err != nil {
		t.Fatal(err)
	}
	page := value.(map[string]any)
	entries := page["entries"].([]MediaLibraryEntry)
	if len(entries) != 50 || page["has_more"] != true {
		t.Fatalf("requested page was not capped at 50: %d has_more=%v", len(entries), page["has_more"])
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		key := entry.ChatID + "\x00" + entry.ID
		if seen[key] {
			t.Fatalf("duplicate identity in first page: %q", key)
		}
		seen[key] = true
	}
	cursor := page["next_cursor"].(MediaLibraryCursor)
	continued, err := budgetCall(a, "media.library", map[string]any{"limit": 50, "before": cursor})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range continued.(map[string]any)["entries"].([]MediaLibraryEntry) {
		key := entry.ChatID + "\x00" + entry.ID
		if seen[key] {
			t.Fatalf("duplicate identity across keyset pages: %q", key)
		}
		seen[key] = true
	}
	if len(seen) != 56 {
		t.Fatalf("stable keyset continuation lost entries: %d", len(seen))
	}
	filtered, err := budgetCall(a, "media.library", map[string]any{"kind": "video", "limit": 200})
	if err != nil {
		t.Fatal(err)
	}
	filteredEntries := filtered.(map[string]any)["entries"].([]MediaLibraryEntry)
	if len(filteredEntries) != 1 || filteredEntries[0].Kind != "video" {
		t.Fatalf("kind filter not applied: %#v", filteredEntries)
	}
	for _, params := range []map[string]any{
		{"kind": "bogus"}, {"unknown": true}, {"limit": 0},
		{"kind": "image", "before": MediaLibraryCursor{Timestamp: 100, ChatID: "hub:b", ID: "global-video"}},
	} {
		if _, err := budgetCall(a, "media.library", params); errorCode(err) != "invalid_params" {
			t.Fatalf("invalid/foreign global cursor or parameters accepted: %#v err=%v", params, err)
		}
	}
	for _, raw := range []string{
		`{"before":{"timestamp":100,"chat_id":"hub:a","id":"global-000","extra":true}}`,
		`{"before":{"timestamp":100,"chat_id":"hub:a","id":"global-000","id":"global-001"}}`,
		`{"before":null}`, `{"limit":1,"unknown":true}`,
	} {
		if _, err := a.Handle(context.Background(), "media.library", json.RawMessage(raw)); errorCode(err) != "invalid_params" {
			t.Fatalf("malformed global parameters accepted: %s err=%v", raw, err)
		}
	}
}

func TestMediaLibraryGlobalFinalJSONBudget(t *testing.T) {
	a, _ := hubApp(t)
	hubMedia(t, a, "hub:a", "budget-b", "image", strings.Repeat("a", 100000), 1)
	hubMedia(t, a, "hub:b", "budget-a", "image", strings.Repeat("b", 100000), 1)
	value, err := budgetCall(a, "media.library", map[string]any{"limit": 50})
	if err != nil {
		t.Fatal(err)
	}
	page := value.(map[string]any)
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(map[string]any{"v": 1, "id": strings.Repeat("x", 128), "result": page})
	if err != nil {
		t.Fatal(err)
	}
	if len(page["entries"].([]MediaLibraryEntry)) != 1 || page["has_more"] != true ||
		len(encoded) > pageResultBudget || len(wire)+1 > 262144 {
		t.Fatalf("global result exceeded final JSON/frame budget: entries=%d more=%v json=%d frame=%d",
			len(page["entries"].([]MediaLibraryEntry)), page["has_more"], len(encoded), len(wire)+1)
	}
	cursor := page["next_cursor"].(MediaLibraryCursor)
	continued, err := budgetCall(a, "media.library", map[string]any{"before": cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(continued.(map[string]any)["entries"].([]MediaLibraryEntry)) != 1 ||
		continued.(map[string]any)["has_more"] != false {
		t.Fatal("final-budget continuation did not make bounded progress")
	}
	hubMedia(t, a, "hub:a", "too-large", "image", strings.Repeat("x", pageResultBudget), 2)
	if _, err := budgetCall(a, "media.library", map[string]any{}); errorCode(err) != "message_too_large" {
		t.Fatalf("single oversized final global result must fail closed: %v", err)
	}
}

func TestMediaLibraryGlobalFiltersPrivacyAndReadOnly(t *testing.T) {
	a, _ := hubApp(t)
	hubMedia(t, a, "hub:a", "safe", "image", "visible caption", 5)
	hubMedia(t, a, "hub:b", "view-once", "image", "VIEW_ONCE_SECRET", 10)
	hubMedia(t, a, "hub:a", "disappearing", "image", "DISAPPEARING_SECRET", 9)
	hubMedia(t, a, "hub:b", "protected", "image", "PROTECTED_SECRET", 8)
	hubMedia(t, a, "hub:a", "unsupported", "unsupported", "UNSUPPORTED_SECRET", 7)
	hubMedia(t, a, "hub:b", "deleted", "image", "DELETED_SECRET", 6)
	for id, flag := range map[string]string{
		"view-once": "view_once", "disappearing": "ephemeral", "protected": "protected",
	} {
		if _, err := a.db.Exec(`UPDATE attachments SET meta=json_set(meta, '$.'||?, json('true')) WHERE message_id=?`, flag, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Exec(`INSERT INTO message_state(chat_id,message_id,deleted) VALUES('hub:b','deleted',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES('link-only','hub:b','https://invalid.example',11,0,'received','text')`); err != nil {
		t.Fatal(err)
	}
	var beforeChanges, afterChanges int64
	if err := a.db.QueryRow(`SELECT total_changes()`).Scan(&beforeChanges); err != nil {
		t.Fatal(err)
	}
	revision := a.revision
	a.SetTransport(&hubNoTransport{t})
	value, err := budgetCall(a, "media.library", map[string]any{"limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	page := value.(map[string]any)
	entries := page["entries"].([]MediaLibraryEntry)
	if len(entries) != 1 || entries[0].ID != "safe" || entries[0].ChatID != "hub:a" || entries[0].Body != "visible caption" {
		t.Fatalf("unsafe/non-media rows were exposed: %#v", entries)
	}
	encoded, _ := json.Marshal(page)
	for _, secret := range []string{"VIEW_ONCE_SECRET", "DISAPPEARING_SECRET", "PROTECTED_SECRET", "UNSUPPORTED_SECRET", "DELETED_SECRET", "PRIVATE_DESCRIPTOR", "https://"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("global media projection leaked %s", secret)
		}
	}
	if err := a.db.QueryRow(`SELECT total_changes()`).Scan(&afterChanges); err != nil {
		t.Fatal(err)
	}
	if beforeChanges != afterChanges || a.revision != revision {
		t.Fatal("global media read mutated local cache")
	}
}

func TestMediaLibraryMixedOrderKindsAndOversizedMetadata(t *testing.T) {
	a, _ := hubApp(t)
	kinds := []string{"image", "video", "audio", "document", "gif", "sticker"}
	for i, kind := range kinds {
		hubMedia(t, a, "hub:a", kind, kind, "caption", int64(i/2))
	}
	for _, kind := range kinds {
		v, err := budgetCall(a, "media.list", map[string]any{"chat_id": "hub:a", "kind": kind, "limit": 1})
		if err != nil {
			t.Fatal(err)
		}
		rows := v.(map[string]any)["messages"].([]Message)
		if len(rows) != 1 || rows[0].Kind != kind {
			t.Fatalf("missing %s", kind)
		}
	}
	var before any
	seen := map[string]bool{}
	var previous *Message
	for {
		p := map[string]any{"chat_id": "hub:a", "limit": 1}
		if before != nil {
			p["before"] = before
		}
		v, err := budgetCall(a, "media.list", p)
		if err != nil {
			t.Fatal(err)
		}
		r := v.(map[string]any)
		rows := r["messages"].([]Message)
		if len(rows) != 1 {
			t.Fatal("bad limit")
		}
		m := rows[0]
		if seen[m.ID] {
			t.Fatal("duplicate")
		}
		seen[m.ID] = true
		if previous != nil && (m.Timestamp > previous.Timestamp || (m.Timestamp == previous.Timestamp && m.ID >= previous.ID)) {
			t.Fatal("bad cursor order")
		}
		previous = &m
		if !r["has_more"].(bool) {
			break
		}
		before = r["next_cursor"]
	}
	if len(seen) != len(kinds) {
		t.Fatal("mixed timestamp tie loss")
	}
	for _, cursor := range []Cursor{{Timestamp: 99, ID: "image"}, {Timestamp: 0, ID: "video"}} {
		if _, err := budgetCall(a, "media.list", map[string]any{"chat_id": "hub:a", "kind": "image", "before": cursor}); errorCode(err) != "invalid_params" {
			t.Fatal("mismatched cursor accepted")
		}
	}
	meta, _ := json.Marshal(Attachment{Name: strings.Repeat("名", pageResultBudget), Kind: "image", State: "remote"})
	if _, err := a.db.Exec(`UPDATE attachments SET meta=? WHERE chat_id='hub:a' AND message_id='image'`, string(meta)); err != nil {
		t.Fatal(err)
	}
	if _, err := budgetCall(a, "media.list", map[string]any{"chat_id": "hub:a", "kind": "image"}); errorCode(err) != "message_too_large" {
		t.Fatalf("oversized metadata error: %v", err)
	}
	var stored string
	if err := a.db.QueryRow(`SELECT meta FROM attachments WHERE chat_id='hub:a' AND message_id='image'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != string(meta) {
		t.Fatal("metadata mutated")
	}
}
