package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type buttonReplyParams struct {
	ChatID      string `json:"chat_id"`
	TargetID    string `json:"target_id"`
	ButtonIndex int    `json:"button_index"`
	ClientID    string `json:"client_id"`
}

func (a *App) messageButtonReply(ctx context.Context, raw json.RawMessage) (any, error) {
	var p buttonReplyParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if !actionID(p.ClientID) || p.ButtonIndex < 0 {
		return nil, fail("invalid_params", "Button selection or client ID is invalid")
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.TargetID)
	if err != nil {
		return nil, err
	}
	if target.Deleted || target.Outgoing || target.Kind != "buttons" {
		return nil, fail("invalid_target", "Cannot select a response from this message")
	}
	hydrated := []Message{target}
	if err = a.fillButtonsProjections(ctx, p.ChatID, hydrated); err != nil {
		return nil, err
	}
	target = hydrated[0]
	switch target.ButtonReplyState {
	case "accepted", "local":
		return nil, fail("already_replied", "A response to this ButtonsMessage was already recorded")
	case "pending", "unknown":
		return nil, fail("unknown_outcome", "A prior button response is unresolved; reconcile history before retrying")
	}
	if target.Buttons == nil || !target.ButtonReplyAvailable {
		return nil, fail("unsupported", "This ButtonsMessage has no complete supported response mapping")
	}
	if p.ButtonIndex >= len(target.Buttons.Buttons) {
		return nil, fail("invalid_params", "Selected button index is out of range")
	}
	label := target.Buttons.Buttons[p.ButtonIndex]
	var buttonID string
	err = a.db.QueryRowContext(ctx, `SELECT button_id FROM message_button_actions
		WHERE chat_id=? AND message_id=? AND button_index=?`, p.ChatID, p.TargetID, p.ButtonIndex).Scan(&buttonID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fail("unsupported", "Selected button has no protocol response mapping")
	}
	if err != nil {
		return nil, err
	}
	if len(buttonID) == 0 || len(buttonID) > 256 || !buttonsTextValid(buttonID, 256, true) {
		return nil, fail("unsupported", "Selected button mapping is invalid")
	}

	existing, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", p.ClientID)
	if err != nil {
		return nil, err
	}
	var existingMessage *Message
	if len(existing) > 0 {
		candidate := existing[0]
		if candidate.ChatID != p.ChatID || candidate.Body != label {
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
	}

	var sender MessageActionTransport
	var buttonSender ButtonReplySender
	done := false
	payload, err := json.Marshal(struct {
		ButtonIndex int    `json:"button_index"`
		ButtonID    string `json:"button_id"`
	}{p.ButtonIndex, buttonID})
	if err != nil {
		return nil, err
	}
	if !a.demo {
		sender, err = a.liveActionTransport()
		if err != nil {
			return nil, err
		}
		buttonSender, _ = sender.(ButtonReplySender)
		if buttonSender == nil {
			return nil, fail("unsupported", "The connected transport cannot send ButtonsMessage responses")
		}
		if existingMessage == nil {
			if err = a.blockExistingButtonReply(ctx, p.ChatID, p.TargetID, p.ClientID); err != nil {
				return nil, err
			}
		}
		done, err = a.reserveAction(ctx, p.ClientID, "button.reply", p.ChatID, p.TargetID, string(payload))
		if err != nil {
			return nil, err
		}
	} else {
		var oldMethod string
		lookupErr := a.db.QueryRowContext(ctx, "SELECT method FROM message_actions WHERE client_id=?", p.ClientID).Scan(&oldMethod)
		if lookupErr == nil {
			return nil, fail("conflict", "Client ID already used for another action")
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return nil, lookupErr
		}
	}

	var response Message
	if existingMessage != nil {
		response = *existingMessage
		if done {
			if _, err = a.db.ExecContext(ctx, "UPDATE messages SET status=CASE WHEN status IN ('read','delivered') THEN status ELSE 'accepted' END WHERE id=? AND chat_id=?", response.ID, response.ChatID); err != nil {
				return nil, err
			}
			response.Status = "accepted"
			target.ButtonReplyAvailable = false
			target.ButtonReplyState = "accepted"
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
			ID: p.ClientID, ChatID: p.ChatID, Body: label, Timestamp: time.Now().UnixMilli(),
			Outgoing: true, Status: status, Kind: "text", QuotedBody: target.Body,
			QuotedSender: target.SenderName, ReplyToID: p.TargetID,
		}
		var tx *sql.Tx
		tx, err = a.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,client_id,kind,quoted_body,quoted_sender)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, response.ID, response.ChatID, response.Body, response.Timestamp,
			response.Outgoing, response.Status, p.ClientID, response.Kind, response.QuotedBody, response.QuotedSender); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO message_state(chat_id,message_id,reply_to_id,provenance) VALUES(?,?,?,'local_only')
			ON CONFLICT(chat_id,message_id) DO UPDATE SET reply_to_id=excluded.reply_to_id`, response.ChatID, response.ID, p.TargetID); err != nil {
			return nil, err
		}
		if a.demo {
			if _, err = tx.ExecContext(ctx, `INSERT INTO message_actions(client_id,chat_id,target_id,method,payload,status)
				VALUES(?,?,?,'button.reply',?,'demo_local')`, p.ClientID, p.ChatID, p.TargetID, string(payload)); err != nil {
				return nil, err
			}
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	a.revision++
	if a.demo {
		target.ButtonReplyAvailable = false
		target.ButtonReplyState = "local"
		return map[string]any{"message": response, "target": target, "status": response.Status}, nil
	}
	if !done {
		if _, err = a.runAction(ctx, sender, p.ClientID, func() (int64, error) {
			return buttonSender.SendButtonReply(ctx, target, buttonID, label, response.ID)
		}); err != nil {
			var actionErr *Error
			if !errors.As(err, &actionErr) || actionErr.Code != "not_dispatched" {
				_, _ = a.db.ExecContext(context.Background(), "UPDATE messages SET status='unknown' WHERE id=? AND chat_id=?", response.ID, response.ChatID)
				response.Status = "unknown"
			}
			return nil, err
		}
	}
	if _, err = a.db.ExecContext(ctx, "UPDATE messages SET status=CASE WHEN status IN ('read','delivered') THEN status ELSE 'accepted' END WHERE id=? AND chat_id=?", response.ID, response.ChatID); err != nil {
		return nil, fail("unknown_outcome", "Button response may have reached WhatsApp; status could not be saved")
	}
	response.Status = "accepted"
	target.ButtonReplyAvailable = false
	target.ButtonReplyState = "accepted"
	return map[string]any{"message": response, "target": target, "status": response.Status}, nil
}

func (a *App) blockExistingButtonReply(ctx context.Context, chat, target, clientID string) error {
	var status string
	err := a.db.QueryRowContext(ctx, `SELECT status FROM message_actions
		WHERE chat_id=? AND target_id=? AND method='button.reply' AND client_id<>?
		AND status IN ('reserved','dispatched','pending','unknown','accepted','demo_local') LIMIT 1`, chat, target, clientID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "accepted" || status == "demo_local" {
		return fail("already_replied", "A response to this ButtonsMessage was already recorded")
	}
	return fail("unknown_outcome", "A prior button response is unresolved; reconcile history before retrying")
}
