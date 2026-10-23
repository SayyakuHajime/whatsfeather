package app

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedAttachmentCanActuallyBeSent(t *testing.T) {
	root := t.TempDir()
	a, err := Open(filepath.Join(root, "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	source := filepath.Join(root, "source.png")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	prepared := invoke(t, a, "attachment.prepare", map[string]any{"path": source, "mode": "original"})
	result := invoke(t, a, "message.attach", map[string]any{"chat_id": "demo:alex", "path": prepared["prepared_path"], "mode": "original", "text": "prepared fixture", "client_id": "prepared-wire-e2e"})
	message := result["message"].(map[string]any)
	if message["status"] != "demo_sent" {
		t.Fatalf("prepared file did not reach send: %v", message["status"])
	}
}

func TestPreparedImageSnapshotCannotChangeBeforeSend(t *testing.T) {
	root := t.TempDir()
	a, err := Open(filepath.Join(root, "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	source := filepath.Join(root, "source.png")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	prepared := invoke(t, a, "attachment.prepare", map[string]any{"path": source, "mode": "original"})
	preparedPath := prepared["prepared_path"].(string)
	f, err = os.OpenFile(preparedPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("changed after review")); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{"chat_id": "demo:alex", "path": preparedPath, "prepared_path": preparedPath, "mode": "original", "text": "prepared fixture", "client_id": "changed-image-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Handle(context.Background(), "message.attach", params)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_attachment" {
		t.Fatalf("changed prepared image error=%v, want invalid_attachment", err)
	}
	var count int
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE client_id=?", "changed-image-e2e").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("changed prepared image persisted a send row: %d", count)
	}
}

func TestPreparedDocumentStagesAndPersistsAcrossReopen(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	a, err := Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	document := []byte("%PDF-1.4\nsynthetic document fixture\n")
	source := filepath.Join(root, "report.pdf")
	if err = os.WriteFile(source, document, 0600); err != nil {
		t.Fatal(err)
	}
	prepared := invoke(t, a, "attachment.prepare", map[string]any{"path": source, "mode": "document"})
	preparedPath, _ := prepared["prepared_path"].(string)
	if preparedPath == "" || prepared["kind"] != "document" || prepared["mime"] != "application/pdf" || prepared["prepared_size"] != float64(len(document)) {
		t.Fatalf("document preparation metadata mismatch: %#v", prepared)
	}
	result := invoke(t, a, "message.attach", map[string]any{
		"chat_id": "demo:alex", "path": preparedPath, "prepared_path": preparedPath,
		"mode": "document", "text": "synthetic report caption", "client_id": "prepared-document-e2e",
	})
	message, _ := result["message"].(map[string]any)
	if message["id"] != "prepared-document-e2e" || message["kind"] != "document" || message["status"] != "demo_sent" {
		t.Fatalf("prepared document did not reach demo persistence: %#v", message)
	}
	attachment, _ := message["attachment"].(map[string]any)
	if attachment["kind"] != "document" || attachment["mime"] != "application/pdf" || attachment["name"] != "report.pdf" || attachment["sha256"] == "" {
		t.Fatalf("typed document descriptor was not persisted: %#v", attachment)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	listed := invoke(t, a, "message.list", map[string]any{"chat_id": "demo:alex", "limit": 10})
	messages, _ := listed["messages"].([]any)
	for _, value := range messages {
		persisted, _ := value.(map[string]any)
		if persisted["id"] != "prepared-document-e2e" {
			continue
		}
		persistedAttachment, _ := persisted["attachment"].(map[string]any)
		if persisted["kind"] != "document" || persistedAttachment["kind"] != "document" || persistedAttachment["mime"] != "application/pdf" || persistedAttachment["name"] != "report.pdf" || persistedAttachment["sha256"] != attachment["sha256"] {
			t.Fatalf("document type/snapshot changed after reopen: %#v", persisted)
		}
		return
	}
	t.Fatal("prepared document was not present after profile reopen")
}
