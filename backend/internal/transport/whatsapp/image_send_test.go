package whatsapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func TestSendMediaImageUsesImageUploadAndOuterImageEnvelope(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "app", "testdata", "sticker-static.webp")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	meta := app.Attachment{Kind: "image", MIME: "image/webp", Width: 512, Height: 512, Size: int64(len(fixture)), PreparedSize: int64(len(fixture)), SHA256: testMediaSHA256(fixture)}
	client := &fakeStickerMediaClient{
		connected: true,
		loggedIn:  true,
		upload: whatsmeow.UploadResponse{
			URL: "https://mmg.whatsapp.net/synthetic-image", DirectPath: "/m/synthetic-image",
			MediaKey: []byte("synthetic-image-key"), FileEncSHA256: []byte("synthetic-image-encrypted-hash"),
			FileSHA256: []byte("synthetic-image-hash"), FileLength: uint64(len(fixture)),
		},
		response: whatsmeow.SendResponse{Timestamp: time.Unix(456, 0)},
	}
	adapter := &Adapter{dir: t.TempDir()}
	checks := 0
	got, err := adapter.sendMediaWithClient(context.Background(), client, func() bool {
		checks++
		return true
	}, "12345@s.whatsapp.net", fixturePath, meta, "synthetic caption", "image-client-id")
	if err != nil {
		t.Fatal(err)
	}
	if got != client.response.Timestamp.UnixMilli() {
		t.Fatalf("timestamp = %d, want %d", got, client.response.Timestamp.UnixMilli())
	}
	if client.uploadCalls != 1 || client.sendCalls != 1 || client.uploadType != whatsmeow.MediaImage || !bytes.Equal(client.uploaded, fixture) {
		t.Fatalf("image transfer boundary mismatch: uploads=%d sends=%d mediaType=%v bytesMatch=%t", client.uploadCalls, client.sendCalls, client.uploadType, bytes.Equal(client.uploaded, fixture))
	}
	if client.message == nil || client.message.GetImageMessage() == nil || client.message.GetStickerMessage() != nil {
		t.Fatalf("send path must pass an outer ImageMessage, got %#v", client.message)
	}
	image := client.message.GetImageMessage()
	if image.GetMimetype() != meta.MIME || image.GetCaption() != "synthetic caption" || image.GetURL() != client.upload.URL || image.GetDirectPath() != client.upload.DirectPath || image.GetFileLength() != client.upload.FileLength || !bytes.Equal(image.GetMediaKey(), client.upload.MediaKey) || !bytes.Equal(image.GetFileEncSHA256(), client.upload.FileEncSHA256) || !bytes.Equal(image.GetFileSHA256(), client.upload.FileSHA256) {
		t.Fatalf("uploaded metadata/caption was not copied into ImageMessage: %#v", image)
	}
	wantTarget, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	if client.target != wantTarget || len(client.extra) != 1 || string(client.extra[0].ID) != "image-client-id" || client.extra[0].Timeout != 20*time.Second || checks < 2 {
		t.Fatalf("send identity/options/client guard mismatch: target=%v extra=%#v checks=%d", client.target, client.extra, checks)
	}
}
