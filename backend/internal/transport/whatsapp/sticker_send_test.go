package whatsapp

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestBuildStickerMessageRoundTripsAsOuterStickerEnvelope(t *testing.T) {
	response := whatsmeow.UploadResponse{
		URL:           "https://mmg.whatsapp.net/synthetic-sticker",
		DirectPath:    "/m/synthetic-sticker",
		MediaKey:      []byte("synthetic-media-key"),
		FileEncSHA256: []byte("synthetic-encrypted-hash"),
		FileSHA256:    []byte("synthetic-file-hash"),
		FileLength:    458,
	}
	meta := app.Attachment{Kind: "sticker", MIME: "image/webp", Width: 512, Height: 512, Size: 458, PreparedSize: 458}
	message, err := buildStickerMessage(response, meta, "")
	if err != nil {
		t.Fatal(err)
	}
	if message.StickerMessage == nil || message.ImageMessage != nil {
		t.Fatalf("message must use the outer StickerMessage envelope: %#v", message)
	}
	encoded, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var restored waE2E.Message
	if err = proto.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	sticker := restored.GetStickerMessage()
	if sticker == nil || sticker.GetMimetype() != "image/webp" || sticker.GetWidth() != 512 || sticker.GetHeight() != 512 || sticker.IsAnimated == nil || sticker.GetIsAnimated() {
		t.Fatalf("sticker metadata did not survive its outer-envelope round trip: %#v", sticker)
	}
	if sticker.GetURL() != response.URL || sticker.GetDirectPath() != response.DirectPath || sticker.GetFileLength() != response.FileLength || !bytes.Equal(sticker.GetMediaKey(), response.MediaKey) || !bytes.Equal(sticker.GetFileEncSHA256(), response.FileEncSHA256) || !bytes.Equal(sticker.GetFileSHA256(), response.FileSHA256) {
		t.Fatalf("uploaded media metadata was not copied into StickerMessage: %#v", sticker)
	}
}

func TestBuildStickerMessageRejectsCaptionAndInvalidMetadata(t *testing.T) {
	response := whatsmeow.UploadResponse{URL: "https://mmg.whatsapp.net/synthetic", DirectPath: "/m/synthetic", FileLength: 458}
	valid := app.Attachment{Kind: "sticker", MIME: "image/webp", Width: 512, Height: 512, Size: 458, PreparedSize: 458}
	if _, err := buildStickerMessage(response, valid, "caption"); err == nil {
		t.Fatal("sticker captions must be rejected rather than dropped")
	}
	mismatched := valid
	mismatched.Size++
	if _, err := buildStickerMessage(response, mismatched, ""); err == nil {
		t.Fatal("uploaded byte count must match the staged sticker")
	}
	cases := []struct {
		name string
		meta app.Attachment
	}{
		{name: "wrong kind", meta: app.Attachment{Kind: "image", MIME: "image/webp", Width: 512, Height: 512, Size: 458}},
		{name: "wrong MIME", meta: app.Attachment{Kind: "sticker", MIME: "image/png", Width: 512, Height: 512, Size: 458}},
		{name: "wrong dimensions", meta: app.Attachment{Kind: "sticker", MIME: "image/webp", Width: 256, Height: 512, Size: 458}},
		{name: "too large", meta: app.Attachment{Kind: "sticker", MIME: "image/webp", Width: 512, Height: 512, Size: app.StickerMaxBytes + 1}},
		{name: "empty", meta: app.Attachment{Kind: "sticker", MIME: "image/webp", Width: 512, Height: 512}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildStickerMessage(response, tc.meta, ""); err == nil {
				t.Fatal("invalid sticker metadata must be rejected")
			}
		})
	}
}

type fakeStickerMediaClient struct {
	connected   bool
	loggedIn    bool
	uploadType  whatsmeow.MediaType
	uploaded    []byte
	uploadCalls int
	sendCalls   int
	upload      whatsmeow.UploadResponse
	message     *waE2E.Message
	target      types.JID
	extra       []whatsmeow.SendRequestExtra
	response    whatsmeow.SendResponse
}

func (f *fakeStickerMediaClient) IsConnected() bool { return f.connected }
func (f *fakeStickerMediaClient) IsLoggedIn() bool  { return f.loggedIn }

