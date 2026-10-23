package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
)

const cachedMediaKinds = `('image','video','audio','document','gif','sticker')`

// Protected incoming content has kind unsupported and no retained media. Use
// an allowlist as well so future or unknown kinds cannot enter this library.
const cachedVisibleMessage = `m.kind != 'unsupported' AND NOT EXISTS (SELECT 1 FROM message_state s WHERE s.chat_id=m.chat_id AND s.message_id=m.id AND s.deleted!=0)`
const cachedMediaMessage = `m.kind IN ` + cachedMediaKinds + ` AND EXISTS (SELECT 1 FROM attachments a WHERE a.chat_id=m.chat_id AND a.message_id=m.id) AND NOT EXISTS (
	SELECT 1 FROM attachments private_a WHERE private_a.chat_id=m.chat_id AND private_a.message_id=m.id AND (
		(json_type(private_a.meta,'$.view_once') IS NOT NULL AND json_type(private_a.meta,'$.view_once')!='false') OR
		(json_type(private_a.meta,'$.viewOnce') IS NOT NULL AND json_type(private_a.meta,'$.viewOnce')!='false') OR
		(json_type(private_a.meta,'$.is_view_once') IS NOT NULL AND json_type(private_a.meta,'$.is_view_once')!='false') OR
		(json_type(private_a.meta,'$.ephemeral') IS NOT NULL AND json_type(private_a.meta,'$.ephemeral')!='false') OR
		(json_type(private_a.meta,'$.is_ephemeral') IS NOT NULL AND json_type(private_a.meta,'$.is_ephemeral')!='false') OR
		(json_type(private_a.meta,'$.disappearing') IS NOT NULL AND json_type(private_a.meta,'$.disappearing')!='false') OR
		(json_type(private_a.meta,'$.is_disappearing') IS NOT NULL AND json_type(private_a.meta,'$.is_disappearing')!='false') OR
		(json_type(private_a.meta,'$.protected') IS NOT NULL AND json_type(private_a.meta,'$.protected')!='false') OR
		(json_type(private_a.meta,'$.is_protected') IS NOT NULL AND json_type(private_a.meta,'$.is_protected')!='false')
	))`

// These selected-chat routes require a JSON object and reject null values and
// duplicate fields, in addition to decode's unknown-field and trailing checks.
func decodeHubParams(raw json.RawMessage, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return fail("invalid_params", "Expected selected-chat parameters")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return fail("invalid_params", "Invalid parameters")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return fail("invalid_params", "Invalid parameters")
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fail("invalid_params", "Invalid parameters")
		}
	}
	if _, err = d.Token(); err != nil {
		return fail("invalid_params", "Invalid parameters")
	}
	if _, err = d.Token(); err != io.EOF {
		return fail("invalid_params", "Invalid parameters")
	}
	return decode(raw, target)
}
func validHubChat(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && len(id) <= 512 && !strings.ContainsAny(id, "\x00\r\n")
}

func mediaLibraryResult(chat string, messages []Message, more bool) map[string]any {
	result := map[string]any{"chat_id": chat, "messages": messages, "has_more": more, "provenance": "local_cached"}
	if more && len(messages) > 0 {
		m := messages[len(messages)-1]
		result["next_cursor"] = Cursor{Timestamp: m.Timestamp, ID: m.ID}
	}
	return result
}

type MediaLibraryCursor struct {
	Timestamp int64  `json:"timestamp"`
	ChatID    string `json:"chat_id"`
	ID        string `json:"id"`
}

