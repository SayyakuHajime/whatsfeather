package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These fixtures are synthetic direct-SQL rows. They model valid local media
// metadata and incoming bodies that message.send cannot create at 16k.
func budgetFixtureChat(t *testing.T, a *App, id string) {
	t.Helper()
	if _, err := a.db.Exec("INSERT INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,?,0)", id, "Budget Fixture", "BF", "#54656f"); err != nil {
		t.Fatal(err)
	}
}

func budgetFixtureMessages(t *testing.T, a *App, chat string, count int, body string, withAttachments bool) []string {
	t.Helper()
	var cacheHash string
	var cacheSize int64
	if withAttachments {
		data := []byte("verified budget media")
		cacheHash = sha256Hex(data)
		cacheSize = int64(len(data))
		mediaRoot := filepath.Join(a.dir, "media")
		if err := os.MkdirAll(mediaRoot, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mediaRoot, cacheHash+".gif"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("budget-%03d", i)
		if _, err := a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES(?,?,?,?,0,'received')", ids[i], chat, body, 1000); err != nil {
			t.Fatal(err)
		}
		if withAttachments {
			meta, err := json.Marshal(Attachment{
				Name:   strings.Repeat("a", 176) + `\"` + "日本語",
				Size:   cacheSize,
				MIME:   "image/gif",
				Kind:   "gif",
				URL:    (&url.URL{Scheme: "file", Path: "/media/v1/" + cacheHash + ".gif"}).String(),
				SHA256: cacheHash,
				State:  "ready",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.db.Exec("INSERT INTO attachments(chat_id,message_id,meta) VALUES(?,?,?)", chat, ids[i], string(meta)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ids
}

func budgetCall(a *App, method string, params any) (any, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return a.Handle(context.Background(), method, raw)
}

func errorCode(err error) string {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

func TestHydratedListBudgetPagesMetadataAndCursor(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), strings.Repeat("深", 70)+"日本語", "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat := "budget:list"
	budgetFixtureChat(t, a, chat)
	ids := budgetFixtureMessages(t, a, chat, 50, strings.Repeat("x", 3519)+"🌿", true)

	seen := map[string]bool{}
	var before any
	pageNumber := 0
	for len(seen) < len(ids) {
		pageNumber++
		params := map[string]any{"chat_id": chat, "limit": 100}
		if before != nil {
			params["before"] = before
		}
		value, err := budgetCall(a, "message.list", params)
		if err != nil {
			t.Fatal(err)
		}
		result := value.(map[string]any)
		encodedResult, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if len(encodedResult) > pageResultBudget {
			t.Fatalf("result exceeded app budget: %d", len(encodedResult))
		}
		rows := result["messages"].([]Message)
		if len(rows) == 0 {
			t.Fatal("cursor made no progress")
		}
		for i, row := range rows {
			if seen[row.ID] {
				t.Fatalf("duplicate row %s on page %d rows=%v before=%v", row.ID, pageNumber, rows, before)
			}
			seen[row.ID] = true
			if row.Attachment == nil || !strings.Contains(row.Attachment.Name, "日本語") {
				t.Fatalf("metadata was not hydrated: %#v", row)
			}
			parsedURL, parseErr := url.Parse(row.Attachment.URL)
			if parseErr != nil || !strings.Contains(parsedURL.Path, "日本語") {
				t.Fatalf("profile-local URL was not safely projected: %#v", row.Attachment)
			}
			if i > 0 {
				previous := rows[i-1]
				if previous.Timestamp > row.Timestamp || (previous.Timestamp == row.Timestamp && previous.ID >= row.ID) {
					t.Fatal("page is not ascending")
				}
			}
		}
		oldest := rows[0]
		before = map[string]any{"timestamp": oldest.Timestamp, "id": oldest.ID}
		if !result["has_more"].(bool) {
			break
		}
	}
	if len(seen) != len(ids) {
		t.Fatalf("paging lost messages: got %d want %d", len(seen), len(ids))
	}
}

func TestCenteredContextBudgetRetainsTiedTimestampTargets(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat := "budget:context"
	budgetFixtureChat(t, a, chat)
	ids := budgetFixtureMessages(t, a, chat, 50, strings.Repeat("x", 3519)+"🌿", true)
	for _, index := range []int{0, len(ids) / 2, len(ids) - 1} {
		value, err := budgetCall(a, "message.context", map[string]any{"chat_id": chat, "message_id": ids[index]})
		if err != nil {
			t.Fatal(err)
		}
		result := value.(map[string]any)
		rows := result["messages"].([]Message)
		if len(rows) == 0 || len(rows) >= 50 {
			t.Fatalf("context was not bounded for target %d: %d rows", index, len(rows))
		}
		found := false
		for i, row := range rows {
			if row.ID == ids[index] {
				found = true
			}
			if row.Attachment == nil {
				t.Fatalf("context metadata was not hydrated")
			}
			if i > 0 {
				previous := rows[i-1]
				if previous.Timestamp > row.Timestamp || (previous.Timestamp == row.Timestamp && previous.ID >= row.ID) {
					t.Fatal("context order is not stable")
				}
			}
		}
		if !found {
			t.Fatalf("context target missing at %d: result=%v", index, result)
		}
		first := rows[0]
		var olderCount int
		if err := a.db.QueryRow("SELECT count(*) FROM messages WHERE chat_id=? AND (timestamp<? OR (timestamp=? AND id<?))", chat, first.Timestamp, first.Timestamp, first.ID).Scan(&olderCount); err != nil {
			t.Fatal(err)
		}
		if result["has_more"].(bool) != (olderCount > 0) {
			t.Fatalf("context older availability mismatch at %d: first=%v older=%d result=%v", index, first.ID, olderCount, result)
		}
	}
}

func TestOversizedStoredMessageFailsWithoutChangingDatabase(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat := "budget:oversized"
	budgetFixtureChat(t, a, chat)
	body := strings.Repeat("x", pageResultBudget)
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES('too-big',?,?,?,0,'received')", chat, body, 1); err != nil {
		t.Fatal(err)
	}
	value, err := budgetCall(a, "message.list", map[string]any{"chat_id": chat})
	if err == nil || value != nil || errorCode(err) != "message_too_large" {
		t.Fatalf("expected bounded oversized error, value=%v err=%v", value, err)
	}
	var stored string
	if err = a.db.QueryRow("SELECT body FROM messages WHERE id='too-big' AND chat_id=?", chat).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != body {
		t.Fatal("oversized body changed")
	}
}

func TestListProgressesToAnOversizedOlderMessage(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat := "budget:progress"
	budgetFixtureChat(t, a, chat)
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES('too-big-older',?,?,?,0,'received'),('newer-fit',?,?,?,0,'received')", chat, strings.Repeat("x", pageResultBudget), 1, chat, "fits", 2); err != nil {
		t.Fatal(err)
	}
	value, err := budgetCall(a, "message.list", map[string]any{"chat_id": chat})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	rows := result["messages"].([]Message)
	if len(rows) != 1 || rows[0].ID != "newer-fit" || !result["has_more"].(bool) {
		t.Fatalf("nearer row did not make cursor progress: %#v", result)
	}
	_, err = budgetCall(a, "message.list", map[string]any{"chat_id": chat, "before": map[string]any{"timestamp": rows[0].Timestamp, "id": rows[0].ID}})
	if errorCode(err) != "message_too_large" {
		t.Fatalf("oversized cursor item did not fail explicitly: %v", err)
	}
}

// The streaming guard stops after the first oversized older row. SQLite still
// has to scan/materialize that one raw SQL value; this is not arbitrary corrupt
// payload memory safety or a new inbound-size limit.
func TestListStopsAtFirstOfManyOversizedOlderMessages(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat := "budget:many-oversized"
	budgetFixtureChat(t, a, chat)
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES('small-recent',?,?,?,0,'received')", chat, "fits", 1000); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES(?,?,?,?,0,'received')", fmt.Sprintf("too-big-old-%02d", i), chat, strings.Repeat("x", pageResultBudget), 100-i); err != nil {
			t.Fatal(err)
		}
	}
	value, err := budgetCall(a, "message.list", map[string]any{"chat_id": chat})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	rows := result["messages"].([]Message)
	if len(rows) != 1 || rows[0].ID != "small-recent" || !result["has_more"].(bool) {
		t.Fatalf("recent row did not stop before oversized history: %#v", result)
	}
	_, err = budgetCall(a, "message.list", map[string]any{"chat_id": chat, "before": map[string]any{"timestamp": rows[0].Timestamp, "id": rows[0].ID}})
	if errorCode(err) != "message_too_large" {
		t.Fatalf("oversized older row did not fail on explicit next request: %v", err)
	}
}

func TestOversizedStoredMetadataFailsWithoutChangingDatabase(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat := "budget:metadata"
	budgetFixtureChat(t, a, chat)
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES('metadata-too-big',?,?,?,0,'received')", chat, "small body", 1); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(Attachment{Name: strings.Repeat("名", pageResultBudget), Size: 1, MIME: "image/gif", Kind: "gif", URL: "file:///valid/fixture.gif"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO attachments(chat_id,message_id,meta) VALUES(?,?,?)", chat, "metadata-too-big", string(meta)); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"message.list", map[string]any{"chat_id": chat}},
		{"message.context", map[string]any{"chat_id": chat, "message_id": "metadata-too-big"}},
	} {
		value, callErr := budgetCall(a, call.method, call.params)
		if callErr == nil || value != nil || errorCode(callErr) != "message_too_large" {
			t.Fatalf("%s did not return bounded metadata error: value=%v err=%v", call.method, value, callErr)
		}
	}
	var stored string
	if err = a.db.QueryRow("SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", chat, "metadata-too-big").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != string(meta) {
		t.Fatal("oversized metadata changed")
	}
}

