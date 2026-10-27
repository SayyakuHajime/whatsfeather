package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

func TestAlbumReplayRejectsChangedPreparedBytesAfterRestart(t *testing.T) {
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	first := writeAlbumPNG(t, base, "first.png", color.RGBA{R: 200, A: 255})
	second := writeAlbumPNG(t, base, "second.png", color.RGBA{B: 200, A: 255})

	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	preparedValue, err := a.Handle(context.Background(), "attachment.album.prepare", json.RawMessage(mustAlbumJSON(t, map[string]any{"paths": []string{first, second}})))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	var prepared AlbumPrepareResult
	decodeResult(t, preparedValue, &prepared)
	paths := []string{prepared.Items[0].PreparedPath, prepared.Items[1].PreparedPath}
	request := json.RawMessage(mustAlbumJSON(t, map[string]any{
		"chat_id": "demo:alex", "prepared_paths": paths, "text": "restart replay", "client_id": "changed-byte-album-replay",
	}))
	if _, err = a.Handle(context.Background(), "message.album.send", request); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(a.prepared) != 0 {
		t.Fatalf("reopened profile unexpectedly restored %d staged snapshots", len(a.prepared))
	}
	for i, path := range paths {
		replacement := bytes.Repeat([]byte{byte('x' + i)}, int(prepared.Items[i].PreparedSize))
		if err = os.WriteFile(path, replacement, 0600); err != nil {
			t.Fatal(err)
		}
	}
	a.SetTransport(&hubNoTransport{t: t})

	var beforeCount, beforeAlbumCount int
	var beforeItems, beforeTokens string
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE client_id=?", "changed-byte-album-replay").Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT count(*),items,source_tokens FROM message_albums WHERE chat_id=? AND message_id=?", "demo:alex", "changed-byte-album-replay").Scan(&beforeAlbumCount, &beforeItems, &beforeTokens); err != nil {
		t.Fatal(err)
	}
	beforeRevision := a.revision
	value, err := a.Handle(context.Background(), "message.album.send", request)
	var appErr *Error
	if value != nil || !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("same client ID with changed prepared bytes should conflict after restart, value=%v err=%v", value, err)
	}
	if a.revision != beforeRevision {
		t.Fatalf("rejected retry changed revision: before=%d after=%d", beforeRevision, a.revision)
	}
	var afterCount, afterAlbumCount int
	var afterItems, afterTokens string
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE client_id=?", "changed-byte-album-replay").Scan(&afterCount); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT count(*),items,source_tokens FROM message_albums WHERE chat_id=? AND message_id=?", "demo:alex", "changed-byte-album-replay").Scan(&afterAlbumCount, &afterItems, &afterTokens); err != nil {
		t.Fatal(err)
	}
	if beforeCount != 1 || afterCount != beforeCount || beforeAlbumCount != 1 || afterAlbumCount != beforeAlbumCount || afterItems != beforeItems || afterTokens != beforeTokens {
		t.Fatalf("rejected retry mutated durable album state: messages %d->%d albums %d->%d items_changed=%t tokens_changed=%t", beforeCount, afterCount, beforeAlbumCount, afterAlbumCount, afterItems != beforeItems, afterTokens != beforeTokens)
	}
}

func TestAlbumReplayAcceptsUnchangedPreparedBytesAfterRestart(t *testing.T) {
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	first := writeAlbumPNG(t, base, "first.png", color.RGBA{G: 200, A: 255})
	second := writeAlbumPNG(t, base, "second.png", color.RGBA{R: 200, B: 200, A: 255})
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	preparedValue, err := a.Handle(context.Background(), "attachment.album.prepare", json.RawMessage(mustAlbumJSON(t, map[string]any{"paths": []string{first, second}})))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	var prepared AlbumPrepareResult
	decodeResult(t, preparedValue, &prepared)
	paths := make([]string, len(prepared.Items))
	original := make([][]byte, len(prepared.Items))
	for i, item := range prepared.Items {
		paths[i] = item.PreparedPath
		original[i], err = os.ReadFile(item.PreparedPath)
		if err != nil {
			a.Close()
			t.Fatal(err)
		}
	}
	request := json.RawMessage(mustAlbumJSON(t, map[string]any{
		"chat_id": "demo:alex", "prepared_paths": paths, "text": "unchanged restart replay", "client_id": "unchanged-album-replay",
	}))
	if _, err = a.Handle(context.Background(), "message.album.send", request); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(a.prepared) != 0 {
		t.Fatalf("reopened profile unexpectedly restored %d staged snapshots", len(a.prepared))
	}
	for i, path := range paths {
		if err = os.WriteFile(path, original[i], 0600); err != nil {
			t.Fatal(err)
		}
	}
	value, err := a.Handle(context.Background(), "message.album.send", request)
	if err != nil {
		t.Fatalf("unchanged durable source bytes were not idempotent after restart: %v", err)
	}
	var replay struct {
		Message Message `json:"message"`
	}
	decodeResult(t, value, &replay)
	if replay.Message.ID != "unchanged-album-replay" {
		t.Fatalf("unchanged replay returned message %q", replay.Message.ID)
	}
}
