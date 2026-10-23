package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func writeAlbumPNG(t *testing.T, dir, name string, fill color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 24, 18))
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			img.SetRGBA(x, y, fill)
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDemoAlbumPrepareSendAndHydrateAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	first := writeAlbumPNG(t, dir, "first.png", color.RGBA{R: 220, A: 255})
	second := writeAlbumPNG(t, dir, "second.png", color.RGBA{B: 220, A: 255})
	profile := filepath.Join(dir, "profile")

	service, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if service != nil {
			_ = service.Close()
		}
	}()

	prepared, err := service.Handle(context.Background(), "attachment.album.prepare", json.RawMessage(mustAlbumJSON(t, map[string]any{
		"paths": []string{first, second},
	})))
	if err != nil {
		t.Fatalf("prepare synthetic album: %v", err)
	}
	var preparation struct {
		Items []Attachment `json:"items"`
	}
	decodeResult(t, prepared, &preparation)
	if len(preparation.Items) != 2 {
		t.Fatalf("prepared %d album items, want 2", len(preparation.Items))
	}
	preparedPaths := make([]string, len(preparation.Items))
	for i, item := range preparation.Items {
		if item.Kind != "image" || item.Mode != "photo" || item.State != "ready" || item.PreparedPath == "" {
			t.Fatalf("album item %d is not a ready private image: %#v", i, item)
		}
		preparedPaths[i] = item.PreparedPath
	}

	request := map[string]any{
		"chat_id": "demo:alex", "prepared_paths": preparedPaths,
		"text": "two synthetic photos", "client_id": "synthetic-album-001",
	}
	sent, err := service.Handle(context.Background(), "message.album.send", json.RawMessage(mustAlbumJSON(t, request)))
	if err != nil {
		t.Fatalf("send synthetic album: %v", err)
	}
	var sentResult struct {
		Message struct {
			ID       string       `json:"id"`
			ChatID   string       `json:"chat_id"`
			Body     string       `json:"body"`
			Kind     string       `json:"kind"`
			Status   string       `json:"status"`
			Album    []Attachment `json:"album"`
			Outgoing bool         `json:"outgoing"`
		} `json:"message"`
	}
	decodeResult(t, sent, &sentResult)
	if sentResult.Message.ID != "synthetic-album-001" || sentResult.Message.ChatID != "demo:alex" || sentResult.Message.Body != "two synthetic photos" || sentResult.Message.Kind != "album" || sentResult.Message.Status != "demo_sent" || !sentResult.Message.Outgoing || len(sentResult.Message.Album) != 2 {
		t.Fatalf("album send projection mismatch: %#v", sentResult.Message)
	}

	if err = service.Close(); err != nil {
		t.Fatal(err)
	}
	service = nil
	service, err = Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"demo:alex","limit":20}`))
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Messages []struct {
			ID    string       `json:"id"`
			Kind  string       `json:"kind"`
			Album []Attachment `json:"album"`
		} `json:"messages"`
	}
	decodeResult(t, page, &listed)
	for _, message := range listed.Messages {
		if message.ID != "synthetic-album-001" {
			continue
		}
		if message.Kind != "album" || len(message.Album) != 2 {
			t.Fatalf("reopened album projection mismatch: %#v", message)
		}
		for i, item := range message.Album {
			if item.Kind != "image" || item.State != "ready" || item.URL == "" || item.SHA256 == "" || item.PreparedPath != "" {
				t.Fatalf("reopened album item %d leaked staging state or lost media: %#v", i, item)
			}
		}
		return
	}
	t.Fatal("sent album was missing from message.list after profile reopen")
}

func mustAlbumJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func decodeResult(t *testing.T, value any, destination any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, destination); err != nil {
		t.Fatal(err)
	}
}

func TestDemoAlbumClientIDRejectsDifferentPreparedImages(t *testing.T) {
	dir := t.TempDir()
	first := writeAlbumPNG(t, dir, "first.png", color.RGBA{R: 210, A: 255})
	second := writeAlbumPNG(t, dir, "second.png", color.RGBA{B: 210, A: 255})
	third := writeAlbumPNG(t, dir, "third.png", color.RGBA{G: 210, A: 255})
	fourth := writeAlbumPNG(t, dir, "fourth.png", color.RGBA{R: 210, B: 210, A: 255})
	service, err := Open(filepath.Join(dir, "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	prepare := func(paths ...string) []Attachment {
		t.Helper()
		result, prepareErr := service.Handle(context.Background(), "attachment.album.prepare", json.RawMessage(mustAlbumJSON(t, map[string]any{"paths": paths})))
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		var prepared AlbumPrepareResult
		decodeResult(t, result, &prepared)
		return prepared.Items
	}
	paths := func(items []Attachment) []string {
		result := make([]string, 0, len(items))
		for _, item := range items {
			result = append(result, item.PreparedPath)
		}
		return result
	}

	initial := prepare(first, second)
	request := map[string]any{
		"chat_id": "demo:alex", "prepared_paths": paths(initial),
		"text": "same caption", "client_id": "album-idempotency-conflict",
	}
	if _, err = service.Handle(context.Background(), "message.album.send", json.RawMessage(mustAlbumJSON(t, request))); err != nil {
		t.Fatalf("first album send: %v", err)
	}
	request["prepared_paths"] = paths(prepare(third, fourth))
	_, err = service.Handle(context.Background(), "message.album.send", json.RawMessage(mustAlbumJSON(t, request)))
	var appError *Error
	if !errors.As(err, &appError) || appError.Code != "conflict" {
		t.Fatalf("same client ID with different image contents must conflict, got %v", err)
	}
}
