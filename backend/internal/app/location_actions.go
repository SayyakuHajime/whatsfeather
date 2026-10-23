package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type locationSendParams struct {
	ChatID    string   `json:"chat_id"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	Caption   string   `json:"caption"`
	Name      string   `json:"name"`
	Address   string   `json:"address"`
	Reviewed  bool     `json:"reviewed"`
	ClientID  string   `json:"client_id"`
}

func (a *App) locationSend(ctx context.Context, raw json.RawMessage) (any, error) {
	var params locationSendParams
	if err := decode(raw, &params); err != nil {
		return nil, err
	}
	if err := a.chatExists(ctx, params.ChatID); err != nil {
		return nil, err
	}
	if !params.Reviewed {
		return nil, fail("review_required", "Review the manual location before sending")
	}
	if !actionID(params.ClientID) {
		return nil, fail("invalid_params", "Location share client ID is invalid")
	}
	if params.Latitude == nil || params.Longitude == nil {
		return nil, fail("invalid_location", "Coordinates or location text are outside the safe limits")
	}
	projection := LocationProjection{
		Latitude: *params.Latitude, Longitude: *params.Longitude,
		Caption: params.Caption, Name: params.Name, Address: params.Address,
	}
	if !LocationProjectionValid(projection) {
		return nil, fail("invalid_location", "Coordinates or location text are outside the safe limits")
	}
	var sender LocationSender
	if !a.demo {
		if a.state != "connected" || a.transport == nil {
			return nil, fail("disconnected", "Connect WhatsApp before sharing a location")
		}
		var ok bool
		sender, ok = a.transport.(LocationSender)
		if !ok {
			return nil, fail("unsupported", "The active transport cannot send static locations")
		}
	}

	messages, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", params.ClientID)
	if err != nil {
		return nil, err
	}
	if len(messages) > 0 {
		message := messages[0]
		if message.ChatID != params.ChatID || message.Kind != "location" {
			return nil, fail("conflict", "Client ID already belongs to another message")
		}
		if err = a.fillLocationProjections(ctx, params.ChatID, messages); err != nil {
			return nil, err
		}
		message = messages[0]
		if message.Location == nil || *message.Location != projection {
			return nil, fail("conflict", "Client ID already belongs to another location share")
		}
		if a.demo || message.Status == "accepted" || message.Status == "delivered" || message.Status == "read" {
			return map[string]any{"message": message}, nil
		}
	}

	projectionJSON, err := json.Marshal(projection)
	if err != nil {
		return nil, err
	}
	if !a.demo {
		done, reserveErr := a.reserveAction(ctx, params.ClientID, "message.location.send", params.ChatID, "", string(projectionJSON))
		if reserveErr != nil {
			return nil, reserveErr
		}
		if done {
			if len(messages) == 0 {
				return nil, fail("unknown_outcome", "Location share was accepted; reconcile history before retrying")
			}
			return map[string]any{"message": messages[0]}, nil
		}
	}

	var message Message
	if len(messages) > 0 {
		message = messages[0]
		if message.Status != "queued" {
			return nil, fail("unknown_outcome", "Location share outcome is not known; reconcile history before retrying")
		}
	} else {
		status := "demo_sent"
		if !a.demo {
			status = "queued"
		}
		message = Message{ID: params.ClientID, ChatID: params.ChatID, Body: "Location", Timestamp: time.Now().UnixMilli(), Outgoing: true, Status: status, Kind: "location", Location: &projection}
		tx, beginErr := a.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return nil, beginErr
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,client_id,kind) VALUES(?,?,?,?,?,?,?,?)", message.ID, message.ChatID, message.Body, message.Timestamp, true, message.Status, params.ClientID, message.Kind); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO message_locations(chat_id,message_id,latitude,longitude,is_live,caption,name,address) VALUES(?,?,?,?,?,?,?,?)`,
			message.ChatID, message.ID, projection.Latitude, projection.Longitude, false, projection.Caption, projection.Name, projection.Address); err != nil {
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
		return sender.SendLocation(ctx, message.ChatID, projection, message.ID)
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
		if _, err = a.db.ExecContext(persistCtx, "UPDATE messages SET status=? WHERE id=? AND chat_id=?", status, message.ID, message.ChatID); err != nil {
			return nil, fail("unknown_outcome", "Location share may have been accepted; check history before retrying")
		}
		a.revision++
		return nil, sendErr
	}
	if _, err = a.db.ExecContext(persistCtx, `UPDATE messages SET status=CASE
		WHEN status='read' THEN 'read' WHEN status='delivered' THEN 'delivered' ELSE 'accepted' END,
		timestamp=CASE WHEN ?>0 THEN ? ELSE timestamp END WHERE id=? AND chat_id=?`, stamp, stamp, message.ID, message.ChatID); err != nil {
		return nil, fail("unknown_outcome", "Location share may have been accepted; check history before retrying")
	}
	listed, err := a.queryMessages(persistCtx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE id=? AND chat_id=?", message.ID, message.ChatID)
	if err != nil || len(listed) == 0 {
		return nil, fail("unknown_outcome", "Location share may have been accepted; check history before retrying")
	}
	if err = a.fillLocationProjections(persistCtx, message.ChatID, listed); err != nil {
		return nil, fail("unknown_outcome", "Location share may have been accepted; check history before retrying")
	}
	a.revision++
	return map[string]any{"message": listed[0]}, nil
}
