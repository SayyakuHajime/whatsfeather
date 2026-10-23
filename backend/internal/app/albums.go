package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxAlbumItems      = 10
	maxAlbumPrepared   = int64(50 << 20)
	maxAlbumProjection = 64 << 10
)

type AlbumPrepareResult struct {
	Items             []Attachment `json:"items"`
	TotalPreparedSize int64        `json:"total_prepared_size"`
	Provenance        string       `json:"provenance"`
}

type albumSource struct {
	meta Attachment
	data []byte
}

func albumSourceTokens(paths []string) []string {
	tokens := make([]string, len(paths))
	for i, path := range paths {
		tokens[i] = sha256Hex([]byte(filepath.Clean(path)))
	}
	return tokens
}

func (a *App) prepareAlbum(ctx context.Context, raw []byte) (any, error) {
	if !a.demo {
		return nil, fail("unsupported", "Album preparation is available only in the local demo")
	}
	var p struct {
		Paths []string `json:"paths"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Paths) < 2 || len(p.Paths) > maxAlbumItems {
		return nil, fail("invalid_params", "Choose between 2 and 10 still images for an album")
	}
	seen := make(map[string]struct{}, len(p.Paths))
	for _, path := range p.Paths {
		if !filepath.IsAbs(path) {
			return nil, fail("invalid_attachment", "Choose local image files")
		}
		clean := filepath.Clean(path)
		if _, duplicate := seen[clean]; duplicate {
			return nil, fail("invalid_params", "An image can appear only once in an album")
		}
		seen[clean] = struct{}{}
	}

	items := make([]Attachment, 0, len(p.Paths))
	preparedPaths := make([]string, 0, len(p.Paths))
	var total int64
	cleanup := func() { a.releasePreparedPaths(preparedPaths) }
	for _, path := range p.Paths {
		if err := ctx.Err(); err != nil {
			cleanup()
			return nil, err
		}
		request, err := json.Marshal(struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
		}{Path: path, Mode: "photo"})
		if err != nil {
			cleanup()
			return nil, err
		}
		result, err := a.prepareWithUniqueStage(ctx, request, true)
		if err != nil {
			cleanup()
			return nil, err
		}
		item, ok := result.(Attachment)
		if !ok {
			cleanup()
			return nil, fail("internal", "Image preparation returned invalid metadata")
		}
		preparedPaths = append(preparedPaths, item.PreparedPath)
		if item.Kind != "image" || item.Mode != "photo" || item.PreparedSize <= 0 || item.PreparedSize > attachmentLimit {
			cleanup()
			return nil, fail("invalid_attachment", "Albums support still images only")
		}
		if total+item.PreparedSize > maxAlbumPrepared {
			cleanup()
			return nil, fail("media_too_large", "An album may contain up to 50 MiB of prepared images")
		}
		total += item.PreparedSize
		items = append(items, item)
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, err
	}
	return AlbumPrepareResult{Items: items, TotalPreparedSize: total, Provenance: "local_demo"}, nil
}

func (a *App) releasePreparedPaths(paths []string) int {
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	released := 0
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		if !isLocalPreparedPath(a.dir, path) {
			continue
		}
		meta, ok := a.prepared[path]
		if !ok || meta.PreparedPath != path || meta.Mode != "photo" || meta.Kind != "image" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			continue
		}
		delete(a.prepared, path)
		released++
	}
	return released
}

func (a *App) releaseAlbum(raw []byte) (any, error) {
	if !a.demo {
		return nil, fail("unsupported", "Album staging is available only in the local demo")
	}
	var p struct {
		Paths []string `json:"paths"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Paths) == 0 || len(p.Paths) > maxAlbumItems {
		return nil, fail("invalid_params", "Album release must name between 1 and 10 staged images")
	}
	return map[string]any{"released": a.releasePreparedPaths(p.Paths)}, nil
}

