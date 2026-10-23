package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type listReplyParams struct {
	ChatID       string `json:"chat_id"`
	TargetID     string `json:"target_id"`
	SectionIndex int    `json:"section_index"`
	RowIndex     int    `json:"row_index"`
	ClientID     string `json:"client_id"`
}

func (a *App) messageListReply(ctx context.Context, raw json.RawMessage) (any, error) {
	var p listReplyParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if !actionID(p.ClientID) || p.SectionIndex < 0 || p.RowIndex < 0 {
		return nil, fail("invalid_params", "List selection or client ID is invalid")
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.TargetID)
	if err != nil {
		return nil, err
	}
	if target.Deleted || target.Outgoing || target.Kind != "list" {
		return nil, fail("invalid_target", "Cannot select a row from this message")
	}
	hydrated := []Message{target}
	if err = a.fillListProjections(ctx, p.ChatID, hydrated); err != nil {
		return nil, err
	}
	target = hydrated[0]
	if target.ListReplyState == "accepted" {
		return nil, fail("already_replied", "A response to this single-select list was already accepted")
	}
	if target.ListReplyState == "pending" || target.ListReplyState == "unknown" {
		return nil, fail("unknown_outcome", "A prior list response is unresolved; reconcile history before retrying")
	}
	if target.List == nil || !target.ListReplyAvailable {
		return nil, fail("unsupported", "This list has no complete actionable row mapping")
	}
	if p.SectionIndex >= len(target.List.Sections) || p.RowIndex >= len(target.List.Sections[p.SectionIndex].Rows) {
		return nil, fail("invalid_params", "Selected list row is out of range")
	}
	row := target.List.Sections[p.SectionIndex].Rows[p.RowIndex]
	var rowID string
	err = a.db.QueryRowContext(ctx, `SELECT row_id FROM message_list_actions
		WHERE chat_id=? AND message_id=? AND section_index=? AND row_index=?`,
		p.ChatID, p.TargetID, p.SectionIndex, p.RowIndex).Scan(&rowID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fail("unsupported", "Selected list row has no protocol action mapping")
	}
	if err != nil {
		return nil, err
	}

	existing, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", p.ClientID)
	if err != nil {
		return nil, err
	}
	var existingMessage *Message
	if len(existing) > 0 {
		candidate := existing[0]
		if candidate.ChatID != p.ChatID || candidate.Body != row.Title {
			return nil, fail("conflict", "Client ID already used for another message")
		}
		if err = a.fillMessageState(ctx, candidate.ChatID, existing); err != nil {
			return nil, err
		}
		candidate = existing[0]
		if candidate.ReplyToID != p.TargetID {
			return nil, fail("conflict", "Client ID already used for another message")
		}
		existingMessage = &candidate
		if a.demo {
			return map[string]any{"message": candidate, "target": target, "status": candidate.Status}, nil
		}
		var actionExists int
		err = a.db.QueryRowContext(ctx, "SELECT 1 FROM message_actions WHERE client_id=?", p.ClientID).Scan(&actionExists)
		if errors.Is(err, sql.ErrNoRows) {
			// A stored echo without a durable action record is historical state; do
			// not guess whether it reached WhatsApp and never replay it.
			return map[string]any{"message": candidate, "target": target, "status": candidate.Status}, nil
		}
		if err != nil {
			return nil, err
		}
	}

	var sender MessageActionTransport
	var listSender ListReplySender
	done := false
	if !a.demo {
		sender, err = a.liveActionTransport()
		if err != nil {
			return nil, err
		}
		listSender, _ = sender.(ListReplySender)
		if listSender == nil {
			return nil, fail("unsupported", "The connected transport cannot send list responses")
		}
		if existingMessage == nil {
			if err = a.blockExistingListReply(ctx, p.ChatID, p.TargetID, p.ClientID); err != nil {
				return nil, err
			}
		}
		payload, marshalErr := json.Marshal(struct {
			SectionIndex int    `json:"section_index"`
			RowIndex     int    `json:"row_index"`
			RowID        string `json:"row_id"`
		}{p.SectionIndex, p.RowIndex, rowID})
		if marshalErr != nil {
			return nil, marshalErr
		}
		done, err = a.reserveAction(ctx, p.ClientID, "list.reply", p.ChatID, p.TargetID, string(payload))
		if err != nil {
			return nil, err
		}
	}

	var response Message
	if existingMessage != nil {
		response = *existingMessage
		if done {
			if _, err = a.db.ExecContext(ctx, "UPDATE messages SET status='accepted' WHERE id=? AND chat_id=? AND status NOT IN ('read','delivered')", response.ID, response.ChatID); err != nil {
				return nil, err
			}
			response.Status = "accepted"
			target.ListReplyAvailable = false
			target.ListReplyState = "accepted"
			return map[string]any{"message": response, "target": target, "status": response.Status}, nil
		}
		if !a.demo {
			if _, err = a.db.ExecContext(ctx, "UPDATE messages SET status='queued' WHERE id=? AND chat_id=? AND status NOT IN ('read','delivered')", response.ID, response.ChatID); err != nil {
				return nil, err
			}
			response.Status = "queued"
		}
	} else {
		status := "demo_sent"
		if !a.demo {
			status = "queued"
		}
		response = Message{
			ID: p.ClientID, ChatID: p.ChatID, Body: row.Title, Timestamp: time.Now().UnixMilli(),
			Outgoing: true, Status: status, Kind: "text", QuotedBody: target.Body,
			QuotedSender: target.SenderName, ReplyToID: p.TargetID,
		}
		if _, err = a.db.ExecContext(ctx, `INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,client_id,kind,quoted_body,quoted_sender)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, response.ID, response.ChatID, response.Body, response.Timestamp,
			response.Outgoing, response.Status, p.ClientID, response.Kind, response.QuotedBody, response.QuotedSender); err != nil {
			return nil, err
		}
	}
	if _, err = a.db.ExecContext(ctx, `INSERT INTO message_state(chat_id,message_id,reply_to_id,provenance) VALUES(?,?,?,'local_only')
		ON CONFLICT(chat_id,message_id) DO UPDATE SET reply_to_id=excluded.reply_to_id`, response.ChatID, response.ID, p.TargetID); err != nil {
		return nil, err
	}
	a.revision++
	if !a.demo && !done {
		if _, err = a.runAction(ctx, sender, p.ClientID, func() (int64, error) {
			return listSender.SendListReply(ctx, target, rowID, row.Title, row.Description, response.ID)
		}); err != nil {
			var actionErr *Error
			if !errors.As(err, &actionErr) || actionErr.Code != "not_dispatched" {
				_, _ = a.db.ExecContext(context.Background(), "UPDATE messages SET status='unknown' WHERE id=? AND chat_id=?", response.ID, response.ChatID)
				response.Status = "unknown"
			}
			return nil, err
		}
		if _, err = a.db.ExecContext(ctx, "UPDATE messages SET status=CASE WHEN status IN ('read','delivered') THEN status ELSE 'accepted' END WHERE id=? AND chat_id=?", response.ID, response.ChatID); err != nil {
			return nil, fail("unknown_outcome", "List response may have reached WhatsApp; status could not be saved")
		}
		response.Status = "accepted"
		target.ListReplyAvailable = false
		target.ListReplyState = "accepted"
	}
	return map[string]any{"message": response, "target": target, "status": response.Status}, nil
}

func (a *App) blockExistingListReply(ctx context.Context, chat, target, clientID string) error {
	var status string
	err := a.db.QueryRowContext(ctx, `SELECT status FROM message_actions
		WHERE chat_id=? AND target_id=? AND method='list.reply' AND client_id<>?
		AND status IN ('reserved','dispatched','pending','unknown','accepted') LIMIT 1`, chat, target, clientID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "accepted" {
		return fail("already_replied", "A response to this single-select list was already accepted")
	}
	return fail("unknown_outcome", "A prior list response is unresolved; reconcile history before retrying")
}