type MediaLibraryAttachment struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime"`
	Kind     string `json:"kind"`
	Duration int64  `json:"duration,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

type MediaLibraryEntry struct {
	ChatID     string                 `json:"chat_id"`
	ChatTitle  string                 `json:"chat_title"`
	ID         string                 `json:"id"`
	Timestamp  int64                  `json:"timestamp"`
	Kind       string                 `json:"kind"`
	Body       string                 `json:"body"`
	Attachment MediaLibraryAttachment `json:"attachment"`
}

const mediaLibraryDisclosure = "Only retained local-cache media is shown. Link-only messages and media not retained here are omitted; this is not a complete history. Nothing is downloaded or opened automatically."

func mediaLibraryGlobalResult(entries []MediaLibraryEntry, more bool) map[string]any {
	result := map[string]any{"entries": entries, "has_more": more, "provenance": "local_cached", "disclosure": mediaLibraryDisclosure}
	if more && len(entries) > 0 {
		last := entries[len(entries)-1]
		result["next_cursor"] = MediaLibraryCursor{Timestamp: last.Timestamp, ChatID: last.ChatID, ID: last.ID}
	}
	return result
}

func mediaLibraryAttachment(raw string, kind string) (MediaLibraryAttachment, error) {
	var stored Attachment
	if len(raw) > pageResultBudget {
		return MediaLibraryAttachment{}, messageTooLarge()
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return MediaLibraryAttachment{}, fail("invalid_media_metadata", "Cached media metadata is invalid")
	}
	if stored.Kind != "" && stored.Kind != kind {
		return MediaLibraryAttachment{}, fail("invalid_media_metadata", "Cached media metadata does not match its media kind")
	}
	return MediaLibraryAttachment{Name: stored.Name, Size: stored.Size, MIME: stored.MIME, Kind: kind, Duration: stored.Duration, Width: stored.Width, Height: stored.Height}, nil
}

func (a *App) mediaLibrary(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Kind   string              `json:"kind"`
		Limit  *int                `json:"limit"`
		Before *MediaLibraryCursor `json:"before"`
	}
	if err := decodeHubParams(raw, &p); err != nil {
		return nil, err
	}
	switch p.Kind {
	case "", "image", "video", "audio", "document", "gif", "sticker":
	default:
		return nil, fail("invalid_params", "Unsupported media kind")
	}
	limit := 50
	if p.Limit != nil {
		limit = *p.Limit
		if limit < 1 {
			return nil, fail("invalid_params", "Limit must be positive")
		}
		if limit > 50 {
			limit = 50
		}
	}
	where := cachedVisibleMessage + ` AND ` + cachedMediaMessage
	args := []any{}
	if p.Kind != "" {
		where += ` AND m.kind=?`
		args = append(args, p.Kind)
	}
	if p.Before != nil {
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil {
			return nil, fail("invalid_params", "Invalid media cursor")
		}
		var cursorFields map[string]json.RawMessage
		if err := json.Unmarshal(outer["before"], &cursorFields); err != nil || len(cursorFields) != 3 {
			return nil, fail("invalid_params", "Media cursor needs timestamp, chat ID, and ID")
		}
		var checked MediaLibraryCursor
		if err := decodeHubParams(outer["before"], &checked); err != nil {
			return nil, err
		}
		if !validHubChat(p.Before.ChatID) || p.Before.ID == "" || len(p.Before.ID) > 512 || strings.ContainsAny(p.Before.ID, "\x00\r\n") {
			return nil, fail("invalid_params", "Invalid media cursor")
		}
		var exists int
		cursorArgs := append(append([]any{}, args...), p.Before.ID, p.Before.ChatID, p.Before.Timestamp)
		if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM messages m WHERE `+where+` AND m.id=? AND m.chat_id=? AND m.timestamp=?`, cursorArgs...).Scan(&exists); err != nil {
			return nil, err
		}
		if exists != 1 {
			return nil, fail("invalid_params", "Media cursor does not match a retained item and filter")
		}
		where += ` AND (m.timestamp<? OR (m.timestamp=? AND (m.chat_id>? OR (m.chat_id=? AND m.id<?))))`
		args = append(args, p.Before.Timestamp, p.Before.Timestamp, p.Before.ChatID, p.Before.ChatID, p.Before.ID)
	}
	args = append(args, limit+1)
	query := `SELECT m.id,m.chat_id,
		CASE WHEN length(CAST(c.title AS BLOB))<=? THEN c.title ELSE '' END,
		CASE WHEN length(CAST(m.body AS BLOB))<=? THEN m.body ELSE '' END,
		m.timestamp,m.kind,
		CASE WHEN length(CAST(a.meta AS BLOB))<=? THEN a.meta ELSE '' END,
		(length(CAST(c.title AS BLOB))>? OR length(CAST(m.body AS BLOB))>? OR length(CAST(a.meta AS BLOB))>?)
		FROM messages m JOIN chats c ON c.id=m.chat_id
		JOIN attachments a ON a.chat_id=m.chat_id AND a.message_id=m.id
		WHERE ` + where + ` ORDER BY m.timestamp DESC,m.chat_id ASC,m.id DESC LIMIT ?`
	rows, err := a.db.QueryContext(ctx, query, append([]any{pageResultBudget, pageResultBudget, pageResultBudget, pageResultBudget, pageResultBudget, pageResultBudget}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]MediaLibraryEntry, 0, limit+1)
	more := false
	for rows.Next() {
		if len(candidates) == limit {
			more = true
			break
		}
		var entry MediaLibraryEntry
		var rawMeta string
		var oversized bool
		if err = rows.Scan(&entry.ID, &entry.ChatID, &entry.ChatTitle, &entry.Body, &entry.Timestamp, &entry.Kind, &rawMeta, &oversized); err != nil {
			return nil, err
		}
		if oversized {
			if len(candidates) == 0 {
				return nil, messageTooLarge()
			}
			more = true
			break
		}
		if entry.ID == "" || len(entry.ID) > 512 || strings.ContainsAny(entry.ID, "\x00\r\n") || !validHubChat(entry.ChatID) {
			return nil, fail("invalid_media_identity", "Cached media identity is invalid")
		}
		entry.Attachment, err = mediaLibraryAttachment(rawMeta, entry.Kind)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	entries := make([]MediaLibraryEntry, 0, min(limit, len(candidates)))
	for i := 0; i < len(candidates) && i < limit; i++ {
		proposed := append(entries, candidates[i])
		proposedMore := more || i+1 < len(candidates)
		encoded, marshalErr := json.Marshal(mediaLibraryGlobalResult(proposed, proposedMore))
		if marshalErr != nil {
			return nil, marshalErr
		}
		if len(encoded) > pageResultBudget {
			if len(entries) == 0 {
				return nil, messageTooLarge()
			}
			more = true
			break
		}
		entries = proposed
	}
	if len(candidates) > limit {
		more = true
	}
	return mediaLibraryGlobalResult(entries, more), nil
}