func TestPageBudgetHasExactResultBoundaryAndEnvelopeRoom(t *testing.T) {
	recordPrefix := `{"id":"boundary","body":"`
	recordSuffix := `"}`
	baseRecord := []byte(recordPrefix + recordSuffix)
	bodyBytes := pageResultBudget - pageResultSize([][]byte{baseRecord})
	if bodyBytes < 0 {
		t.Fatal("test record unexpectedly exceeds budget")
	}
	atBoundary := []byte(recordPrefix + strings.Repeat("x", bodyBytes) + recordSuffix)
	if pageResultSize([][]byte{atBoundary}) != pageResultBudget {
		t.Fatalf("boundary accounting mismatch: got %d", pageResultSize([][]byte{atBoundary}))
	}
	result := map[string]any{"messages": []json.RawMessage{atBoundary}, "has_more": false}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encodedResult) != pageResultBudget {
		t.Fatalf("actual result boundary mismatch: got %d want %d", len(encodedResult), pageResultBudget)
	}
	overBoundary := []byte(recordPrefix + strings.Repeat("x", bodyBytes+1) + recordSuffix)
	if pageResultSize([][]byte{overBoundary}) <= pageResultBudget {
		t.Fatal("one byte over boundary was accepted")
	}
	identifier := strings.Repeat("\x01", 128)
	response, err := json.Marshal(map[string]any{"v": 1, "id": identifier, "result": json.RawMessage(encodedResult)})
	if err != nil {
		t.Fatal(err)
	}
	if len(response)+1 > 256*1024 {
		t.Fatalf("no RPC envelope room: %d", len(response)+1)
	}
}
