package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const attachmentLimit = 20 << 20
const mediaQuota = 256 << 20

// Keep the complete JSON result below the 256 KiB wire frame. This leaves
// room for the versioned response envelope, the request ID, and the newline.
// The accounting includes JSON escaping, array punctuation, and has_more.
const pageResultBudget = 192 << 10

func pageResultSize(records [][]byte) int {
	size := len(`{"has_more":false,"messages":[`) + len(`]}`)
	for i, record := range records {
		size += len(record)
		if i > 0 {
			size++
		}
	}
	return size
}

func messageTooLarge() error {
	return fail("message_too_large", "This message is too large for the local preview page")
}

type Attachment struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	MIME         string `json:"mime"`
	VideoCodec   string `json:"video_codec,omitempty"`
	AudioCodec   string `json:"audio_codec,omitempty"`
	Kind         string `json:"kind"`
	IsVoiceNote  bool   `json:"is_voice_note"`
	URL          string `json:"url"`
	SHA256       string `json:"sha256,omitempty"`
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
	Duration     int64  `json:"duration,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	OriginalSize int64  `json:"original_size,omitempty"`
	PreparedSize int64  `json:"prepared_size,omitempty"`
	Compressed   bool   `json:"compressed,omitempty"`
	Mode         string `json:"mode,omitempty"`
	PreparedPath string `json:"prepared_path,omitempty"`
	Attribution  string `json:"attribution,omitempty"`
	SourceURL    string `json:"source_url,omitempty"`
}

func inspectAttachment(path string) (*os.File, Attachment, error) {
	var meta Attachment
	if !filepath.IsAbs(path) {
		return nil, meta, fail("invalid_params", "Choose a local file")
	}
	file, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, meta, fail("invalid_attachment", "File is unavailable or is a symbolic link")
	}
	valid := false
	defer func() {
		if !valid {
			file.Close()
		}
	}()
	st, e := file.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > attachmentLimit || st.Size() == 0 {
		return nil, meta, fail("invalid_attachment", "Choose a regular, non-empty file up to 20 MiB")
	}
	head := make([]byte, 512)
	n, e := file.Read(head)
	if e != nil && e != io.EOF {
		return nil, meta, e
	}
	mime := http.DetectContentType(head[:n])
	// net/http does not identify every WebP header on all supported Go
	// versions; recognize only the bounded RIFF/WEBP signature ourselves.
	if n >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		mime = "image/webp"
	}
	kind := "document"
	if mime == "image/gif" {
		kind = "gif"
	} else if mime == "image/png" || mime == "image/jpeg" || mime == "image/webp" {
		kind = "image"
	}
	if _, e = file.Seek(0, 0); e != nil {
		return nil, meta, e
	}
	var videoMeta Attachment
	if n >= 12 && string(head[4:8]) == "ftyp" {
		if e = applyVideoMetadata(context.Background(), file, &videoMeta); e != nil {
			return nil, meta, e
		}
		mime, kind = videoMeta.MIME, videoMeta.Kind
	}
	if kind == "image" || kind == "gif" {
		cfg, _, err := image.DecodeConfig(file)
		if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 16000000 {
			return nil, meta, fail("invalid_attachment", "Image dimensions exceed this preview's safe limit")
		}
		file.Seek(0, 0)
	}
	name := filepath.Base(path)
	if utf8.RuneCountInString(name) > 180 {
		return nil, meta, fail("invalid_attachment", "Filename is too long")
	}
	meta = Attachment{Name: name, Size: st.Size(), MIME: mime, Kind: kind, URL: (&url.URL{Scheme: "file", Path: path}).String(), State: "ready"}
	if videoMeta.Kind == "video" {
		meta.VideoCodec, meta.AudioCodec = videoMeta.VideoCodec, videoMeta.AudioCodec
		meta.Width, meta.Height, meta.Duration = videoMeta.Width, videoMeta.Height, videoMeta.Duration
	}
	valid = true
	return file, meta, nil
}

// mediaCacheUsage is the single quota accounting rule for direct media files and
// app-owned prepared files. It deliberately rejects all other shapes.
func mediaCacheUsage(root string) (int, int64, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	count, varSize := 0, int64(0)
	for _, e := range entries {
		info, err := os.Lstat(filepath.Join(root, e.Name()))
		if err != nil {
			return 0, 0, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return 0, 0, fail("invalid_attachment", "Unexpected media cache entry")
		}
		if info.IsDir() {
			if e.Name() != "prepared" {
				return 0, 0, fail("invalid_attachment", "Unexpected media cache entry")
			}
			children, err := os.ReadDir(filepath.Join(root, e.Name()))
			if err != nil {
				return 0, 0, err
			}
			for _, child := range children {
				ci, err := os.Lstat(filepath.Join(root, e.Name(), child.Name()))
				if err != nil || ci.Mode()&os.ModeSymlink != 0 || !ci.Mode().IsRegular() {
					return 0, 0, fail("invalid_attachment", "Unexpected media cache entry")
				}
				count++
				varSize += ci.Size()
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return 0, 0, fail("invalid_attachment", "Unexpected media cache entry")
		}
		count++
		varSize += info.Size()
	}
	return count, varSize, nil
}
func (a *App) checkMediaCacheLocked() error {
	root := filepath.Join(a.dir, "media")
	if st, err := os.Lstat(root); err == nil && (st.Mode()&os.ModeSymlink != 0 || !st.IsDir()) {
		return fail("invalid_attachment", "Unsafe media directory")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	_, _, err := mediaCacheUsage(root)
	return err
}

func (a *App) inspect(raw json.RawMessage) (any, error) {
	var p struct {
		Path       string `json:"path"`
		Mode       string `json:"mode"`
		AsDocument bool   `json:"as_document"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	file, m, e := inspectAttachment(p.Path)
	if e != nil {
		return nil, e
	}
	if p.Mode == "sticker" {
		if p.AsDocument {
			file.Close()
			return nil, fail("invalid_attachment", "A sticker cannot also be forced to document mode")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxStickerBytes+1))
		file.Close()
		if readErr != nil {
			return nil, fail("invalid_attachment", "Sticker file could not be read")
		}
		cfg, validationErr := validateStaticWebPSticker(data)
		if validationErr != nil {
			return nil, fail("invalid_attachment", validationErr.Error())
		}
		m.Kind, m.Mode, m.MIME = "sticker", "sticker", "image/webp"
		m.Width, m.Height = cfg.Width, cfg.Height
	} else {
		file.Close()
	}
	a.mediaMu.Lock()
	if trusted, ok := a.prepared[p.Path]; ok {
		m = trusted
	}
	a.mediaMu.Unlock()
	if p.AsDocument || p.Mode == "document" {
		m.Kind, m.Mode = "document", "document"
	} else if p.Mode == "video" {
		if m.Kind != "video" {
			return nil, fail("invalid_attachment", "Video mode requires a validated MP4 video file")
		}
		m.Mode = "video"
	} else if p.Mode != "" {
		m.Mode = p.Mode
	}
	return m, nil
}
func (a *App) attach(ctx context.Context, raw json.RawMessage) (any, error) {
	var sender MediaSender
	if !a.demo {
		var ok bool
		sender, ok = a.transport.(MediaSender)
		if !ok || a.state != "connected" {
			return nil, fail("disconnected", "Connect WhatsApp before sending attachments")
		}
	}
	var p struct {
		ChatID       string `json:"chat_id"`
		Path         string `json:"path"`
		Text         string `json:"text"`
		ClientID     string `json:"client_id"`
		Mode         string `json:"mode"`
		AsDocument   bool   `json:"as_document"`
		PreparedPath string `json:"prepared_path"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if e := a.chatExists(ctx, p.ChatID); e != nil {
		return nil, e
	}
	if p.ClientID == "" || len(p.ClientID) > 128 || utf8.RuneCountInString(p.Text) > 16000 {
		return nil, fail("invalid_params", "Invalid caption or client ID")
	}
	existing, e := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", p.ClientID)
	if e != nil {
		return nil, e
	}
	if len(existing) > 0 {
		m := existing[0]
		if m.ChatID != p.ChatID || m.Body != p.Text {
			return nil, fail("conflict", "Client ID already used")
		}
		if e = a.fillAttachments(ctx, p.ChatID, existing); e != nil {
			return nil, e
		}
		if existing[0].Attachment == nil {
			return nil, fail("conflict", "Client ID belongs to a text message")
		}
		return map[string]any{"message": existing[0]}, nil
	}
	path := p.Path
	if p.PreparedPath != "" {
		path = p.PreparedPath
	}
	file, meta, e := inspectAttachment(path)
	if e != nil {
		return nil, e
	}
	defer file.Close()
	a.mediaMu.Lock()
	trustedPrepared := false
	trustedPreparedSize := int64(0)
	trustedPreparedHash := ""
	if trusted, ok := a.prepared[path]; ok {
		meta = trusted
		trustedPrepared = true
		trustedPreparedSize = trusted.PreparedSize
		trustedPreparedHash = trusted.SHA256
		st, statErr := file.Stat()
		if statErr != nil || trustedPreparedSize <= 0 || st.Size() != trustedPreparedSize || trustedPreparedHash == "" {
			a.mediaMu.Unlock()
			return nil, fail("invalid_attachment", "Prepared attachment no longer matches its validated snapshot")
		}
		if trusted.Kind == "audio" && trusted.Size != trusted.PreparedSize {
			a.mediaMu.Unlock()
			return nil, fail("invalid_attachment", "Prepared audio no longer matches its validated snapshot")
		}
		if trusted.Kind != "audio" {
			meta.Size = st.Size()
			meta.PreparedSize = st.Size()
		}
	}
	a.mediaMu.Unlock()
	if p.Mode == "voice-note" && p.AsDocument {
		return nil, fail("invalid_attachment", "A voice note cannot be forced into document mode")
	}
	if p.Mode == "audio" || p.Mode == "voice-note" {
		if !trustedPrepared || p.PreparedPath == "" || meta.Kind != "audio" || meta.MIME != audioMIME || meta.AudioCodec != "opus" || meta.Mode != p.Mode {
			return nil, fail("invalid_attachment", "Audio must use a validated matching private preparation")
		}
	}
	if p.Mode == "sticker" && p.AsDocument {
		return nil, fail("invalid_attachment", "A sticker cannot also be forced to document mode")
	}
	if p.AsDocument || p.Mode == "document" {
		meta.Kind = "document"
		meta.Mode = "document"
	} else if p.Mode == "video" {
		if meta.Kind != "video" {
			return nil, fail("invalid_attachment", "Video mode requires a validated MP4 video file")
		}
		meta.Mode = "video"
	} else if p.Mode == "sticker" || meta.Kind == "sticker" {
		if p.Mode != "" && p.Mode != "sticker" {
			return nil, fail("invalid_attachment", "A prepared sticker must remain in sticker mode")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxStickerBytes+1))
		if readErr != nil {
			return nil, fail("invalid_attachment", "Sticker file could not be read")
		}
		cfg, validationErr := validateStaticWebPSticker(data)
		if validationErr != nil {
			return nil, fail("invalid_attachment", validationErr.Error())
		}
		if _, seekErr := file.Seek(0, 0); seekErr != nil {
			return nil, fail("invalid_attachment", "Sticker file could not be read")
		}
		meta.Kind, meta.Mode, meta.MIME = "sticker", "sticker", "image/webp"
		meta.Width, meta.Height = cfg.Width, cfg.Height
	} else if p.Mode != "" {
		meta.Mode = p.Mode
	}
	if meta.Kind == "audio" {
		if !trustedPrepared || p.PreparedPath == "" || (meta.Mode != "audio" && meta.Mode != "voice-note") ||
			meta.MIME != audioMIME || meta.AudioCodec != "opus" || meta.IsVoiceNote != (meta.Mode == "voice-note") {
			return nil, fail("invalid_attachment", "Audio must use a validated private preparation")
		}
		if p.Text != "" {
			return nil, fail("invalid_attachment", "Audio messages do not support captions")
		}
	}
	if meta.Kind == "sticker" && p.Text != "" {
		return nil, fail("invalid_attachment", "Sticker messages do not support captions")
	}
	if p.PreparedPath != "" && (!isLocalPreparedPath(a.dir, p.PreparedPath)) {
		return nil, fail("invalid_attachment", "Prepared path is not app-owned")
	}
	root := filepath.Join(a.dir, "media")
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	if e = a.checkMediaCacheLocked(); e != nil {
		return nil, e
	}
	count, used, e := mediaCacheUsage(root)
	if e != nil {
		return nil, e
	}
	if count >= 500 {
		return nil, fail("quota", "Media cache file limit reached")
	}
	if used+meta.Size > mediaQuota {
		return nil, fail("quota", "Media cache reached its 256 MiB quota")
	}
	temp, e := os.CreateTemp(root, "copy-")
	if e != nil {
		return nil, e
	}
	defer os.Remove(temp.Name())
	hash := sha256.New()
	n, e := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(file, attachmentLimit+1))
	if e != nil || n > attachmentLimit || n != meta.Size {
		temp.Close()
		return nil, fail("invalid_attachment", "File changed or exceeds the attachment size limit")
	}
	actualSHA256 := hex.EncodeToString(hash.Sum(nil))
	if trustedPrepared && (int64(n) != trustedPreparedSize || actualSHA256 != trustedPreparedHash) {
		temp.Close()
		return nil, fail("invalid_attachment", "Prepared attachment no longer matches its validated snapshot")
	}
	if e = temp.Sync(); e != nil {
		temp.Close()
		return nil, e
	}
	if e = temp.Close(); e != nil {
		return nil, e
	}
	meta.SHA256 = actualSHA256
	destination := mediaDestination(root, meta.SHA256, meta.MIME, meta.Name)
	if e = os.Rename(temp.Name(), destination); e != nil {
		return nil, e
	}
	meta.URL = (&url.URL{Scheme: "file", Path: destination}).String()
	status := "demo_sent"
	if !a.demo {
		status = "queued"
	}
	m := Message{ID: p.ClientID, ChatID: p.ChatID, Body: p.Text, Timestamp: time.Now().UnixMilli(), Outgoing: true, Status: status, Kind: meta.Kind, Attachment: &meta}
	encoded, encodeErr := marshalStoredAttachment(meta)
	if encodeErr != nil {
		return nil, encodeErr
	}
	tx, e := a.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind,client_id) VALUES(?,?,?,?,?,?,?,?)", m.ID, m.ChatID, m.Body, m.Timestamp, true, m.Status, m.Kind, p.ClientID)
	if e != nil {
		return nil, e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO attachments(chat_id,message_id,meta,payload) VALUES(?,?,?,NULL)", m.ChatID, m.ID, string(encoded))
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	a.revision++
	if a.demo {
		return map[string]any{"message": m}, nil
	}

	// The copy and queued row are durable before any network operation. A
	// message-send error is classified as failed only before SendMessage could
	// have accepted it; adapters mark post-upload/send ambiguity explicitly.
	a.beginOperationLocked()
	defer a.endOperationLocked()
	if _, e = a.db.ExecContext(ctx, "UPDATE messages SET status='sending' WHERE id=? AND chat_id=?", m.ID, m.ChatID); e != nil {
		return nil, e
	}
	a.mu.Unlock()
	sent, sendErr := sender.SendMedia(ctx, m.ChatID, destination, meta, p.Text, m.ID)
	a.mu.Lock()
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	persistCtx := ctx
	if persistCtx.Err() != nil {
		persistCtx = context.Background()
	}
	completion := "accepted"
	if sendErr != nil {
		completion = "failed"
		if unknown, ok := sendErr.(unknownOutcome); ok && unknown.UnknownOutcome() {
			completion = "unknown"
		}
	} else if sent > 0 {
		m.Timestamp = sent
	}
	if _, e = a.db.ExecContext(persistCtx, `UPDATE messages SET status=CASE
		WHEN status='read' THEN 'read'
		WHEN status='delivered' THEN 'delivered'
		ELSE ? END,timestamp=? WHERE id=? AND chat_id=?`, completion, m.Timestamp, m.ID, m.ChatID); e != nil {
		return nil, fail("unknown_outcome", "Attachment may be accepted; check history before retrying")
	}
	rows, queryErr := a.queryMessages(persistCtx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE id=? AND chat_id=?", m.ID, m.ChatID)
	if queryErr != nil || len(rows) == 0 {
		return nil, fail("unknown_outcome", "Attachment may be accepted; check history before retrying")
	}
	if e = a.fillAttachments(persistCtx, m.ChatID, rows); e != nil {
		return nil, e
	}
	a.revision++
	return map[string]any{"message": rows[0]}, nil
}
func (a *App) download(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID string `json:"chat_id"`
		ID     string `json:"id"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if e := a.chatExists(ctx, p.ChatID); e != nil {
		return nil, e
	}
	scopedAccess, hasScopedAccess := a.transport.(ScopedMediaAccess)
	var profileMessageTimestamp int64
	if hasScopedAccess {
		if e := a.db.QueryRowContext(ctx, "SELECT timestamp FROM messages WHERE chat_id=? AND id=?", p.ChatID, p.ID).Scan(&profileMessageTimestamp); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return nil, fail("not_found", "Message has no downloadable attachment")
			}
			return nil, e
		}
		if !scopedAccess.AllowManualMedia(p.ChatID, p.ID, profileMessageTimestamp) {
			return nil, fail("not_found", "Message is outside the manual media scope")
		}
	}
	var metaRaw string
	var payloadValue any
	if e := a.db.QueryRowContext(ctx, "SELECT meta,payload FROM attachments WHERE chat_id=? AND message_id=?", p.ChatID, p.ID).Scan(&metaRaw, &payloadValue); e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return nil, fail("not_found", "Message has no downloadable attachment")
		}
		return nil, e
	}
	var payload []byte
	switch value := payloadValue.(type) {
	case []byte:
		payload = value
	case string:
		payload = []byte(value)
	}
	var meta Attachment
	if e := json.Unmarshal([]byte(metaRaw), &meta); e != nil {
		return nil, fail("internal", "Stored media metadata is invalid")
	}
	if meta.State == "ready" && isLocalFileURL(meta.URL) {
		rows, e := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=? AND id=?", p.ChatID, p.ID)
		if e != nil || len(rows) == 0 {
			return nil, fail("not_found", "Message is not stored in this conversation")
		}
		if e = a.fillAttachments(ctx, p.ChatID, rows); e != nil {
			return nil, e
		}
		return map[string]any{"message": rows[0]}, nil
	}
	provider, ok := a.transport.(MediaDownloader)
	if !ok || a.demo {
		return nil, fail("unsupported", "Media download is unavailable in this profile")
	}
	if len(payload) == 0 {
		return nil, fail("media_unavailable", "Original media payload was not retained; future server re-delivery is required")
	}
	if meta.Size < 0 {
		return nil, fail("invalid_attachment", "Media size is invalid")
	}
	if meta.Size > attachmentLimit {
		return nil, fail("media_too_large", "Media exceeds the 20 MiB download limit")
	}
	// Handle serializes callers on App.mu, but network work deliberately does
	// not hold it. Never wait for another download while holding App.mu: a
	// concurrent status/list request must be able to observe the bounded busy
	// result instead of joining a lock cycle.
	if !a.mediaMu.TryLock() {
		return nil, fail("busy", "Another media download is already in progress")
	}
	a.beginOperationLocked()
	defer func() {
		a.endOperationLocked()
		a.mediaMu.Unlock()
	}()
	root := filepath.Join(a.dir, "media")
	if st, e := os.Lstat(root); e == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
		return nil, fail("invalid_attachment", "Unsafe media directory")
	}
	if e := os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	if e := os.Chmod(root, 0700); e != nil {
		return nil, e
	}
	if e := a.checkMediaCacheLocked(); e != nil {
		return nil, e
	}
	count, used, e := mediaCacheUsage(root)
	if e != nil {
		return nil, e
	}
	if count >= 500 {
		return nil, fail("quota", "Media cache file limit reached")
	}
	if used+meta.Size > mediaQuota && meta.Size > 0 {
		return nil, fail("quota", "Media cache reached its 256 MiB quota")
	}
	meta.State, meta.Error, meta.URL = "downloading", "", ""
	encoded, _ := json.Marshal(meta)
	if _, e = a.db.ExecContext(ctx, "UPDATE attachments SET meta=? WHERE chat_id=? AND message_id=?", string(encoded), p.ChatID, p.ID); e != nil {
		return nil, e
	}
	temp, e := os.CreateTemp(root, "download-")
	if e != nil {
		return nil, e
	}
	tempPath := temp.Name()
	temp.Close()
	defer os.Remove(tempPath)
	a.mu.Unlock()
	downloadCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	if hasScopedAccess {
		e = scopedAccess.DownloadScopedMedia(downloadCtx, p.ChatID, p.ID, profileMessageTimestamp, payload, meta.Kind, tempPath)
	} else if typedProvider, ok := provider.(MediaDownloaderWithKind); ok {
		e = typedProvider.DownloadMediaKind(downloadCtx, payload, meta.Kind, tempPath)
	} else {
		e = provider.DownloadMedia(downloadCtx, payload, tempPath)
	}
	cancel()
	a.mu.Lock()
	if e == nil && ctx.Err() != nil {
		e = ctx.Err()
	}
	var info os.FileInfo
	if e == nil {
		info, e = os.Stat(tempPath)
		if e == nil && (!info.Mode().IsRegular() || info.Size() == 0 || info.Size() > attachmentLimit) {
			e = fmt.Errorf("downloaded media exceeds the 20 MiB limit")
		}
		if e == nil && used+info.Size() > mediaQuota {
			e = fmt.Errorf("media cache quota exceeded")
		}
	}
	if a.closing || a.closed {
		// Close waits for this operation and the database is intentionally left
		// untouched once closing owns the lifecycle.
		return nil, fail("app_closing", "App is closing")
	}
	var currentMetaRaw string
	var currentPayloadValue any
	if readErr := a.db.QueryRowContext(context.Background(), "SELECT meta,payload FROM attachments WHERE chat_id=? AND message_id=?", p.ChatID, p.ID).Scan(&currentMetaRaw, &currentPayloadValue); readErr != nil {
		if errors.Is(readErr, sql.ErrNoRows) {
			return nil, fail("not_found", "Message has no downloadable attachment")
		}
		return nil, readErr
	}
	var currentPayload []byte
	switch value := currentPayloadValue.(type) {
	case []byte:
		currentPayload = value
	case string:
		currentPayload = []byte(value)
	}
	var currentMeta Attachment
	if decodeErr := json.Unmarshal([]byte(currentMetaRaw), &currentMeta); decodeErr != nil {
		return nil, fail("internal", "Stored media metadata is invalid")
	}
	if currentMeta != meta || sha256.Sum256(currentPayload) != sha256.Sum256(payload) {
		return nil, fail("conflict", "Attachment changed while the download was in progress")
	}
	markDownloadError := func(err error) error {
		message := err.Error()
		if len(message) > 240 {
			message = message[:240]
		}
		meta.State, meta.Error, meta.URL = "error", message, ""
		encoded, _ = json.Marshal(meta)
		if _, saveErr := a.db.ExecContext(context.Background(), "UPDATE attachments SET meta=? WHERE chat_id=? AND message_id=?", string(encoded), p.ChatID, p.ID); saveErr == nil {
			a.revision++
		}
		return fail("media_download_failed", "Media download failed; retry explicitly")
	}
	if e != nil {
		// Cancellation is a retryable terminal result, not a permanent
		// `downloading` marker. Preserve the caller's cancellation error while
		// recording the safe-to-retry state with a non-canceled DB context.
		if ctx.Err() != nil {
			_ = markDownloadError(ctx.Err())
			return nil, ctx.Err()
		}
		return nil, markDownloadError(e)
	}
	file, e := os.Open(tempPath)
	if e != nil {
		return nil, markDownloadError(e)
	}
	hash := sha256.New()
	_, e = io.Copy(hash, io.LimitReader(file, attachmentLimit+1))
	file.Close()
	if e != nil {
		return nil, markDownloadError(e)
	}
	meta.SHA256 = hex.EncodeToString(hash.Sum(nil))
	if meta.Name == "" {
		meta.Name = "attachment"
	}
	meta.Name = filepath.Base(meta.Name)
	if utf8.RuneCountInString(meta.Name) > 180 || strings.ContainsAny(meta.Name, "\\/\x00") {
		return nil, markDownloadError(fmt.Errorf("media filename is unsafe"))
	}
	if meta.MIME == "" {
		f, openErr := os.Open(tempPath)
		if openErr == nil {
			head := make([]byte, 512)
			n, _ := f.Read(head)
			meta.MIME = http.DetectContentType(head[:n])
			if n >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
				meta.MIME = "image/webp"
			}
			f.Close()
		}
	}
	if meta.MIME == "" || strings.ContainsAny(meta.MIME, "\r\n") {
		return nil, markDownloadError(fmt.Errorf("media MIME type is unsafe"))
	}
	if strings.HasPrefix(meta.MIME, "image/") {
		f, openErr := os.Open(tempPath)
		if openErr != nil {
			return nil, markDownloadError(openErr)
		}
		cfg, _, decodeErr := image.DecodeConfig(f)
		f.Close()
		if decodeErr != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 16000000 {
			return nil, markDownloadError(fmt.Errorf("image dimensions exceed the safe limit"))
		}
		meta.Width, meta.Height = cfg.Width, cfg.Height
	}
	destination := mediaDestination(root, meta.SHA256, meta.MIME, meta.Name)
	destinationCreated := false
	if existing, statErr := os.Lstat(destination); statErr == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return nil, markDownloadError(fmt.Errorf("unsafe media cache destination"))
		}
		reuseExisting := false
		if existing.Size() == info.Size() && existing.Mode().Perm() == 0600 {
			if existingFile, openErr := os.Open(destination); openErr == nil {
				existingHash := sha256.New()
				copied, readErr := io.Copy(existingHash, io.LimitReader(existingFile, attachmentLimit+1))
				closeErr := existingFile.Close()
				reuseExisting = readErr == nil && closeErr == nil && copied == info.Size() && hex.EncodeToString(existingHash.Sum(nil)) == meta.SHA256
			}
		}
		if reuseExisting {
			if e = os.Remove(tempPath); e != nil {
				return nil, markDownloadError(e)
			}
		} else if e = os.Rename(tempPath, destination); e != nil {
			return nil, markDownloadError(e)
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		if e = os.Rename(tempPath, destination); e != nil {
			return nil, markDownloadError(e)
		}
		destinationCreated = true
	} else {
		return nil, markDownloadError(statErr)
	}
	meta.Size, meta.State, meta.Error = info.Size(), "ready", ""
	meta.URL = (&url.URL{Scheme: "file", Path: destination}).String()
	encoded, persistErr := marshalStoredAttachment(meta)
	if persistErr != nil {
		if destinationCreated {
			if cleanupErr := os.Remove(destination); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				persistErr = fmt.Errorf("%w; media cleanup failed: %v", persistErr, cleanupErr)
			}
		}
		return nil, markDownloadError(persistErr)
	}
	if _, persistErr = a.db.ExecContext(context.Background(), "UPDATE attachments SET meta=? WHERE chat_id=? AND message_id=?", string(encoded), p.ChatID, p.ID); persistErr != nil {
		if destinationCreated {
			if cleanupErr := os.Remove(destination); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				persistErr = fmt.Errorf("%w; media cleanup failed: %v", persistErr, cleanupErr)
			}
		}
		return nil, markDownloadError(persistErr)
	}
	a.revision++
	rows, e := a.queryMessages(context.Background(), "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=? AND id=?", p.ChatID, p.ID)
	if e != nil || len(rows) == 0 {
		return nil, fail("not_found", "Message is not stored in this conversation")
	}
	if e = a.fillAttachments(context.Background(), p.ChatID, rows); e != nil {
		return nil, e
	}
	return map[string]any{"message": rows[0]}, nil
}

