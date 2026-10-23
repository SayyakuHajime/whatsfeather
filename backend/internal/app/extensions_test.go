package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchAndContextFindOlderLocalMessage(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "profile"), true)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	result := invoke(t, a, "message.search", map[string]any{"chat_id": "demo:alex", "query": "Morning!"})
	rows := result["messages"].([]any)
	if len(rows) == 0 {
		t.Fatal("no search results")
	}
	id := rows[len(rows)-1].(map[string]any)["id"].(string)
	page := invoke(t, a, "message.context", map[string]any{"chat_id": "demo:alex", "message_id": id})["messages"].([]any)
	found := false
	for _, r := range page {
		m := r.(map[string]any)
		if m["chat_id"] != "demo:alex" {
			t.Fatal("cross-chat context")
		}
		if m["id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatal("target missing from context")
	}
	if len(invoke(t, a, "message.search", map[string]any{"chat_id": "demo:maya", "query": "' OR 1=1 --"})["messages"].([]any)) != 0 {
		t.Fatal("query escaped into SQL")
	}
}
func fixtureGIF(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tiny animation.gif")
	f, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	im := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	if e = gif.Encode(f, im, nil); e != nil {
		t.Fatal(e)
	}
	f.Close()
	return path
}
func TestAttachmentPrepareCapsStillAndPreservesSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	src := filepath.Join(t.TempDir(), "high-detail.jpg")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 2400, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 2400; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x * y), uint8(x), uint8(y), 255})
		}
	}
	if err = jpeg.Encode(f, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	before, _ := os.ReadFile(src)
	beforeHash := sha256.Sum256(before)
	m := invoke(t, a, "attachment.prepare", map[string]any{"path": src, "mode": "photo"})
	if m["compressed"] != true || m["prepared_size"].(float64) >= float64(len(before)) {
		t.Fatalf("photo was not reduced: %#v", m)
	}
	if int(m["width"].(float64)) > 1600 || int(m["height"].(float64)) > 1600 {
		t.Fatalf("dimensions not capped: %#v", m)
	}
	after, _ := os.ReadFile(src)
	if sha256.Sum256(after) != beforeHash {
		t.Fatal("source changed")
	}
	p := m["prepared_path"].(string)
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("unsafe prepared copy: %v", err)
	}
	original := invoke(t, a, "attachment.prepare", map[string]any{"path": src, "mode": "original"})
	if original["compressed"] == true || original["prepared_size"].(float64) != float64(len(before)) {
		t.Fatalf("original mode changed bytes: %#v", original)
	}
}