func (a *App) mediaList(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID string  `json:"chat_id"`
		Kind   string  `json:"kind"`
		Limit  *int    `json:"limit"`
		Before *Cursor `json:"before"`
	}
	if err := decodeHubParams(raw, &p); err != nil {
		return nil, err
	}
	if !validHubChat(p.ChatID) {
		return nil, fail("invalid_params", "Select an exact chat ID")
	}
	if err := a.chatExists(ctx, p.ChatID); err != nil {
		return nil, err
	}
	switch p.Kind {
	case "", "image", "video", "audio", "document", "gif", "sticker":
	default:
		return nil, fail("invalid_params", "Unsupported media kind")
	}
	limit := 50
	if p.Limit != nil {
		limit = *p.Limit
		if limit < 1 {
			return nil, fail("invalid_params", "Limit must be positive")
		}
		if limit > 50 {
			limit = 50
		}
	}
	where := `m.chat_id=? AND ` + cachedVisibleMessage + ` AND ` + cachedMediaMessage
	args := []any{p.ChatID}
	if p.Kind != "" {
		where += ` AND m.kind=?`
		args = append(args, p.Kind)
	}
	if p.Before != nil {
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil {
			return nil, fail("invalid_params", "Invalid media cursor")
		}
		var cursorFields map[string]json.RawMessage
		if err := json.Unmarshal(outer["before"], &cursorFields); err != nil || len(cursorFields) != 2 {
			return nil, fail("invalid_params", "Media cursor needs timestamp and ID")
		}
		var checked Cursor
		if err := decodeHubParams(outer["before"], &checked); err != nil {
			return nil, err
		}
		var exists int
		if p.Before.ID == "" || len(p.Before.ID) > 512 {
			return nil, fail("invalid_params", "Invalid media cursor")
		}
		// A cursor must refer to an eligible row in this chat and filter. A bare
		// timestamp or another chat's message must never broaden the selection.
		cursorArgs := append(append([]any{}, args...), p.Before.ID, p.Before.Timestamp)
		if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM messages m WHERE `+where+` AND m.id=? AND m.timestamp=?`, cursorArgs...).Scan(&exists); err != nil {
			return nil, err
		}
		if exists != 1 {
			return nil, fail("invalid_params", "Media cursor does not match this chat and filter")
		}
		where += ` AND (m.timestamp<? OR (m.timestamp=? AND m.id<?))`
		args = append(args, p.Before.Timestamp, p.Before.Timestamp, p.Before.ID)
	}
	// timeline(chat_id,timestamp,id) already supplies the selected-chat order.
	args = append(args, limit+1)
	rows, err := a.db.QueryContext(ctx, `SELECT m.id,m.chat_id,m.body,m.timestamp,m.outgoing,m.status,m.kind,m.sender_name,m.sender_id,m.quoted_body,m.quoted_sender FROM messages m WHERE `+where+` ORDER BY m.timestamp DESC,m.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []Message{}
	rawBytes := 0
	more := false
	for rows.Next() {
		var m Message
		if err = rows.Scan(&m.ID, &m.ChatID, &m.Body, &m.Timestamp, &m.Outgoing, &m.Status, &m.Kind, &m.SenderName, &m.SenderID, &m.QuotedBody, &m.QuotedSender); err != nil {
			return nil, err
		}
		if len(candidates) == limit {
			more = true
			break
		}
		b, e := json.Marshal(m)
		if e != nil {
			return nil, e
		}
		if rawBytes+len(b) > pageResultBudget {
			if len(candidates) == 0 {
				return nil, messageTooLarge()
			}
			more = true
			break
		}
		candidates = append(candidates, m)
		rawBytes += len(b)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	messages := []Message{}
	for i := range candidates {
		item := candidates[i : i+1]
		if err = a.fillAttachments(ctx, p.ChatID, item); err != nil {
			return nil, err
		}
		if err = a.fillMessageState(ctx, p.ChatID, item); err != nil {
			return nil, err
		}
		proposed := append(messages, item[0])
		// Account for the final hydrated result, including escaped chat ID and
		// cursor, before retaining each row. No descriptor payload is queried.
		b, e := json.Marshal(mediaLibraryResult(p.ChatID, proposed, true))
		if e != nil {
			return nil, e
		}
		if len(b) > pageResultBudget {
			if len(messages) == 0 {
				return nil, messageTooLarge()
			}
			more = true
			break
		}
		messages = proposed
	}
	result := mediaLibraryResult(p.ChatID, messages, more)
	// SQL is newest first for efficient cursor reads; match message.list's
	// ascending order within each page. The cursor was taken before reversing.
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return result, nil
}