func mediaDestination(root, hash, mime, name string) string {
	ext := ""
	switch strings.ToLower(mime) {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	case "video/mp4":
		ext = ".mp4"
	case "application/pdf":
		ext = ".pdf"
	}
	if ext == "" {
		candidate := strings.ToLower(filepath.Ext(filepath.Base(name)))
		switch candidate {
		case ".txt", ".csv", ".json", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".zip":
			ext = candidate
		}
	}
	return filepath.Join(root, hash+ext)
}

func mediaCacheFilename(meta Attachment) (string, bool) {
	if len(meta.SHA256) != sha256.Size*2 || strings.ToLower(meta.SHA256) != meta.SHA256 {
		return "", false
	}
	decoded, err := hex.DecodeString(meta.SHA256)
	if err != nil || len(decoded) != sha256.Size {
		return "", false
	}
	name := filepath.Base(mediaDestination("", meta.SHA256, meta.MIME, meta.Name))
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", false
	}
	return name, true
}

func mediaCacheIdentityURL(meta Attachment) (string, bool) {
	name, ok := mediaCacheFilename(meta)
	if !ok {
		return "", false
	}
	return (&url.URL{Scheme: "file", Path: "/media/v1/" + name}).String(), true
}

func mediaIdentityHash(value string, meta Attachment) (string, bool) {
	if !isLocalFileURL(value) {
		return "", false
	}
	parsed, err := url.Parse(value)
	if err != nil || !strings.HasPrefix(parsed.Path, "/media/v1/") {
		return "", false
	}
	name := strings.TrimPrefix(parsed.Path, "/media/v1/")
	if name == "" || filepath.Base(name) != name || len(name) < sha256.Size*2 {
		return "", false
	}
	hash := name[:sha256.Size*2]
	if len(hash) != sha256.Size*2 || strings.ToLower(hash) != hash {
		return "", false
	}
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size || (meta.SHA256 != "" && meta.SHA256 != hash) {
		return "", false
	}
	meta.SHA256 = hash
	expected, ok := mediaCacheFilename(meta)
	return hash, ok && name == expected
}