func (f *fakeStickerMediaClient) UploadReader(ctx context.Context, plaintext io.Reader, tempFile io.ReadWriteSeeker, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploadCalls++
	if tempFile != nil {
		return whatsmeow.UploadResponse{}, os.ErrInvalid
	}
	f.uploadType = appInfo
	data, err := io.ReadAll(plaintext)
	f.uploaded = data
	return f.upload, err
}

func (f *fakeStickerMediaClient) SendMessage(ctx context.Context, target types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sendCalls++
	f.target = target
	f.message = proto.Clone(message).(*waE2E.Message)
	f.extra = append([]whatsmeow.SendRequestExtra(nil), extra...)
	return f.response, nil
}

func TestSendMediaStickerUsesImageUploadAndStickerEnvelope(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "app", "testdata", "sticker-static.webp")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	meta := app.Attachment{Kind: "sticker", MIME: "image/webp", Width: 512, Height: 512, Size: int64(len(fixture)), PreparedSize: int64(len(fixture)), SHA256: testMediaSHA256(fixture)}
	client := &fakeStickerMediaClient{
		connected: true,
		loggedIn:  true,
		upload: whatsmeow.UploadResponse{
			URL: "https://mmg.whatsapp.net/synthetic-sticker", DirectPath: "/m/synthetic-sticker",
			MediaKey: []byte("synthetic-media-key"), FileEncSHA256: []byte("synthetic-encrypted-hash"),
			FileSHA256: []byte("synthetic-file-hash"), FileLength: uint64(len(fixture)),
		},
		response: whatsmeow.SendResponse{Timestamp: time.Unix(123, 0)},
	}
	adapter := &Adapter{dir: t.TempDir()}
	currentChecks := 0
	got, err := adapter.sendMediaWithClient(context.Background(), client, func() bool {
		currentChecks++
		return true
	}, "12345@s.whatsapp.net", fixturePath, meta, "", "sticker-client-id")
	if err != nil {
		t.Fatal(err)
	}
	if got != client.response.Timestamp.UnixMilli() {
		t.Fatalf("timestamp = %d, want %d", got, client.response.Timestamp.UnixMilli())
	}
	if client.uploadCalls != 1 || client.sendCalls != 1 || client.uploadType != whatsmeow.MediaImage || !bytes.Equal(client.uploaded, fixture) {
		t.Fatalf("sticker transfer boundary mismatch: uploads=%d sends=%d mediaType=%v bytesMatch=%t", client.uploadCalls, client.sendCalls, client.uploadType, bytes.Equal(client.uploaded, fixture))
	}
	if client.message == nil || client.message.GetStickerMessage() == nil || client.message.GetImageMessage() != nil {
		t.Fatalf("send path must pass an outer StickerMessage, got %#v", client.message)
	}
	sticker := client.message.GetStickerMessage()
	if sticker.GetMimetype() != "image/webp" || sticker.GetWidth() != 512 || sticker.GetHeight() != 512 || sticker.GetIsAnimated() || sticker.GetFileLength() != uint64(len(fixture)) {
		t.Fatalf("send path lost static sticker metadata: %#v", sticker)
	}
	wantTarget, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	if client.target != wantTarget || len(client.extra) != 1 || string(client.extra[0].ID) != "sticker-client-id" || client.extra[0].Timeout != 20*time.Second {
		t.Fatalf("send identity/options mismatch: target=%v extra=%#v", client.target, client.extra)
	}
	if currentChecks < 2 {
		t.Fatalf("current-client guard ran %d times, want checks before and after send", currentChecks)
	}
}

func TestSendMediaStickerRejectsCaptionBeforeUpload(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "app", "testdata", "sticker-static.webp")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	meta := app.Attachment{Kind: "sticker", MIME: "image/webp", Width: 512, Height: 512, Size: int64(len(fixture)), PreparedSize: int64(len(fixture)), SHA256: testMediaSHA256(fixture)}
	client := &fakeStickerMediaClient{connected: true, loggedIn: true}
	adapter := &Adapter{dir: t.TempDir()}
	_, err = adapter.sendMediaWithClient(context.Background(), client, func() bool { return true }, "12345@s.whatsapp.net", fixturePath, meta, "caption", "sticker-caption-id")
	if err == nil {
		t.Fatal("sticker caption must be rejected")
	}
	if client.uploadCalls != 0 || client.sendCalls != 0 {
		t.Fatalf("caption rejection occurred after transfer side effects: uploads=%d sends=%d", client.uploadCalls, client.sendCalls)
	}
}
