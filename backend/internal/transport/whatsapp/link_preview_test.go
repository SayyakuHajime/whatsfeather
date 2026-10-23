package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestMessageContentProjectsOnlyEmbeddedBoundedLinkPreviewText(t *testing.T) {
	body := "A local read: https://example.invalid/guide"
	event := &events.Message{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String(body), MatchedText: proto.String("https://example.invalid/guide"),
		Title: proto.String("Synthetic guide"), Description: proto.String("Metadata supplied in the message."),
		JPEGThumbnail: []byte("synthetic-thumbnail-bytes"), ThumbnailDirectPath: proto.String("/private/media/path"),
	}}}
	content := messageContent(event)
	if content.kind != "text" || content.body != body || content.linkPreview == nil {
		t.Fatalf("message content = %#v, want bounded link preview projection", content)
	}
	want := app.LinkPreviewProjection{Site: "example.invalid", Title: "Synthetic guide", Description: "Metadata supplied in the message."}
	if *content.linkPreview != want {
		t.Fatalf("link preview = %#v, want %#v", *content.linkPreview, want)
	}

	cases := []struct {
		name  string
		text  string
		url   string
		title string
	}{
		{"missing-metadata", body, "", ""},
		{"url-not-in-body", body, "https://elsewhere.invalid/", "Wrong source"},
		{"unsafe-scheme", "Open javascript:alert(1)", "javascript:alert(1)", "Unsafe"},
		{"oversized-title", body, "https://example.invalid/guide", strings.Repeat("x", 257)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extended := &waE2E.ExtendedTextMessage{Text: proto.String(tc.text)}
			if tc.url != "" {
				extended.MatchedText = proto.String(tc.url)
				extended.Title = proto.String(tc.title)
			}
			got := messageContent(&events.Message{Message: &waE2E.Message{ExtendedTextMessage: extended}})
			if got.body != tc.text || got.kind != "text" || got.linkPreview != nil {
				t.Fatalf("invalid metadata changed text or created a preview: %#v", got)
			}
		})
	}
}

func TestAdapterPersistsEmbeddedLinkPreviewWithoutThumbnailOrFetch(t *testing.T) {
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	adapter := New(a, t.TempDir())
	chat, err := types.ParseJID("15550123@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	body := "Read https://example.invalid/guide"
	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat},
			ID:            "synthetic-link-preview",
			Timestamp:     time.Unix(1, 0),
		},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(body), MatchedText: proto.String("https://example.invalid/guide"),
			Title: proto.String("Synthetic guide"), Description: proto.String("Embedded only."),
			JPEGThumbnail: []byte("should-not-be-stored"), ThumbnailDirectPath: proto.String("/private/media/path"),
		}},
	}
	adapter.record(context.Background(), event, "Synthetic peer")
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"15550123@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].LinkPreview == nil || page.Messages[0].LinkPreview.Site != "example.invalid" {
		t.Fatalf("adapter did not persist embedded link metadata: %#v", page.Messages)
	}
	for _, forbidden := range []string{"should-not-be-stored", "private/media/path", "synthetic-thumbnail-bytes", "thumbnail", "media_key", "direct_path"} {
		if bytes.Contains(bytes.ToLower(encoded), []byte(strings.ToLower(forbidden))) {
			t.Fatalf("link preview leaked thumbnail/fetch metadata %q: %s", forbidden, encoded)
		}
	}
}