func marshalStoredAttachment(meta Attachment) ([]byte, error) {
	if meta.State == "ready" && isLocalFileURL(meta.URL) {
		identity, ok := mediaCacheIdentityURL(meta)
		if !ok {
			return nil, fail("invalid_attachment", "Local media metadata has no valid cache identity")
		}
		meta.URL = identity
	}
	return json.Marshal(meta)
}

func isLocalFileURL(value string) bool {
	u, e := url.Parse(value)
	return e == nil && u.Scheme == "file" && u.Opaque == "" && u.User == nil && u.Host == "" &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Path != "" && filepath.IsAbs(u.Path) &&
		filepath.Clean(u.Path) == u.Path && u.String() == value
}

func (a *App) queryMessages(ctx context.Context, query string, args ...any) ([]Message, error) {
	rows, e := a.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	result := []Message{}
	for rows.Next() {
		var m Message
		if e = rows.Scan(&m.ID, &m.ChatID, &m.Body, &m.Timestamp, &m.Outgoing, &m.Status, &m.Kind, &m.SenderName, &m.SenderID, &m.QuotedBody, &m.QuotedSender); e != nil {
			return nil, e
		}
		result = append(result, m)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return nil, e
	}
	rows.Close()
	if len(result) > 0 {
		if e = a.fillMessageState(ctx, result[0].ChatID, result); e != nil {
			return nil, e
		}
	}
	return result, nil
}
func (a *App) fillAttachments(ctx context.Context, chat string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	slots := []string{}
	args := []any{chat}
	for _, m := range messages {
		slots = append(slots, "?")
		args = append(args, m.ID)
	}
	rows, e := a.db.QueryContext(ctx, "SELECT message_id,meta FROM attachments WHERE chat_id=? AND message_id IN ("+strings.Join(slots, ",")+")", args...)
	if e != nil {
		return e
	}
	defer rows.Close()
	byID := map[string]Attachment{}
	for rows.Next() {
		var id, raw string
		if e = rows.Scan(&id, &raw); e != nil {
			return e
		}
		var meta Attachment
		if e = json.Unmarshal([]byte(raw), &meta); e != nil {
			return fmt.Errorf("invalid media metadata")
		}
		if meta.State == "" {
			if meta.URL != "" {
				meta.State = "ready"
			} else {
				meta.State = "remote"
			}
		}
		if meta.SHA256 == "" {
			if hash, ok := mediaIdentityHash(meta.URL, meta); ok {
				meta.SHA256 = hash
			}
		}
		if meta.State == "ready" {
			projected, ok, verifyErr := resolveOwnedMediaURL(ctx, a.dir, meta)
			if verifyErr != nil {
				return verifyErr
			}
			if ok {
				meta.URL = projected
			} else {
				meta.URL = ""
				meta.State = "remote"
			}
		} else {
			meta.URL = ""
		}
		byID[id] = meta
	}
	if e = rows.Err(); e != nil {
		return e
	}
	for i := range messages {
		if meta, ok := byID[messages[i].ID]; ok {
			messages[i].Attachment = &meta
		}
	}
	if e = rows.Close(); e != nil {
		return e
	}
	return a.fillAlbums(ctx, chat, messages)
}
func (a *App) searchMessages(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID string `json:"chat_id"`
		Query  string `json:"query"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if e := a.chatExists(ctx, p.ChatID); e != nil {
		return nil, e
	}
	if utf8.RuneCountInString(p.Query) > 512 {
		return nil, fail("invalid_params", "Search query is too long")
	}
	if strings.TrimSpace(p.Query) == "" {
		return map[string]any{"messages": []Message{}}, nil
	}
	msgs, e := a.queryMessages(ctx, "SELECT id,chat_id,substr(body,1,240),timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=? AND instr(lower(body),lower(?))>0 ORDER BY timestamp DESC,id DESC LIMIT 100", p.ChatID, p.Query)
	return map[string]any{"messages": msgs}, e
}
func (a *App) messageContext(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID string `json:"chat_id"`
		ID     string `json:"message_id"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	var ts int64
	if e := a.db.QueryRowContext(ctx, "SELECT timestamp FROM messages WHERE chat_id=? AND id=?", p.ChatID, p.ID).Scan(&ts); e != nil {
		return nil, fail("not_found", "Message is not stored in this conversation")
	}
	left, e := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=? AND (timestamp<? OR (timestamp=? AND id<=?)) ORDER BY timestamp DESC,id DESC LIMIT 25", p.ChatID, ts, ts, p.ID)
	if e != nil {
		return nil, e
	}
	right, e := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=? AND (timestamp>? OR (timestamp=? AND id>?)) ORDER BY timestamp,id LIMIT 25", p.ChatID, ts, ts, p.ID)
	if e != nil {
		return nil, e
	}

	// Hydrate the complete candidate window before measuring it. The first
	// element of left is the target; all other rows may be removed, but never
	// the target itself.
	candidates := make([]Message, 0, len(left)+len(right))
	candidates = append(candidates, left...)
	candidates = append(candidates, right...)
	if e = a.fillAttachments(ctx, p.ChatID, candidates); e != nil {
		return nil, e
	}
	target := candidates[0]
	older := make([]Message, len(left)-1)
	for i := range older {
		older[i] = candidates[len(left)-1-i]
	}
	newer := candidates[len(left):]
	olderEncoded := make([][]byte, len(older))
	newerEncoded := make([][]byte, len(newer))
	for i, m := range older {
		if olderEncoded[i], e = json.Marshal(m); e != nil {
			return nil, e
		}
	}
	targetEncoded, e := json.Marshal(target)
	if e != nil {
		return nil, e
	}
	for i, m := range newer {
		if newerEncoded[i], e = json.Marshal(m); e != nil {
			return nil, e
		}
	}

	// Start with the full centered window and remove its farthest edge. The
	// incremental accounting avoids repeatedly serializing a growing array.
	used := pageResultSize([][]byte{targetEncoded})
	for _, encoded := range olderEncoded {
		used += len(encoded) + 1
	}
	for _, encoded := range newerEncoded {
		used += len(encoded) + 1
	}
	olderStart, newerEnd := 0, len(newer)
	for used > pageResultBudget {
		if olderStart < len(older) && len(older)-olderStart > newerEnd {
			used -= len(olderEncoded[olderStart]) + 1
			olderStart++
		} else if newerEnd > 0 {
			newerEnd--
			used -= len(newerEncoded[newerEnd]) + 1
		} else if olderStart < len(older) {
			used -= len(olderEncoded[olderStart]) + 1
			olderStart++
		} else {
			return nil, messageTooLarge()
		}
	}
	msgs := make([]Message, 0, len(older)-olderStart+1+newerEnd)
	msgs = append(msgs, older[olderStart:]...)
	msgs = append(msgs, target)
	msgs = append(msgs, newer[:newerEnd]...)
	if len(msgs) == 0 {
		return nil, messageTooLarge()
	}

	// has_more drives the existing Load earlier UI, so it describes only
	// omitted older history. Counts include rows beyond the SQL window as well
	// as rows removed by the byte budget.
	first, last := msgs[0], msgs[len(msgs)-1]
	var olderCount, newerCount int
	e = a.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE chat_id=? AND (timestamp<? OR (timestamp=? AND id<?))", p.ChatID, first.Timestamp, first.Timestamp, first.ID).Scan(&olderCount)
	if e != nil {
		return nil, e
	}
	e = a.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE chat_id=? AND (timestamp>? OR (timestamp=? AND id>?))", p.ChatID, last.Timestamp, last.Timestamp, last.ID).Scan(&newerCount)
	if e != nil {
		return nil, e
	}
	return map[string]any{"messages": msgs, "has_more": olderCount > 0}, nil
}
