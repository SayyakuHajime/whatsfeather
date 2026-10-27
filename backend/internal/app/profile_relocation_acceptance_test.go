package app

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Parent-owned integration acceptance for encrypted profiles whose private
// decrypted mount path changes after close/reopen. Synthetic demo only.
func TestAttachmentURLFollowsProfileRelocation(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old-mount")
	newRoot := filepath.Join(base, "new-mount")
	source := filepath.Join(base, "synthetic.pdf")
	if err := os.WriteFile(source, []byte("%PDF-1.4\nSynthetic profile relocation fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(oldRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{"chat_id": "demo:alex", "path": source, "text": "synthetic relocation", "client_id": "synthetic-relocation-document", "mode": "original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(context.Background(), "message.attach", params); err != nil {
		a.Close()
		t.Fatal(err)
	}
	var storedRaw string
	if err := a.db.QueryRow("SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", "demo:alex", "synthetic-relocation-document").Scan(&storedRaw); err != nil {
		a.Close()
		t.Fatal(err)
	}
	var stored Attachment
	if err := json.Unmarshal([]byte(storedRaw), &stored); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if stored.URL != "file:///media/v1/"+stored.SHA256+".pdf" {
		a.Close()
		t.Fatalf("persisted attachment retained an ephemeral mount path: %q", stored.URL)
	}
	a.Close()
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	a, err = Open(newRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	replayed, err := a.Handle(context.Background(), "message.attach", params)
	if err != nil {
		t.Fatalf("idempotent attachment lookup after relocation failed: %v", err)
	}
	var replayResult struct {
		Message Message `json:"message"`
	}
	replayedJSON, err := json.Marshal(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(replayedJSON, &replayResult); err != nil {
		t.Fatal(err)
	}
	if replayResult.Message.ID != "synthetic-relocation-document" || replayResult.Message.Attachment == nil || !strings.HasPrefix(replayResult.Message.Attachment.URL, fileURL(filepath.Join(newRoot, "media")+string(filepath.Separator))) {
		t.Fatalf("idempotent attachment did not resolve against the new profile root: %#v", replayResult.Message)
	}
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"demo:alex","limit":50}`))
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
	if err := json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Messages {
		if message.ID != "synthetic-relocation-document" {
			continue
		}
		if message.Attachment == nil {
			t.Fatal("stored synthetic attachment missing")
		}
		parsed, err := url.Parse(message.Attachment.URL)
		if err != nil || parsed.Scheme != "file" || !strings.HasPrefix(parsed.Path, filepath.Join(newRoot, "media")+string(filepath.Separator)) {
			t.Fatal("attachment URL still targets the previous profile/mount root after reopen")
		}
		if _, err := os.Stat(parsed.Path); err != nil {
			t.Fatal("relocated attachment URL does not resolve to the encrypted-profile media")
		}
		return
	}
	t.Fatal("synthetic relocation message missing")
}