func (a *App) sendAlbum(ctx context.Context, raw []byte) (any, error) {
	if !a.demo {
		return nil, fail("unsupported", "Album sends are available only in the local demo")
	}
	var p struct {
		ChatID        string   `json:"chat_id"`
		PreparedPaths []string `json:"prepared_paths"`
		Text          string   `json:"text"`
		ClientID      string   `json:"client_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if err := a.chatExists(ctx, p.ChatID); err != nil {
		return nil, err
	}
	if len(p.PreparedPaths) < 2 || len(p.PreparedPaths) > maxAlbumItems || p.ClientID == "" || len(p.ClientID) > 128 || utf8.RuneCountInString(p.Text) > 16000 {
		return nil, fail("invalid_params", "Album needs 2 to 10 images and a valid caption/client ID")
	}
	seen := make(map[string]struct{}, len(p.PreparedPaths))
	for _, path := range p.PreparedPaths {
		if !filepath.IsAbs(path) || !isLocalPreparedPath(a.dir, path) {
			return nil, fail("invalid_attachment", "Album items must be private prepared images")
		}
		clean := filepath.Clean(path)
		if _, duplicate := seen[clean]; duplicate {
			return nil, fail("invalid_params", "An image can appear only once in an album")
		}
		seen[clean] = struct{}{}
	}
	sourceTokens := albumSourceTokens(p.PreparedPaths)
	encodedTokens, err := json.Marshal(sourceTokens)
	if err != nil {
		return nil, err
	}
	if len(encodedTokens) > 2048 {
		return nil, fail("invalid_params", "Album request identity exceeds its limit")
	}

	existing, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", p.ClientID)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		if existing[0].ChatID != p.ChatID || existing[0].Body != p.Text || existing[0].Kind != "album" {
			return nil, fail("conflict", "Client ID already used")
		}
		if err = a.fillAttachments(ctx, p.ChatID, existing); err != nil {
			return nil, err
		}
		if existing[0].Album == nil || len(*existing[0].Album) != len(p.PreparedPaths) {
			return nil, fail("conflict", "Client ID belongs to a different album")
		}
		var storedTokensJSON string
		if err = a.db.QueryRowContext(ctx, "SELECT source_tokens FROM message_albums WHERE chat_id=? AND message_id=?", p.ChatID, existing[0].ID).Scan(&storedTokensJSON); err != nil {
			if err == sql.ErrNoRows {
				return nil, fail("conflict", "Client ID belongs to an incomplete album")
			}
			return nil, err
		}
		var storedTokens []string
		if err = json.Unmarshal([]byte(storedTokensJSON), &storedTokens); err != nil || len(storedTokens) != len(sourceTokens) {
			return nil, fail("conflict", "Client ID belongs to an incomplete album")
		}
		for i := range sourceTokens {
			if sourceTokens[i] != storedTokens[i] {
				return nil, fail("conflict", "Client ID already belongs to a different album request")
			}
		}
		return map[string]any{"message": existing[0]}, nil
	}

	if !a.mediaMu.TryLock() {
		return nil, fail("busy", "Another media preparation or send is in progress")
	}
	mediaLocked := true
	defer func() {
		if mediaLocked {
			a.mediaMu.Unlock()
		}
	}()
	root := filepath.Join(a.dir, "media")
	if err = a.checkMediaCacheLocked(); err != nil {
		return nil, err
	}
	count, used, err := mediaCacheUsage(root)
	if err != nil {
		return nil, err
	}
	if count+len(p.PreparedPaths) > 500 {
		return nil, fail("quota", "Media cache file limit reached")
	}

	sources := make([]albumSource, 0, len(p.PreparedPaths))
	var total int64
	for _, path := range p.PreparedPaths {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		trusted, ok := a.prepared[path]
		if !ok || trusted.PreparedPath != path || trusted.Kind != "image" || trusted.Mode != "photo" || !strings.HasPrefix(trusted.MIME, "image/") || trusted.MIME == "image/gif" || trusted.PreparedSize <= 0 || trusted.PreparedSize > attachmentLimit || trusted.SHA256 == "" {
			return nil, fail("invalid_attachment", "An album item is not a validated private still image")
		}
		file, _, openErr := inspectAttachment(path)
		if openErr != nil {
			return nil, openErr
		}
		stat, statErr := file.Stat()
		if statErr != nil || stat.Size() != trusted.PreparedSize {
			_ = file.Close()
			return nil, fail("invalid_attachment", "Prepared album image no longer matches its reviewed size")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, attachmentLimit+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(data)) != trusted.PreparedSize || sha256Hex(data) != trusted.SHA256 {
			return nil, fail("invalid_attachment", "Prepared album image no longer matches its reviewed contents")
		}
		total += int64(len(data))
		if total > maxAlbumPrepared {
			return nil, fail("media_too_large", "An album may contain up to 50 MiB of prepared images")
		}
		sources = append(sources, albumSource{meta: trusted, data: data})
	}
	if used+total > mediaQuota {
		return nil, fail("quota", "Media cache reached its 256 MiB quota")
	}

	items := make([]Attachment, 0, len(sources))
	for _, source := range sources {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		item, copyErr := copyAlbumItem(root, source)
		if copyErr != nil {
			return nil, copyErr
		}
		items = append(items, item)
	}
	a.mediaMu.Unlock()
	mediaLocked = false

	encodedItems, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	if len(encodedItems) > maxAlbumProjection {
		return nil, fail("message_too_large", "Album metadata exceeds the local message limit")
	}
	message := Message{ID: p.ClientID, ChatID: p.ChatID, Body: p.Text, Timestamp: time.Now().UnixMilli(), Outgoing: true, Status: "demo_sent", Kind: "album", Album: &items}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind,client_id) VALUES(?,?,?,?,?,?,?,?)", message.ID, message.ChatID, message.Body, message.Timestamp, true, message.Status, message.Kind, p.ClientID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO message_albums(chat_id,message_id,items,source_tokens) VALUES(?,?,?,?)", message.ChatID, message.ID, string(encodedItems), string(encodedTokens)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	preparedPaths := append([]string(nil), p.PreparedPaths...)
	a.releasePreparedPaths(preparedPaths)
	a.mu.Lock()
	a.revision++
	a.mu.Unlock()
	rows, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=? AND id=?", message.ChatID, message.ID)
	if err != nil || len(rows) == 0 {
		return nil, fail("unknown_outcome", "Album is saved locally; refresh the conversation before retrying")
	}
	if err = a.fillAttachments(ctx, message.ChatID, rows); err != nil {
		return nil, fail("unknown_outcome", "Album is saved locally; refresh the conversation before retrying")
	}
	return map[string]any{"message": rows[0]}, nil
}

func copyAlbumItem(root string, source albumSource) (Attachment, error) {
	item := source.meta
	if int64(len(source.data)) != item.PreparedSize || sha256Hex(source.data) != item.SHA256 {
		return Attachment{}, fail("invalid_attachment", "Prepared album image no longer matches its reviewed contents")
	}
	temp, err := os.CreateTemp(root, "album-send-")
	if err != nil {
		return Attachment{}, err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err = temp.Write(source.data); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Attachment{}, err
	}
	if err = os.Chmod(tempPath, 0600); err != nil {
		return Attachment{}, err
	}
	destination := mediaDestination(root, item.SHA256, item.MIME, item.Name)
	if err = os.Rename(tempPath, destination); err != nil {
		return Attachment{}, err
	}
	item.URL = (&url.URL{Scheme: "file", Path: destination}).String()
	item.State = "ready"
	item.Error = ""
	item.Size = item.PreparedSize
	item.PreparedPath = ""
	return item, nil
}

func (a *App) fillAlbums(ctx context.Context, chat string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	slots := make([]string, 0, len(messages))
	args := []any{chat}
	for _, message := range messages {
		slots = append(slots, "?")
		args = append(args, message.ID)
	}
	rows, err := a.db.QueryContext(ctx, "SELECT message_id,items FROM message_albums WHERE chat_id=? AND message_id IN ("+strings.Join(slots, ",")+")", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string][]Attachment)
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		if len(raw) > maxAlbumProjection {
			return fmt.Errorf("invalid album metadata")
		}
		var items []Attachment
		if err = json.Unmarshal([]byte(raw), &items); err != nil || len(items) < 2 || len(items) > maxAlbumItems {
			return fmt.Errorf("invalid album metadata")
		}
		for i := range items {
			item := &items[i]
			if item.Kind != "image" || item.Mode != "photo" || item.PreparedPath != "" || item.State != "ready" || !strings.HasPrefix(item.MIME, "image/") || item.MIME == "image/gif" {
				return fmt.Errorf("invalid album metadata")
			}
			if item.URL != "" && !isLocalFileURL(item.URL) {
				item.URL = ""
				item.State = "remote"
			}
			if item.URL == "" {
				item.State = "remote"
			}
		}
		byID[id] = items
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		if items, ok := byID[messages[i].ID]; ok {
			messages[i].Album = &items
		}
	}
	return nil
}

func isAlbumMetaReady(items []Attachment) bool {
	if len(items) < 2 || len(items) > maxAlbumItems {
		return false
	}
	for _, item := range items {
		if item.Kind != "image" || item.Mode != "photo" || item.PreparedPath != "" || item.State != "ready" {
			return false
		}
	}
	return true
}
