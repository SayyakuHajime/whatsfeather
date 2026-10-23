package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const notificationRingLimit = 128
const notificationPageLimit = 32

type notificationEvent struct {
	chat, id string
	sequence uint64
	recorded time.Time
}
type notificationProjection struct {
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
}

// The ring contains only bounded identifiers, never copies of private message bodies.
// The caller holds App.mu and only invokes this after a new live inbound commit.
func (a *App) rememberNotification(chat, id string) {
	if len(chat) == 0 || len(chat) > 512 || len(id) == 0 || len(id) > 128 {
		return
	}
	a.notificationSeq++
	a.notificationEvents = append(a.notificationEvents, notificationEvent{chat: chat, id: id, sequence: a.notificationSeq, recorded: time.Now()})
	if len(a.notificationEvents) > notificationRingLimit {
		a.notificationEvents = append([]notificationEvent(nil), a.notificationEvents[len(a.notificationEvents)-notificationRingLimit:]...)
	}
}
func notificationText(value string, limit int) string {
	var b strings.Builder
	count := 0
	for _, r := range value {
		if count >= limit {
			break
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			r = ' '
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// Cursor subscription deliberately starts at the high-water mark. Reconnect,
// daemon restart, expired cursors, or overflow cannot replay old notification bursts.
func (a *App) pollNotifications(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Cursor  string `json:"cursor"`
		Preview bool   `json:"include_preview"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if ctx == nil || ctx.Err() != nil || a.closed || a.closing {
		return nil, fail("app_closing", "Notification stream unavailable")
	}
	if len(p.Cursor) > 128 {
		return nil, fail("invalid_params", "Invalid notification cursor")
	}
	if a.notificationEpoch == "" {
		a.notificationEpoch = rand.Text()
	}
	cursor := func(seq uint64) string { return a.notificationEpoch + ":" + strconv.FormatUint(seq, 10) }
	events := make([]notificationProjection, 0)
	result := func(seq uint64, reset bool) any {
		return map[string]any{"cursor": cursor(seq), "reset": reset, "events": events}
	}
	parts := strings.Split(p.Cursor, ":")
	if len(parts) != 2 || parts[0] != a.notificationEpoch {
		return result(a.notificationSeq, true), nil
	}
	after, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || after > a.notificationSeq {
		return result(a.notificationSeq, true), nil
	}
	if len(a.notificationEvents) > 0 && after < a.notificationEvents[0].sequence-1 {
		return result(a.notificationSeq, true), nil
	}
	last := after
	for _, event := range a.notificationEvents {
		if event.sequence <= after {
			continue
		}
		last = event.sequence
		if a.demo || time.Since(event.recorded) > 30*time.Second {
			continue
		}
		var body, title string
		var muted, outgoing, deleted bool
		err := a.db.QueryRowContext(ctx, `SELECT m.body,c.title,m.outgoing,COALESCE(o.muted,0),COALESCE(s.deleted,0) FROM messages m JOIN chats c ON c.id=m.chat_id LEFT JOIN chat_organization o ON o.chat_id=m.chat_id LEFT JOIN message_state s ON s.chat_id=m.chat_id AND s.message_id=m.id WHERE m.chat_id=? AND m.id=?`, event.chat, event.id).Scan(&body, &title, &outgoing, &muted, &deleted)
		if err != nil || muted || outgoing || deleted {
			continue
		}
		item := notificationProjection{ChatID: event.chat, MessageID: event.id, Title: notificationText(title, 80)}
		if p.Preview {
			item.Body = notificationText(body, 160)
		}
		events = append(events, item)
		if len(events) == notificationPageLimit {
			break
		}
	}
	return result(last, false), nil
}