func TestDemoAttachmentIsCopiedAndPersisted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, e := Open(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	path := fixtureGIF(t)
	meta := invoke(t, a, "attachment.inspect", map[string]any{"path": path})
	if meta["kind"] != "gif" {
		t.Fatal(meta)
	}
	params := map[string]any{"chat_id": "demo:alex", "path": path, "text": "a local gif", "client_id": "attach-once"}
	one := invoke(t, a, "message.attach", params)["message"].(map[string]any)
	two := invoke(t, a, "message.attach", params)["message"].(map[string]any)
	if one["id"] != two["id"] {
		t.Fatal("attachment duplicated")
	}
	media := one["attachment"].(map[string]any)
	if media["kind"] != "gif" || media["name"] != "tiny animation.gif" {
		t.Fatal(media)
	}
	os.Remove(path)
	a.Close()
	a, e = Open(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	rows := invoke(t, a, "message.list", map[string]any{"chat_id": "demo:alex"})["messages"].([]any)
	found := false
	for _, r := range rows {
		m := r.(map[string]any)
		if m["id"] == one["id"] {
			found = true
			if m["attachment"].(map[string]any)["url"] != media["url"] {
				t.Fatal("media lost")
			}
		}
	}
	if !found {
		t.Fatal("message lost")
	}
}

type fixtureMediaTransport struct{}

func (fixtureMediaTransport) Connect(context.Context) error { return nil }
func (fixtureMediaTransport) Disconnect()                   {}
func (fixtureMediaTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (fixtureMediaTransport) DownloadMedia(_ context.Context, payload []byte, path string) error {
	return os.WriteFile(path, payload, 0600)
}

type scopedProfileMediaProbe struct {
	downloads int
}

func (p *scopedProfileMediaProbe) Connect(context.Context) error { return nil }
func (p *scopedProfileMediaProbe) Disconnect()                   {}
func (p *scopedProfileMediaProbe) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (p *scopedProfileMediaProbe) DownloadMedia(context.Context, []byte, string) error {
	return errors.New("unscoped downloader path must not be used")
}
func (p *scopedProfileMediaProbe) AllowManualMedia(chat, id string, timestamp int64) bool {
	return chat == "allowed-chat" && id != "" && timestamp >= 1000
}
func (p *scopedProfileMediaProbe) DownloadScopedMedia(_ context.Context, chat, id string, timestamp int64, payload []byte, _ string, destination string) error {
	p.downloads++
	if chat != "allowed-chat" || id != "current-media" || timestamp < 1000 {
		return errors.New("unexpected profile lookup reached downloader")
	}
	return os.WriteFile(destination, payload, 0600)
}

func TestScopedMediaProfileLookupBlocksOldUnallowedAndMissingMessages(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &scopedProfileMediaProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic scoped media probe", "")
	fixture := fixtureGIF(t)
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	attachment := &Attachment{Name: "fixture.gif", Size: int64(len(data)), MIME: "image/gif", Kind: "image", State: "remote"}
	for _, message := range []Message{
		{ID: "current-media", ChatID: "allowed-chat", Body: "current", Timestamp: 1000, Status: "received", Kind: "image", Attachment: attachment, MediaPayload: string(data)},
		{ID: "outside-media", ChatID: "other-chat", Body: "outside", Timestamp: 2000, Status: "received", Kind: "image", Attachment: attachment, MediaPayload: string(data)},
		{ID: "old-media", ChatID: "allowed-chat", Body: "old", Timestamp: 999, Status: "received", Kind: "image", Attachment: attachment, MediaPayload: string(data)},
	} {
		if err := a.Record(context.Background(), message, "Synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ chat, id string }{
		{"other-chat", "outside-media"},
		{"allowed-chat", "old-media"},
		{"allowed-chat", "missing-media"},
	} {
		_, err := a.Handle(context.Background(), "message.download", []byte(`{"chat_id":"`+item.chat+`","id":"`+item.id+`"}`))
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Code != "not_found" {
			t.Fatalf("out-of-scope profile media %s/%s returned %v, want not_found", item.chat, item.id, err)
		}
		if probe.downloads != 0 {
			t.Fatalf("blocked profile media reached downloader %d times", probe.downloads)
		}
	}
	if _, err = a.Handle(context.Background(), "message.download", []byte(`{"chat_id":"allowed-chat","id":"current-media"}`)); err != nil {
		t.Fatalf("allowed current profile media failed: %v", err)
	}
	if probe.downloads != 1 {
		t.Fatalf("current in-scope profile lookup dispatched %d downloads, want 1", probe.downloads)
	}
}

type gatedDownloadTransport struct {
	entered chan struct{}
	release chan struct{}
}

func (t *gatedDownloadTransport) Connect(context.Context) error { return nil }
func (t *gatedDownloadTransport) Disconnect()                   {}
func (t *gatedDownloadTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (t *gatedDownloadTransport) DownloadMedia(ctx context.Context, payload []byte, path string) error {
	t.entered <- struct{}{}
	select {
	case <-t.release:
		return os.WriteFile(path, payload, 0600)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestConcurrentDownloadReturnsBusyWithoutBlockingStatus(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &gatedDownloadTransport{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	fixture := fixtureGIF(t)
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), Message{ID: "gated", ChatID: "chat", Body: "caption", Timestamp: 1, Status: "received", Kind: "image", Attachment: &Attachment{Name: "x.gif", Size: int64(len(data)), MIME: "image/gif", Kind: "image", State: "remote"}, MediaPayload: string(data)}, "Chat"); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(context.Background(), "message.download", []byte(`{"chat_id":"chat","id":"gated"}`))
		first <- callErr
	}()
	select {
	case <-transport.entered:
	case <-time.After(time.Second):
		t.Fatal("first download did not reach the gated transport")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(context.Background(), "message.download", []byte(`{"chat_id":"chat","id":"gated"}`))
		secondDone <- callErr
	}()
	select {
	case callErr := <-secondDone:
		var appErr *Error
		if !errors.As(callErr, &appErr) || appErr.Code != "busy" {
			t.Fatalf("second download error=%v, want busy", callErr)
		}
	case <-time.After(time.Second):
		t.Fatal("second download waited instead of returning bounded busy")
	}
	statusDone := make(chan struct{})
	go func() {
		_, _ = a.Handle(context.Background(), "app.status", nil)
		close(statusDone)
	}()
	select {
	case <-statusDone:
	case <-time.After(time.Second):
		t.Fatal("app.status blocked behind a concurrent download")
	}
	close(transport.release)
	if callErr := <-first; callErr != nil {
		t.Fatal(callErr)
	}
}

func TestDownloadDoesNotPublishAfterLocalDeleteDuringTransport(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &gatedDownloadTransport{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	const chatID = "synthetic-download-chat"
	const messageID = "delete-during-download"
	payload := []byte("synthetic private document")
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: string(payload),
		Attachment: &Attachment{Name: "fixture.pdf", Size: int64(len(payload)), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	downloadDone := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"delete-during-download"}`))
		downloadDone <- callErr
	}()
	select {
	case <-transport.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not reach the gated fake transport")
	}
	if _, err = a.Handle(ctx, "message.delete", json.RawMessage(`{"chat_id":"synthetic-download-chat","id":"delete-during-download","scope":"local"}`)); err != nil {
		close(transport.release)
		t.Fatalf("local delete while download was pending: %v", err)
	}
	close(transport.release)
	var downloadErr error
	select {
	case downloadErr = <-downloadDone:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not settle after fake transport release")
	}
	var appErr *Error
	if !errors.As(downloadErr, &appErr) || appErr.Code != "not_found" {
		t.Errorf("download after local deletion returned %v, want not_found", downloadErr)
	}
	entries, err := os.ReadDir(filepath.Join(a.dir, "media"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("download published %d private media files after the message was deleted", len(entries))
	}
}

func TestDownloadDoesNotOverwriteRedeliveredDescriptorDuringTransport(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &gatedDownloadTransport{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	const chatID = "synthetic-download-chat"
	const messageID = "redelivered-during-download"
	oldPayload := []byte("old synthetic document")
	newPayload := []byte("new synthetic document with replacement descriptor")
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: string(oldPayload),
		Attachment: &Attachment{Name: "old.pdf", Size: int64(len(oldPayload)), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	downloadDone := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"redelivered-during-download"}`))
		downloadDone <- callErr
	}()
	select {
	case <-transport.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not reach the gated fake transport")
	}
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: string(newPayload),
		Attachment: &Attachment{Name: "new.pdf", Size: int64(len(newPayload)), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		close(transport.release)
		t.Fatalf("record replacement descriptor during download: %v", err)
	}
	close(transport.release)
	var downloadErr error
	select {
	case downloadErr = <-downloadDone:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not settle after fake transport release")
	}
	var appErr *Error
	if !errors.As(downloadErr, &appErr) || appErr.Code != "conflict" {
		t.Errorf("stale download returned %v, want conflict", downloadErr)
	}
	entries, err := os.ReadDir(filepath.Join(a.dir, "media"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("stale download published %d media files", len(entries))
	}
	var metaRaw string
	var payloadValue any
	if err = a.db.QueryRowContext(ctx, "SELECT meta,payload FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&metaRaw, &payloadValue); err != nil {
		t.Fatal(err)
	}
	var currentMeta Attachment
	if err = json.Unmarshal([]byte(metaRaw), &currentMeta); err != nil {
		t.Fatal(err)
	}
	var currentPayload []byte
	switch value := payloadValue.(type) {
	case []byte:
		currentPayload = value
	case string:
		currentPayload = []byte(value)
	}
	if currentMeta.State != "remote" || currentMeta.Name != "new.pdf" || sha256.Sum256(currentPayload) != sha256.Sum256(newPayload) {
		t.Errorf("stale download changed the re-delivered descriptor: meta=%#v payload_hash=%x", currentMeta, sha256.Sum256(currentPayload))
	}
}

func TestCanceledDownloadIsRetryableAndDoesNotPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer a.Close()
	defer cancel()
	transport := &gatedDownloadTransport{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	const chatID = "synthetic-download-chat"
	const messageID = "cancelled-download"
	payload := []byte("synthetic retryable document")
	if err = a.Record(context.Background(), Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: string(payload),
		Attachment: &Attachment{Name: "retry.pdf", Size: int64(len(payload)), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	downloadDone := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"cancelled-download"}`))
		downloadDone <- callErr
	}()
	select {
	case <-transport.entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("download did not reach the gated fake transport")
	}
	cancel()
	select {
	case downloadErr := <-downloadDone:
		if !errors.Is(downloadErr, context.Canceled) {
			t.Fatalf("canceled download returned %v, want context.Canceled", downloadErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not settle after cancellation")
	}
	var metaRaw string
	if err = a.db.QueryRowContext(context.Background(), "SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&metaRaw); err != nil {
		t.Fatal(err)
	}
	var meta Attachment
	if err = json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.State != "error" || meta.URL != "" {
		t.Fatalf("cancellation left an unsafe attachment state: %#v", meta)
	}
	entries, err := os.ReadDir(filepath.Join(a.dir, "media"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled download published %d media files", len(entries))
	}
	a.SetTransport(fixtureMediaTransport{})
	result := invoke(t, a, "message.download", map[string]any{"chat_id": chatID, "id": messageID})["message"].(map[string]any)
	attachment := result["attachment"].(map[string]any)
	if attachment["state"] != "ready" || attachment["url"] == "" {
		t.Fatalf("explicit retry did not recover the canceled download: %#v", attachment)
	}
	fileInfo, err := os.Stat(strings.TrimPrefix(attachment["url"].(string), "file://"))
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0600 {
		t.Fatalf("explicit retry did not create a private regular file: info=%v err=%v", fileInfo, err)
	}
}

func TestDownloadPersistenceFailureRemovesPublishedFileAndMarksRetryable(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(fixtureMediaTransport{})
	const chatID = "synthetic-download-chat"
	const messageID = "ready-persist-failure"
	payload := []byte("synthetic persistence-failure document")
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: string(payload),
		Attachment: &Attachment{Name: "persistence.pdf", Size: int64(len(payload)), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.ExecContext(ctx, `CREATE TRIGGER reject_ready_attachment BEFORE UPDATE OF meta ON attachments
		WHEN instr(NEW.meta, '"state":"ready"') > 0
		BEGIN SELECT RAISE(ABORT, 'injected ready metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, downloadErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"ready-persist-failure"}`))
	var appErr *Error
	if !errors.As(downloadErr, &appErr) || appErr.Code != "media_download_failed" {
		t.Errorf("persistence failure returned %v, want media_download_failed", downloadErr)
	}
	entries, err := os.ReadDir(filepath.Join(a.dir, "media"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed metadata persistence left %d orphaned media files", len(entries))
	}
	var metaRaw string
	if err = a.db.QueryRowContext(ctx, "SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&metaRaw); err != nil {
		t.Fatal(err)
	}
	var meta Attachment
	if err = json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.State != "error" || meta.URL != "" {
		t.Errorf("persistence failure did not leave an explicit retryable state: %#v", meta)
	}
}

type oversizedMediaTransport struct{}

func (oversizedMediaTransport) Connect(context.Context) error { return nil }
func (oversizedMediaTransport) Disconnect()                   {}
func (oversizedMediaTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (oversizedMediaTransport) DownloadMedia(_ context.Context, _ []byte, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err = file.Truncate(int64(attachmentLimit + 1)); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

type symlinkDestinationMediaTransport struct{ target string }

func (symlinkDestinationMediaTransport) Connect(context.Context) error { return nil }
func (symlinkDestinationMediaTransport) Disconnect()                   {}
func (symlinkDestinationMediaTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (t symlinkDestinationMediaTransport) DownloadMedia(_ context.Context, payload []byte, path string) error {
	hash := sha256.Sum256(payload)
	destination := filepath.Join(filepath.Dir(path), hex.EncodeToString(hash[:])+".pdf")
	if err := os.Symlink(t.target, destination); err != nil {
		return err
	}
	return os.WriteFile(path, payload, 0600)
}

func TestDownloadRejectsSymlinkAtContentAddressedDestination(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	profileDir := filepath.Join(root, "profile")
	victimPath := filepath.Join(root, "outside.pdf")
	victimContents := []byte("synthetic unrelated outside file")
	if err := os.WriteFile(victimPath, victimContents, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(symlinkDestinationMediaTransport{target: victimPath})
	const chatID = "synthetic-download-chat"
	const messageID = "symlink-destination"
	payload := []byte("synthetic content-addressed document")
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: string(payload),
		Attachment: &Attachment{Name: "linked.pdf", Size: int64(len(payload)), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	_, downloadErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"symlink-destination"}`))
	var appErr *Error
	if !errors.As(downloadErr, &appErr) || appErr.Code != "media_download_failed" {
		t.Errorf("symlink destination returned %v, want media_download_failed", downloadErr)
	}
	gotVictim, err := os.ReadFile(victimPath)
	if err != nil || !bytes.Equal(gotVictim, victimContents) {
		t.Errorf("download modified the outside target: bytes=%q err=%v", gotVictim, err)
	}
	var metaRaw string
	if err = a.db.QueryRowContext(ctx, "SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&metaRaw); err != nil {
		t.Fatal(err)
	}
	var meta Attachment
	if err = json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.State != "error" || meta.URL != "" {
		t.Errorf("symlink destination was exposed as ready media: %#v", meta)
	}
}

func TestDownloadRepairsCorruptContentAddressedCacheEntry(t *testing.T) {
	ctx := context.Background()
	profileDir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(fixtureMediaTransport{})
	const chatID = "synthetic-download-chat"
	const messageID = "corrupt-cache-entry"
	payload := []byte("synthetic expected bytes for the cache digest")
	hash := sha256.Sum256(payload)
	mediaRoot := filepath.Join(profileDir, "media")
	if err = os.MkdirAll(mediaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(mediaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(mediaRoot, hex.EncodeToString(hash[:])+".pdf")
	if err = os.WriteFile(destination, []byte("wrong content under the expected digest name"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: string(payload),
		Attachment: &Attachment{Name: "cached.pdf", Size: int64(len(payload)), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	result := invoke(t, a, "message.download", map[string]any{"chat_id": chatID, "id": messageID})
	attachment := result["message"].(map[string]any)["attachment"].(map[string]any)
	if attachment["state"] != "ready" {
		t.Fatalf("download did not produce a ready attachment: %#v", attachment)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("cache entry did not match its content digest: bytes=%q err=%v", got, err)
	}
	if attachment["sha256"] != hex.EncodeToString(hash[:]) {
		t.Fatalf("metadata hash does not match repaired file: %#v", attachment)
	}
}

func TestDownloadRejectsNegativeSizeBeforeTransport(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &gatedDownloadTransport{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.SetTransport(transport)
	const chatID = "synthetic-download-chat"
	const messageID = "negative-size"
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: "synthetic payload",
		Attachment: &Attachment{Name: "invalid.pdf", Size: -1, MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	downloadDone := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"negative-size"}`))
		downloadDone <- callErr
	}()
	select {
	case <-transport.entered:
		close(transport.release)
		<-downloadDone
		t.Fatal("negative media size reached the transport")
	case downloadErr := <-downloadDone:
		var appErr *Error
		if !errors.As(downloadErr, &appErr) || appErr.Code != "invalid_attachment" {
			t.Fatalf("negative media size returned %v, want invalid_attachment", downloadErr)
		}
	case <-time.After(5 * time.Second):
		close(transport.release)
		t.Fatal("negative-size download did not terminate")
	}
	var metaRaw string
	if err = a.db.QueryRowContext(ctx, "SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&metaRaw); err != nil {
		t.Fatal(err)
	}
	var meta Attachment
	if err = json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.State != "remote" || meta.URL != "" {
		t.Fatalf("negative-size rejection mutated remote metadata: %#v", meta)
	}
}

func TestDownloadRejectsDeclaredOversizeBeforeTransport(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &gatedDownloadTransport{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.SetTransport(transport)
	const chatID = "synthetic-download-chat"
	const messageID = "declared-oversize"
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: "synthetic payload",
		Attachment: &Attachment{Name: "large.pdf", Size: int64(attachmentLimit + 1), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	_, downloadErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"declared-oversize"}`))
	var appErr *Error
	if !errors.As(downloadErr, &appErr) || appErr.Code != "media_too_large" {
		t.Fatalf("declared oversized media returned %v, want media_too_large", downloadErr)
	}
	select {
	case <-transport.entered:
		t.Fatal("oversized media reached the transport")
	default:
	}
	var metaRaw string
	if err = a.db.QueryRowContext(ctx, "SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&metaRaw); err != nil {
		t.Fatal(err)
	}
	var meta Attachment
	if err = json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.State != "remote" || meta.URL != "" {
		t.Fatalf("preflight rejection mutated remote metadata: %#v", meta)
	}
}

func TestDownloadRespectsCacheQuotaBeforeTransport(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &gatedDownloadTransport{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.SetTransport(transport)
	mediaRoot := filepath.Join(a.dir, "media")
	if err = os.MkdirAll(mediaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(mediaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	quotaFile, err := os.OpenFile(filepath.Join(mediaRoot, "existing-cache.bin"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = quotaFile.Truncate(mediaQuota); err != nil {
		quotaFile.Close()
		t.Fatal(err)
	}
	if err = quotaFile.Close(); err != nil {
		t.Fatal(err)
	}
	const chatID = "synthetic-download-chat"
	const messageID = "quota-limited-download"
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: "x",
		Attachment: &Attachment{Name: "small.pdf", Size: 1, MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	_, downloadErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"quota-limited-download"}`))
	var appErr *Error
	if !errors.As(downloadErr, &appErr) || appErr.Code != "quota" {
		t.Fatalf("full cache returned %v, want quota", downloadErr)
	}
	select {
	case <-transport.entered:
		t.Fatal("cache-quota rejection reached the transport")
	default:
	}
	var metaRaw string
	if err = a.db.QueryRowContext(ctx, "SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&metaRaw); err != nil {
		t.Fatal(err)
	}
	var meta Attachment
	if err = json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.State != "remote" || meta.URL != "" {
		t.Fatalf("cache-quota rejection mutated attachment metadata: %#v", meta)
	}
}

func TestDownloadRejectsOversizedTransportBodyAndCleansTemporaryFile(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(oversizedMediaTransport{})
	const chatID = "synthetic-download-chat"
	const messageID = "transport-oversize"
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: "x",
		Attachment: &Attachment{Name: "oversized.pdf", Size: 1, MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	_, downloadErr := a.Handle(ctx, "message.download", []byte(`{"chat_id":"synthetic-download-chat","id":"transport-oversize"}`))
	var appErr *Error
	if !errors.As(downloadErr, &appErr) || appErr.Code != "media_download_failed" {
		t.Errorf("oversized transport body returned %v, want media_download_failed", downloadErr)
	}
	entries, err := os.ReadDir(filepath.Join(a.dir, "media"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("oversized transport body left %d media files", len(entries))
	}
	var metaRaw string
	if err = a.db.QueryRowContext(ctx, "SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID).Scan(&metaRaw); err != nil {
		t.Fatal(err)
	}
	var meta Attachment
	if err = json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.State != "error" || meta.URL != "" {
		t.Errorf("oversized body did not leave a retryable safe state: %#v", meta)
	}
}

func TestDownloadedMediaPersistsInPrivateProfileAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	profileDir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if a != nil {
			_ = a.Close()
		}
	}()
	a.SetTransport(fixtureMediaTransport{})
	const chatID = "synthetic-download-chat"
	const messageID = "private-persisted-media"
	payload := []byte("synthetic private document that survives reopening")
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "fixture caption", Timestamp: 1,
		Status: "received", Kind: "document", MediaPayload: string(payload),
		Attachment: &Attachment{Name: "private.pdf", Size: int64(len(payload)), MIME: "application/pdf", Kind: "document", State: "remote"},
	}, "Synthetic chat"); err != nil {
		t.Fatal(err)
	}
	result := invoke(t, a, "message.download", map[string]any{"chat_id": chatID, "id": messageID})
	message := result["message"].(map[string]any)
	attachment := message["attachment"].(map[string]any)
	mediaURL, err := url.Parse(attachment["url"].(string))
	if err != nil || mediaURL.Scheme != "file" || mediaURL.Host != "" || mediaURL.RawQuery != "" || mediaURL.Fragment != "" {
		t.Fatalf("download returned an unsafe local URL: %q (%v)", attachment["url"], err)
	}
	mediaRoot := filepath.Join(profileDir, "media")
	rootInfo, err := os.Lstat(mediaRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0700 {
		t.Fatalf("media root is not a private directory: info=%v err=%v", rootInfo, err)
	}
	mediaPath, err := filepath.Abs(mediaURL.Path)
	if err != nil {
		t.Fatal(err)
	}
	rootPath, err := filepath.Abs(mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(rootPath, mediaPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("local media URL escaped the private profile media directory: %q (%v)", mediaURL.Path, err)
	}
	fileInfo, err := os.Lstat(mediaPath)
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0600 {
		t.Fatalf("download is not a private regular file: info=%v err=%v", fileInfo, err)
	}
	contents, err := os.ReadFile(mediaPath)
	if err != nil || !bytes.Equal(contents, payload) {
		t.Fatalf("downloaded contents differ: bytes=%d err=%v", len(contents), err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a = nil
	a, err = Open(profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	rows := invoke(t, a, "message.list", map[string]any{"chat_id": chatID, "limit": 10})["messages"].([]any)
	if len(rows) != 1 {
		t.Fatalf("reopened profile returned %d messages, want 1", len(rows))
	}
	persistedAttachment := rows[0].(map[string]any)["attachment"].(map[string]any)
	if persistedAttachment["state"] != "ready" || persistedAttachment["url"] != attachment["url"] {
		t.Fatalf("ready media metadata did not survive reopen: %#v", persistedAttachment)
	}
	persistedInfo, err := os.Lstat(mediaPath)
	if err != nil || !persistedInfo.Mode().IsRegular() || persistedInfo.Mode().Perm() != 0600 {
		t.Fatalf("private media file did not survive reopen safely: info=%v err=%v", persistedInfo, err)
	}
}

func TestExplicitMediaDownloadHydratesOneMessage(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(fixtureMediaTransport{})
	fixture := fixtureGIF(t)
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	message := Message{ID: "remote-media", ChatID: "remote-chat", Body: "caption", Timestamp: 1, Status: "received", Kind: "image", Attachment: &Attachment{Name: "remote.gif", Size: int64(len(data)), MIME: "image/gif", Kind: "image", State: "remote"}, MediaPayload: string(data)}
	if err := a.Record(context.Background(), message, "Known contact"); err != nil {
		t.Fatal(err)
	}
	result := invoke(t, a, "message.download", map[string]any{"chat_id": "remote-chat", "id": "remote-media"})["message"].(map[string]any)
	attachment := result["attachment"].(map[string]any)
	if attachment["state"] != "ready" || attachment["url"] == "" || result["body"] != "caption" {
		t.Fatalf("download was not hydrated: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "media")); err != nil {
		t.Fatal(err)
	}
	// A genuine server re-delivery can hydrate newer typed fields without
	// replacing an already downloaded private copy.
	if err := a.Record(context.Background(), Message{ID: "remote-media", ChatID: "remote-chat", Body: "new caption", Timestamp: 1, Status: "received", Kind: "image", Attachment: &Attachment{Name: "remote.gif", Size: int64(len(data)), MIME: "image/gif", Kind: "image", State: "remote"}, MediaPayload: string(data)}, "Known contact"); err != nil {
		t.Fatal(err)
	}
	rows := invoke(t, a, "message.list", map[string]any{"chat_id": "remote-chat", "limit": 10})["messages"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["body"] != "new caption" || rows[0].(map[string]any)["attachment"].(map[string]any)["state"] != "ready" {
		t.Fatalf("redelivery did not hydrate while preserving local media: %#v", rows)
	}
}

type historyRequestTransport struct {
	anchor HistoryAnchor
	count  int
	calls  int
}

func (t *historyRequestTransport) Connect(context.Context) error { return nil }
func (t *historyRequestTransport) Disconnect()                   {}
func (t *historyRequestTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (t *historyRequestTransport) RequestHistory(_ context.Context, chat string, anchor HistoryAnchor, count int) error {
	t.calls++
	anchor.ChatID = chat
	t.anchor, t.count = anchor, count
	return nil
}

func TestHistorySyncIsExplicitBoundedAndDoesNotMutateUntilEvent(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &historyRequestTransport{}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "old", ChatID: "chat-a", Body: "old", Timestamp: 10, Status: "received"}, "A"); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), Message{ID: "newer", ChatID: "chat-a", Body: "newer", Timestamp: 20, Status: "received"}, "A"); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), Message{ID: "other", ChatID: "chat-b", Body: "other", Timestamp: 999, Status: "received"}, "B"); err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), "history.sync", []byte(`{"chat_id":"chat-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := result.(map[string]any)
	if got["requested"] != true || got["completed"] != false || got["count"] != 50 || got["anchor_timestamp"] != int64(10) {
		t.Fatalf("untruthful history result: %#v", got)
	}
	if transport.calls != 1 || transport.count != 50 || transport.anchor.ChatID != "chat-a" || transport.anchor.ID != "old" || transport.anchor.Timestamp != 10 {
		t.Fatalf("wrong bounded/scoped anchor: %#v count=%d calls=%d", transport.anchor, transport.count, transport.calls)
	}
	var count int
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE chat_id='chat-a'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("history request mutated rows before an event: %d", count)
	}
}

type gatedMediaSender struct {
	entered chan struct{}
	release chan struct{}
	calls   int
	err     error
}

func (s *gatedMediaSender) SendMedia(ctx context.Context, _ string, _ string, _ Attachment, _ string, _ string) (int64, error) {
	s.calls++
	s.entered <- struct{}{}
	select {
	case <-s.release:
		return 123, s.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestLiveAttachmentIsScopedIdempotentAndReceiptMonotonic(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sender := &gatedMediaSender{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.SetTransport(struct {
		Transport
		MediaSender
	}{Transport: fixtureMediaTransport{}, MediaSender: sender})
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "seed-a", ChatID: "target", Body: "seed", Timestamp: 1, Status: "received"}, "Target"); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), Message{ID: "seed-b", ChatID: "other", Body: "seed", Timestamp: 1, Status: "received"}, "Other"); err != nil {
		t.Fatal(err)
	}
	path := fixtureGIF(t)
	result := make(chan struct {
		value any
		err   error
	}, 1)
	go func() {
		value, callErr := a.Handle(context.Background(), "message.attach", []byte(`{"chat_id":"target","path":"`+path+`","text":"caption","client_id":"media-once"}`))
		result <- struct {
			value any
			err   error
		}{value, callErr}
	}()
	select {
	case <-sender.entered:
	case <-time.After(time.Second):
		t.Fatal("media sender did not start")
	}
	_, err = a.Handle(context.Background(), "message.attach", []byte(`{"chat_id":"other","path":"`+path+`","text":"caption","client_id":"media-once"}`))
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "conflict" {
		t.Fatalf("cross-chat client id was accepted: %v", err)
	}
	a.Receipt(context.Background(), "target", []string{"media-once"}, "read")
	close(sender.release)
	completed := <-result
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	message := completed.value.(map[string]any)["message"].(Message)
	if message.ChatID != "target" || message.Status != "read" {
		t.Fatalf("media completion lost identity/receipt: %#v", message)
	}
	_, err = a.Handle(context.Background(), "message.attach", []byte(`{"chat_id":"target","path":"`+path+`","text":"caption","client_id":"media-once"}`))
	if err != nil || sender.calls != 1 {
		t.Fatalf("idempotent media attach resent: err=%v calls=%d", err, sender.calls)
	}
}

func TestAttachmentValidationAndLiveRefusal(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "p"), true)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	path := fixtureGIF(t)
	link := path + ".link"
	if e = os.Symlink(path, link); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{link, filepath.Dir(path), "/nonexistent-wf-fixture"} {
		raw, _ := json.Marshal(map[string]any{"path": bad})
		if _, e = a.Handle(context.Background(), "attachment.inspect", raw); e == nil {
			t.Fatalf("accepted unsafe path %s", bad)
		}
	}
	live, e := Open(filepath.Join(t.TempDir(), "live"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer live.Close()
	raw, _ := json.Marshal(map[string]any{"chat_id": "x", "path": path, "client_id": "no-live", "text": ""})
	if _, e = live.Handle(context.Background(), "message.attach", raw); e == nil {
		t.Fatal("live attachment not implemented but accepted")
	}
}
