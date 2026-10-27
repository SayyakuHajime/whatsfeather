package app

import (
	"context"
	"encoding/json"
	"image/color"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAlbumURLsFollowProfileRelocation(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old-mount")
	newRoot := filepath.Join(base, "new-mount")
	first := writeAlbumPNG(t, base, "first.png", color.RGBA{R: 220, A: 255})
	second := writeAlbumPNG(t, base, "second.png", color.RGBA{B: 220, A: 255})

	a, err := Open(oldRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := a.Handle(context.Background(), "attachment.album.prepare", json.RawMessage(mustAlbumJSON(t, map[string]any{"paths": []string{first, second}})))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	var staged AlbumPrepareResult
	decodeResult(t, prepared, &staged)
	preparedPaths := make([]string, len(staged.Items))
	for i, item := range staged.Items {
		preparedPaths[i] = item.PreparedPath
	}
	if _, err = a.Handle(context.Background(), "message.album.send", json.RawMessage(mustAlbumJSON(t, map[string]any{
		"chat_id": "demo:alex", "prepared_paths": preparedPaths, "text": "relocation album", "client_id": "album-relocation",
	}))); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	a, err = Open(newRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	reprepared, err := a.Handle(context.Background(), "attachment.album.prepare", json.RawMessage(mustAlbumJSON(t, map[string]any{"paths": []string{first, second}})))
	if err != nil {
		t.Fatal(err)
	}
	var retryPreparation AlbumPrepareResult
	decodeResult(t, reprepared, &retryPreparation)
	retryPaths := []string{retryPreparation.Items[0].PreparedPath, retryPreparation.Items[1].PreparedPath}
	replayed, err := a.Handle(context.Background(), "message.album.send", json.RawMessage(mustAlbumJSON(t, map[string]any{
		"chat_id": "demo:alex", "prepared_paths": retryPaths, "text": "relocation album", "client_id": "album-relocation",
	})))
	if err != nil {
		t.Fatalf("same album snapshot after relocation was not idempotent: %v", err)
	}
	var replay struct {
		Message Message `json:"message"`
	}
	decodeResult(t, replayed, &replay)
	if replay.Message.ID != "album-relocation" {
		t.Fatalf("idempotent album replay returned %q", replay.Message.ID)
	}

	listed, err := a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:alex","limit":50}`))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []Message `json:"messages"`
	}
	decodeResult(t, listed, &page)
	for _, message := range page.Messages {
		if message.ID != "album-relocation" {
			continue
		}
		if message.Album == nil || len(*message.Album) != 2 {
			t.Fatalf("relocated album metadata missing: %#v", message.Album)
		}
		for i, item := range *message.Album {
			parsed, parseErr := url.Parse(item.URL)
			if parseErr != nil || parsed.Scheme != "file" || !strings.HasPrefix(parsed.Path, filepath.Join(newRoot, "media")+string(filepath.Separator)) {
				t.Fatalf("album item %d still targets the previous profile/mount root: %q", i, item.URL)
			}
			if _, err = os.Stat(parsed.Path); err != nil {
				t.Fatalf("relocated album item %d does not resolve to profile media: %v", i, err)
			}
		}
		return
	}
	t.Fatal("synthetic relocation album missing from message.list")
}

func TestMessageListRejectsUnsafeLocalAttachmentReferences(t *testing.T) {
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	mediaRoot := filepath.Join(profile, "media")
	if err := os.MkdirAll(mediaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic cache bytes")
	hash := sha256Hex(data)
	name := hash + ".pdf"
	outside := filepath.Join(base, "foreign.pdf")
	if err := os.WriteFile(outside, data, 0600); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(mediaRoot, name)
	if err := os.WriteFile(cachePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	cases := []struct {
		id, rawURL, state string
	}{
		{id: "foreign", rawURL: "file:///foreign/media/" + name, state: "ready"},
		{id: "traversal", rawURL: "file:///old/media/../outside/" + name, state: "ready"},
		{id: "malformed", rawURL: "file:relative/" + name, state: "ready"},
		{id: "file-host", rawURL: "file://localhost/media/v1/" + name, state: "ready"},
		{id: "symlink", rawURL: fileURL(filepath.Join(mediaRoot, name)), state: "ready"},
		{id: "remote-file-url", rawURL: "file:///media/v1/" + name, state: "remote"},
		{id: "remote-url", rawURL: "https://media.invalid/" + name, state: "remote"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if tc.id == "symlink" {
				if err := os.Remove(cachePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, cachePath); err != nil {
					t.Fatal(err)
				}
			}
			message := Message{
				ID: tc.id, ChatID: "demo:alex", Body: "synthetic", Timestamp: time.Now().Add(time.Hour).UnixMilli(),
				Status: "received", Kind: "document",
				Attachment: &Attachment{Name: "fixture.pdf", Size: int64(len(data)), MIME: "application/pdf", Kind: "document", SHA256: hash, State: tc.state, URL: tc.rawURL},
			}
			if err := a.Record(context.Background(), message, "Synthetic"); err != nil {
				t.Fatal(err)
			}
			result, err := a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:alex","limit":50}`))
			if err != nil {
				t.Fatal(err)
			}
			var page struct {
				Messages []Message `json:"messages"`
			}
			decodeResult(t, result, &page)
			for _, listed := range page.Messages {
				if listed.ID == tc.id {
					if listed.Attachment == nil || listed.Attachment.URL != "" || listed.Attachment.State != "remote" {
						t.Fatalf("unsafe or remote metadata produced a local attachment URL: %#v", listed.Attachment)
					}
					return
				}
			}
			ids := make([]string, 0, len(page.Messages))
			for _, listed := range page.Messages {
				ids = append(ids, listed.ID)
			}
			t.Fatalf("message %q was not returned by message.list (IDs: %v)", tc.id, ids)
		})
	}
}

func TestLegacyAttachmentURLFailsClosedAfterRelocation(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old-mount")
	newRoot := filepath.Join(base, "new-mount")
	source := filepath.Join(base, "synthetic.pdf")
	if err := os.WriteFile(source, []byte("%PDF-1.4\nLegacy synthetic media\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(oldRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), "message.attach", json.RawMessage(mustAlbumJSON(t, map[string]any{
		"chat_id": "demo:alex", "path": source, "text": "legacy relocation", "client_id": "legacy-relocation", "mode": "original",
	})))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	var sent struct {
		Message Message `json:"message"`
	}
	decodeResult(t, result, &sent)
	if sent.Message.Attachment == nil {
		a.Close()
		t.Fatal("synthetic attachment missing")
	}
	parsed, err := url.Parse(sent.Message.Attachment.URL)
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	var raw string
	if err = a.db.QueryRow("SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", "demo:alex", sent.Message.ID).Scan(&raw); err != nil {
		a.Close()
		t.Fatal(err)
	}
	var legacy Attachment
	if err = json.Unmarshal([]byte(raw), &legacy); err != nil {
		a.Close()
		t.Fatal(err)
	}
	legacy.URL = fileURL(filepath.Join(oldRoot, "media", filepath.Base(parsed.Path)))
	encoded, err := json.Marshal(legacy)
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	if _, err = a.db.Exec("UPDATE attachments SET meta=? WHERE chat_id=? AND message_id=?", string(encoded), "demo:alex", sent.Message.ID); err != nil {
		a.Close()
		t.Fatal(err)
	}
	beforeMove, err := a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:alex","limit":100}`))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	var beforeMovePage struct {
		Messages []Message `json:"messages"`
	}
	decodeResult(t, beforeMove, &beforeMovePage)
	for _, message := range beforeMovePage.Messages {
		if message.ID == sent.Message.ID && (message.Attachment == nil || !strings.HasPrefix(message.Attachment.URL, fileURL(filepath.Join(oldRoot, "media")))) {
			a.Close()
			t.Fatalf("legacy path inside the current profile root was not retained: %#v", message.Attachment)
		}
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	a, err = Open(newRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	listed, err := a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:alex","limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []Message `json:"messages"`
	}
	decodeResult(t, listed, &page)
	for _, message := range page.Messages {
		if message.ID != sent.Message.ID {
			continue
		}
		if message.Attachment == nil || message.Attachment.URL != "" || message.Attachment.State != "remote" {
			t.Fatalf("legacy absolute path outside the relocated profile was not rejected: %#v", message.Attachment)
		}
		return
	}
	t.Fatalf("legacy synthetic message %q missing after relocation", sent.Message.ID)
}

func TestMessageListRejectsUnsafeAlbumItemReferences(t *testing.T) {
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	first := writeAlbumPNG(t, base, "first.png", color.RGBA{R: 200, A: 255})
	second := writeAlbumPNG(t, base, "second.png", color.RGBA{B: 200, A: 255})
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	prepared, err := a.Handle(context.Background(), "attachment.album.prepare", json.RawMessage(mustAlbumJSON(t, map[string]any{"paths": []string{first, second}})))
	if err != nil {
		t.Fatal(err)
	}
	var staged AlbumPrepareResult
	decodeResult(t, prepared, &staged)
	paths := []string{staged.Items[0].PreparedPath, staged.Items[1].PreparedPath}
	if _, err = a.Handle(context.Background(), "message.album.send", json.RawMessage(mustAlbumJSON(t, map[string]any{
		"chat_id": "demo:alex", "prepared_paths": paths, "text": "unsafe reference", "client_id": "unsafe-album-reference",
	}))); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = a.db.QueryRow("SELECT items FROM message_albums WHERE chat_id=? AND message_id=?", "demo:alex", "unsafe-album-reference").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var items []Attachment
	if err = json.Unmarshal([]byte(raw), &items); err != nil || len(items) != 2 {
		t.Fatalf("stored album metadata invalid: %v", err)
	}
	parsed, err := url.Parse(items[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	items[0].URL = fileURL(filepath.Join("/foreign", "profile", "media", filepath.Base(parsed.Path)))
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("UPDATE message_albums SET items=? WHERE chat_id=? AND message_id=?", string(encoded), "demo:alex", "unsafe-album-reference"); err != nil {
		t.Fatal(err)
	}
	listed, err := a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:alex","limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []Message `json:"messages"`
	}
	decodeResult(t, listed, &page)
	for _, message := range page.Messages {
		if message.ID != "unsafe-album-reference" {
			continue
		}
		if message.Album == nil || len(*message.Album) != 2 {
			t.Fatal("album metadata missing")
		}
		if (*message.Album)[0].URL != "" || (*message.Album)[0].State != "remote" {
			t.Fatalf("foreign album metadata produced a local file URL: %#v", (*message.Album)[0])
		}
		return
	}
	t.Fatal("synthetic album missing from message.list")
}

func TestPreparedAttachmentURLIsCanonicalForSpecialPaths(t *testing.T) {
	base := t.TempDir()
	profile := filepath.Join(base, "profile with space")
	source := filepath.Join(base, "synthetic # ? percent%.pdf")
	if err := os.WriteFile(source, []byte("%PDF-1.4\nSynthetic URI fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "attachment.prepare", json.RawMessage(mustAlbumJSON(t, map[string]any{"path": source, "mode": "original"})))
	if err != nil {
		t.Fatal(err)
	}
	var prepared Attachment
	decodeResult(t, result, &prepared)
	parsed, err := url.Parse(prepared.URL)
	if err != nil || parsed.Scheme != "file" || parsed.Path != prepared.PreparedPath || !isLocalFileURL(prepared.URL) {
		t.Fatalf("prepared path was not exposed as a canonical file URI: URL=%q path=%q err=%v", prepared.URL, prepared.PreparedPath, err)
	}
}
