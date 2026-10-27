package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hydrationCacheFixture(t *testing.T, a *App, name, data string, album bool) Attachment {
	t.Helper()
	mediaRoot := filepath.Join(a.dir, "media")
	if err := os.MkdirAll(mediaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	bytes := []byte(data)
	hash := sha256Hex(bytes)
	meta := Attachment{
		Name: name, Size: int64(len(bytes)), MIME: "image/png", Kind: "image",
		URL: "file:///media/v1/" + hash + ".png", SHA256: hash, State: "ready",
	}
	if album {
		meta.Mode = "photo"
	}
	if err := os.WriteFile(filepath.Join(mediaRoot, hash+".png"), bytes, 0600); err != nil {
		t.Fatal(err)
	}
	return meta
}

func storeHydrationAttachment(t *testing.T, a *App, chat, id string, meta Attachment, timestamp int64) {
	t.Helper()
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES(?,?,?, ?,0,'received')", id, chat, "synthetic media", timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO attachments(chat_id,message_id,meta) VALUES(?,?,?)", chat, id, string(encoded)); err != nil {
		t.Fatal(err)
	}
}

func TestMessageListVerificationBudgetIsSharedAcrossAttachmentAndAlbumHydration(t *testing.T) {
	const chat = "hydration:shared-budget"
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	budgetFixtureChat(t, a, chat)

	first := hydrationCacheFixture(t, a, "attachment.png", strings.Repeat("a", 15<<20), false)
	second := hydrationCacheFixture(t, a, "album-first.png", strings.Repeat("b", 20<<20), true)
	third := hydrationCacheFixture(t, a, "album-second.png", strings.Repeat("c", 20<<20), true)
	storeHydrationAttachment(t, a, chat, "attachment", first, 10)
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES(?,?,?, ?,0,'received','album')", "album", chat, "synthetic album", 11); err != nil {
		t.Fatal(err)
	}
	items, err := json.Marshal([]Attachment{second, third})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO message_albums(chat_id,message_id,items,source_tokens) VALUES(?,?,?,'[]')", chat, "album", string(items)); err != nil {
		t.Fatal(err)
	}

	value, err := budgetCall(a, "message.list", map[string]any{"chat_id": chat, "limit": 10})
	if errorCode(err) != "media_verification_budget" || value != nil {
		t.Fatalf("expected explicit 50 MiB request-wide verification failure after unique attachment+album hydration, value=%v err=%v", value, err)
	}
}

func TestMessageListHashesRepeatedMediaOnceAcrossHydrationPhases(t *testing.T) {
	const chat = "hydration:deduplicated"
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	budgetFixtureChat(t, a, chat)
	const fileSize = 4 << 20
	meta := hydrationCacheFixture(t, a, "same.png", strings.Repeat("c", fileSize), false)
	for i := 0; i < 50; i++ {
		storeHydrationAttachment(t, a, chat, fmt.Sprintf("attachment-%02d", i), meta, int64(i+1))
	}
	if _, err = a.db.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind) VALUES(?,?,?, ?,0,'received','album')", "album", chat, "synthetic album", 51); err != nil {
		t.Fatal(err)
	}
	albumItem := meta
	albumItem.Mode = "photo"
	items, err := json.Marshal([]Attachment{albumItem, albumItem})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO message_albums(chat_id,message_id,items,source_tokens) VALUES(?,?,?,'[]')", chat, "album", string(items)); err != nil {
		t.Fatal(err)
	}

	var bytesRead int64
	scope := &mediaHydrationScope{read: func(_ context.Context, file *os.File, p []byte) (int, error) {
		n, readErr := file.Read(p)
		bytesRead += int64(n)
		return n, readErr
	}}
	ctx := context.WithValue(context.Background(), mediaHydrationContextKey{}, scope)
	raw, err := json.Marshal(map[string]any{"chat_id": chat, "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(ctx, "message.list", raw)
	if err != nil {
		t.Fatalf("repeated verified file should fit the request budget: %v", err)
	}
	if got := len(result.(map[string]any)["messages"].([]Message)); got != 51 {
		t.Fatalf("hydrated messages=%d want 51", got)
	}
	if bytesRead != fileSize {
		t.Fatalf("repeated same-file hydration read %d bytes, want one %d-byte verification", bytesRead, fileSize)
	}
}

func TestMessageListCancellationInterruptsInFlightMediaRead(t *testing.T) {
	const chat = "hydration:canceled-read"
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	budgetFixtureChat(t, a, chat)
	meta := hydrationCacheFixture(t, a, "cancel.png", strings.Repeat("d", 1<<20), false)
	storeHydrationAttachment(t, a, chat, "attachment", meta, 1)

	entered := make(chan struct{})
	release := make(chan struct{})
	readFinished := make(chan struct{})
	scope := &mediaHydrationScope{read: func(_ context.Context, file *os.File, p []byte) (int, error) {
		close(entered)
		<-release
		defer close(readFinished)
		return file.Read(p)
	}}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), mediaHydrationContextKey{}, scope))
	defer cancel()
	raw, err := json.Marshal(map[string]any{"chat_id": chat, "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(ctx, "message.list", raw)
		result <- callErr
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("media file read did not start")
	}
	cancel()
	select {
	case callErr := <-result:
		close(release)
		<-readFinished
		if !errors.Is(callErr, context.Canceled) {
			t.Fatalf("canceled hydration error=%v; expected context cancellation", callErr)
		}
	case <-time.After(200 * time.Millisecond):
		close(release)
		callErr := <-result
		<-readFinished
		t.Fatalf("in-flight verification did not stop promptly after cancellation; eventual error=%v", callErr)
	}
}

func TestMessageListVerificationBudgetAllowsOneMaximumAttachment(t *testing.T) {
	const chat = "hydration:max-file"
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	budgetFixtureChat(t, a, chat)
	meta := hydrationCacheFixture(t, a, "maximum.png", strings.Repeat("e", attachmentLimit), false)
	storeHydrationAttachment(t, a, chat, "maximum", meta, 1)

	var bytesRead int64
	scope := &mediaHydrationScope{read: func(_ context.Context, file *os.File, p []byte) (int, error) {
		n, readErr := file.Read(p)
		bytesRead += int64(n)
		return n, readErr
	}}
	ctx := context.WithValue(context.Background(), mediaHydrationContextKey{}, scope)
	raw, err := json.Marshal(map[string]any{"chat_id": chat, "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(ctx, "message.list", raw)
	if err != nil {
		t.Fatalf("one maximum-sized attachment should fit the verification budget: %v", err)
	}
	rows := result.(map[string]any)["messages"].([]Message)
	if len(rows) != 1 || rows[0].Attachment == nil || rows[0].Attachment.URL == "" {
		t.Fatalf("maximum-sized local media did not hydrate truthfully: %#v", rows)
	}
	if bytesRead != attachmentLimit {
		t.Fatalf("maximum file read %d bytes, want %d", bytesRead, attachmentLimit)
	}
}
