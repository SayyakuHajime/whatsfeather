package whatsapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func testMediaSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestSendMediaDocumentRejectsChangedSnapshotBeforeUpload(t *testing.T) {
	original := []byte("%PDF-1.4\nsynthetic report fixture\n")
	path := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(original)
	meta := app.Attachment{
		Name: "report.pdf", Kind: "document", MIME: "application/pdf",
		Size: int64(len(original)), PreparedSize: int64(len(original)),
		SHA256: hex.EncodeToString(digest[:]),
	}
	changed := append([]byte(nil), original...)
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	client := &fakeStickerMediaClient{connected: true, loggedIn: true}
	adapter := &Adapter{dir: t.TempDir()}
	_, err := adapter.sendMediaWithClient(context.Background(), client, func() bool { return true },
		"12345@s.whatsapp.net", path, meta, "synthetic caption", "document-snapshot-id")
	if err == nil {
		t.Fatal("changed document snapshot was uploaded")
	}
	if client.uploadCalls != 0 || client.sendCalls != 0 {
		t.Fatalf("changed document crossed transport boundary: uploads=%d sends=%d", client.uploadCalls, client.sendCalls)
	}
}

func TestSendMediaDocumentUsesDocumentUploadAndOuterDocumentEnvelope(t *testing.T) {
	document := []byte("%PDF-1.4\nsynthetic report fixture\n")
	path := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(path, document, 0600); err != nil {
		t.Fatal(err)
	}
	meta := app.Attachment{
		Name: "report.pdf", Kind: "document", MIME: "application/pdf",
		Size: int64(len(document)), PreparedSize: int64(len(document)), SHA256: testMediaSHA256(document),
	}
	client := &fakeStickerMediaClient{
		connected: true,
		loggedIn:  true,
		upload: whatsmeow.UploadResponse{
			URL: "https://mmg.whatsapp.net/synthetic-document", DirectPath: "/m/synthetic-document",
			MediaKey: []byte("synthetic-document-key"), FileEncSHA256: []byte("synthetic-document-encrypted-hash"),
			FileSHA256: []byte("synthetic-document-hash"), FileLength: uint64(len(document)),
		},
		response: whatsmeow.SendResponse{Timestamp: time.Unix(789, 0)},
	}
	adapter := &Adapter{dir: t.TempDir()}
	got, err := adapter.sendMediaWithClient(context.Background(), client, func() bool { return true },
		"12345@s.whatsapp.net", path, meta, "synthetic caption", "document-client-id")
	if err != nil {
		t.Fatal(err)
	}
	if got != client.response.Timestamp.UnixMilli() || client.uploadCalls != 1 || client.sendCalls != 1 || client.uploadType != whatsmeow.MediaDocument || !bytes.Equal(client.uploaded, document) {
		t.Fatalf("document transfer boundary mismatch: timestamp=%d uploads=%d sends=%d mediaType=%v bytesMatch=%t", got, client.uploadCalls, client.sendCalls, client.uploadType, bytes.Equal(client.uploaded, document))
	}
	if client.message == nil || client.message.GetDocumentMessage() == nil || client.message.GetImageMessage() != nil {
		t.Fatalf("send path must pass an outer DocumentMessage, got %#v", client.message)
	}
	message := client.message.GetDocumentMessage()
	if message.GetMimetype() != meta.MIME || message.GetFileName() != meta.Name || message.GetTitle() != meta.Name || message.GetCaption() != "synthetic caption" || message.GetURL() != client.upload.URL || message.GetDirectPath() != client.upload.DirectPath || message.GetFileLength() != client.upload.FileLength || !bytes.Equal(message.GetMediaKey(), client.upload.MediaKey) || !bytes.Equal(message.GetFileEncSHA256(), client.upload.FileEncSHA256) || !bytes.Equal(message.GetFileSHA256(), client.upload.FileSHA256) {
		t.Fatalf("uploaded metadata/caption was not copied into DocumentMessage: %#v", message)
	}
	wantTarget, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	if client.target != wantTarget || len(client.extra) != 1 || string(client.extra[0].ID) != "document-client-id" || client.extra[0].Timeout != 20*time.Second {
		t.Fatalf("send identity/options mismatch: target=%v extra=%#v", client.target, client.extra)
	}
}
