package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func readContactVCard(path string) ([]ContactCard, error) {
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".vcf") {
		return nil, fail("invalid_params", "Choose one local .vcf file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fail("invalid_contact", "The selected vCard file could not be read")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > ContactVCardMaxBytes {
		return nil, contactVCardError()
	}
	data, err := io.ReadAll(io.LimitReader(file, ContactVCardMaxBytes+1))
	if err != nil || int64(len(data)) != before.Size() || len(data) > ContactVCardMaxBytes {
		return nil, contactVCardError()
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fail("invalid_contact", "The selected vCard changed while it was being read")
	}
	return ParseVCard(data)
}

func (a *App) contactPrepare(_ context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := decode(raw, &params); err != nil {
		return nil, err
	}
	cards, err := readContactVCard(params.Path)
	if err != nil {
		return nil, err
	}
	return map[string]any{"contacts": cards}, nil
}

func (a *App) contactSend(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ChatID   string        `json:"chat_id"`
		Contacts []ContactCard `json:"contacts"`
		ClientID string        `json:"client_id"`
	}
	if err := decode(raw, &params); err != nil {
		return nil, err
	}
	if err := a.chatExists(ctx, params.ChatID); err != nil {
		return nil, err
	}
	if !actionID(params.ClientID) {
		return nil, fail("invalid_params", "Contact share client ID is invalid")
	}
	cards, err := normalizeContactCards(params.Contacts)
	if err != nil {
		return nil, err
	}
	projection, err := json.Marshal(cards)
	if err != nil {
		return nil, err
	}
	var sender ContactCardSender
	if !a.demo {
		if a.state != "connected" || a.transport == nil {
			return nil, fail("disconnected", "Connect WhatsApp before sharing a contact")
		}
		var ok bool
		sender, ok = a.transport.(ContactCardSender)
		if !ok {
			return nil, fail("unsupported", "The active transport cannot send contact cards")
		}
	}

	messages, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", params.ClientID)
	if err != nil {
		return nil, err
	}
	var message Message
	if len(messages) > 0 {
		message = messages[0]
		if message.ChatID != params.ChatID || message.Kind != "contact" {
			return nil, fail("conflict", "Client ID already belongs to another message")
		}
		if err = a.fillContactCards(ctx, params.ChatID, messages); err != nil {
			return nil, err
		}
		message = messages[0]
		oldProjection, marshalErr := json.Marshal(message.ContactCards)
		if marshalErr != nil || message.ContactCards == nil || string(oldProjection) != string(projection) {
			return nil, fail("conflict", "Client ID already belongs to another contact share")
		}
		if message.Status != "queued" || a.demo {
			return map[string]any{"message": message}, nil
		}
	}

	done := false
	if !a.demo {
		done, err = a.reserveAction(ctx, params.ClientID, "message.contact.send", params.ChatID, "", string(projection))
		if err != nil {
			return nil, err
		}
		if done {
			if len(messages) > 0 {
				return map[string]any{"message": message}, nil
			}
			return nil, fail("unknown_outcome", "Contact share was already accepted; reconcile history before retrying")
		}
	}

	if len(messages) == 0 {
		status := "demo_sent"
		if !a.demo {
			status = "queued"
		}
		message = Message{ID: params.ClientID, ChatID: params.ChatID, Timestamp: time.Now().UnixMilli(), Outgoing: true, Status: status, Kind: "contact", ContactCards: &cards}
		tx, beginErr := a.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return nil, beginErr
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,client_id,kind) VALUES(?,?,?,?,?,?,?,?)", message.ID, message.ChatID, message.Body, message.Timestamp, true, message.Status, params.ClientID, message.Kind); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO message_contacts(chat_id,message_id,projection) VALUES(?,?,?)", message.ChatID, message.ID, string(projection)); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		a.revision++
	}
	if a.demo {
		return map[string]any{"message": message}, nil
	}

	if _, err = a.db.ExecContext(ctx, "UPDATE messages SET status='sending' WHERE id=? AND chat_id=? AND status='queued'", message.ID, message.ChatID); err != nil {
		return nil, err
	}
	stamp, sendErr := a.runAction(ctx, nil, params.ClientID, func() (int64, error) {
		return sender.SendContacts(ctx, message.ChatID, cards, message.ID)
	})
	persistCtx := ctx
	if persistCtx.Err() != nil {
		persistCtx = context.Background()
	}
	if sendErr != nil {
		status := "unknown"
		var appErr *Error
		if errors.As(sendErr, &appErr) && appErr.Code == "not_dispatched" {
			status = "queued"
		}
		_, _ = a.db.ExecContext(persistCtx, `UPDATE messages SET status=CASE
			WHEN status='read' THEN 'read' WHEN status='delivered' THEN 'delivered' ELSE ? END
			WHERE id=? AND chat_id=?`, status, message.ID, message.ChatID)
		a.revision++
		return nil, sendErr
	}
	if _, err = a.db.ExecContext(persistCtx, `UPDATE messages SET status=CASE
		WHEN status='read' THEN 'read' WHEN status='delivered' THEN 'delivered' ELSE 'accepted' END,
		timestamp=CASE WHEN ?>0 THEN ? ELSE timestamp END WHERE id=? AND chat_id=?`, stamp, stamp, message.ID, message.ChatID); err != nil {
		return nil, fail("unknown_outcome", "Contact share may have been accepted; check history before retrying")
	}
	listed, err := a.queryMessages(persistCtx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE id=? AND chat_id=?", message.ID, message.ChatID)
	if err != nil || len(listed) == 0 {
		return nil, fail("unknown_outcome", "Contact share may have been accepted; check history before retrying")
	}
	if err = a.fillContactCards(persistCtx, message.ChatID, listed); err != nil {
		return nil, fail("unknown_outcome", "Contact share may have been accepted; check history before retrying")
	}
	a.revision++
	return map[string]any{"message": listed[0]}, nil
}
