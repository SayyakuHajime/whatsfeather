package app

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildLinkPreviewProjectionAcceptsOnlyBoundedMatchedHTTPMetadata(t *testing.T) {
	body := "Read https://example.invalid/guide?mode=short"
	preview, ok := BuildLinkPreviewProjection(body, "https://example.invalid/guide?mode=short", "Native guide", "A short synthetic preview.")
	if !ok || preview == nil {
		t.Fatal("valid message-supplied preview metadata was rejected")
	}
	if want := (LinkPreviewProjection{Site: "example.invalid", Title: "Native guide", Description: "A short synthetic preview."}); !reflect.DeepEqual(*preview, want) {
		t.Fatalf("projection = %#v, want %#v", *preview, want)
	}

	cases := []struct {
		name, body, matched, title, description string
	}{
		{"unsafe-scheme", "Open javascript:alert(1)", "javascript:alert(1)", "Bad", ""},
		{"credentials-in-url", "Open https://user@example.invalid/", "https://user@example.invalid/", "Bad", ""},
		{"url-not-in-body", "Read this page", "https://example.invalid/secret", "Bad", ""},
		{"malformed-url", "Open https://[::1", "https://[::1", "Bad", ""},
		{"empty-card-text", "Open https://example.invalid/", "https://example.invalid/", "", ""},
		{"oversized-title", "Open https://example.invalid/", "https://example.invalid/", strings.Repeat("x", 257), ""},
		{"control-in-description", "Open https://example.invalid/", "https://example.invalid/", "Title", "unsafe\x00description"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := BuildLinkPreviewProjection(tc.body, tc.matched, tc.title, tc.description); ok || got != nil {
				t.Fatalf("unsafe preview metadata accepted: %#v", got)
			}
		})
	}
}

func TestRecordPersistsOnlyBoundedLinkPreviewProjectionAcrossProfileReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	preview := &LinkPreviewProjection{Site: "example.invalid", Title: "Native guide", Description: "A short synthetic preview."}
	message := Message{
		ID: "synthetic-link-preview", ChatID: "synthetic-link-chat", Kind: "text",
		Body: "Read https://example.invalid/guide", Timestamp: 1234, Status: "received",
		LinkPreview: preview,
	}
	if err = a.Record(context.Background(), message, "Synthetic peer"); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-link-chat","limit":10}`))
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
	if len(page.Messages) != 1 || page.Messages[0].LinkPreview == nil || !reflect.DeepEqual(*page.Messages[0].LinkPreview, *preview) {
		t.Fatalf("link preview did not survive message.list/profile reopen: %#v", page.Messages)
	}
	for _, forbidden := range []string{"thumbnail", "media_key", "direct_path", "jpeg_thumbnail"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("preview leaked unfetched thumbnail/protocol metadata %q: %s", forbidden, encoded)
		}
	}
	_, err = a.Handle(context.Background(), "message.delete", []byte(`{"chat_id":"synthetic-link-chat","id":"synthetic-link-preview","scope":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	var retained int
	if err = a.db.QueryRow(`SELECT count(*) FROM message_link_previews WHERE chat_id=? AND message_id=?`, message.ChatID, message.ID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 0 {
		t.Fatalf("locally deleted message retained %d link-preview metadata rows", retained)
	}
}

func TestRecordRejectsLinkPreviewOnNonTextMessage(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	err = a.Record(context.Background(), Message{
		ID: "wrong-kind-preview", ChatID: "synthetic-link-chat", Kind: "image",
		Body: "https://example.invalid/image", Timestamp: 1234, Status: "received",
		LinkPreview: &LinkPreviewProjection{Site: "example.invalid", Title: "Image page"},
	}, "Synthetic peer")
	if err == nil {
		t.Fatal("link preview on a non-text message was accepted")
	}
}

func TestBuildLinkPreviewProjectionDoesNotFetchMessageURL(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	previewURL := "http://" + listener.Addr().String() + "/metadata"
	if preview, ok := BuildLinkPreviewProjection("Open "+previewURL, previewURL, "Local metadata", "No request expected."); !ok || preview == nil {
		t.Fatal("safe local URL metadata should be projected without fetching it")
	}
	if err = listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	connection, err := listener.Accept()
	if err == nil {
		connection.Close()
		t.Fatal("building a link preview made an outbound HTTP connection")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("listener accept error = %v, want timeout with no request", err)
	}
}
