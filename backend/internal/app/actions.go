package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const editWindow = 15 * time.Minute
const revokeWindow = 48 * time.Hour

func (a *App) capabilities() map[string]any {
	methods := []string{
		"notification.poll", "poll.vote", "account.get", "app.capabilities", "app.status", "attachment.inspect", "attachment.prepare", "attachment.album.prepare", "attachment.album.release",
		"chat.info", "media.list", "media.library", "chat.list", "chat.open", "chat.self", "chat.update", "contact.list", "contact.prepare", "community.list", "community.refresh", "draft.get", "draft.set",
		"gif.prepare", "gif.providers", "gif.search", "history.sync", "message.attach", "message.album.send", "message.context",
		"message.contact.send", "message.delete", "message.download", "message.edit", "message.forward", "message.list", "message.location.send", "message.pin",
		"message.react", "message.reply", "list.reply", "button.reply", "message.search", "message.send", "message.star", "auth.connect", "auth.disconnect",
	}
	features := map[string]map[string]any{
		"chat-info-local":                 {"available": true, "reason": "chat.info reads selected-chat cached identity, retained counts, local-only organization flags, and stored community links; demo mode adds explicitly synthetic contact/group details only. Live participant, about, avatar, privacy, and presence fields are unavailable."},
		"media-library-local":             {"available": true, "reason": "media.list reads one selected chat from the local cache with bounded pages and optional media kind; synthetic native UI verifies reveal-in-chat without download, and full WhatsApp history is not claimed."},
		"reactions":                       {"available": true, "reason": "Outgoing protocol actions and incoming reaction events are implemented; demo reactions are explicitly local-only and live delivery remains unverified."},
		"reply":                           {"available": true, "reason": "Replies carry a protocol quote context in live mode; demo replies are local-only."},
		"list-message-replies":            {"available": a.demo, "reason": "Incoming single-select lists support explicit row selection; demo replies stay local and live support requires a typed transport."},
		"button-message-replies":          {"available": a.demo, "reason": "Legacy RESPONSE buttons support explicit selection; demo replies are local, and native-flow buttons remain display-only."},
		"account-profile-view":            {"available": true, "reason": "account.get exposes verified account identity when the SDK session is ready; username, about, and avatar are not supplied by this adapter."},
		"local-disconnect":                {"available": !a.demo && a.transport != nil, "reason": "Disconnect closes only the local WhatsFeather connection, cancels adapter work, and closes the device-store handle while preserving the linked-device store and cached local history; it does not request remote unlink/logout."},
		"remote-device-unlink":            {"available": false, "reason": "Remote unlink/logout is not implemented or exposed. Use WhatsApp Linked devices settings to remove a linked device; this app has no logout RPC and does not delete the local device store."},
		"native-media-send":               {"available": true, "reason": "Live image/document sends, static WebP stickers, and animated GIF-to-video are wired. Ordinary video is limited to validated MP4 with H.264 AVC 4:2:0 and optional AAC-LC; demo attachments are local."},
		"multi-attachment-albums":         {"available": a.demo, "reason": "Albums are limited to 2–10 validated still images, 20 MiB per prepared image, and 50 MiB total; preparation and sends are local-demo only."},
		"gif-search":                      {"available": true, "reason": "Wikimedia Commons search is available; GIPHY stays disabled until an owner-provided key and tested adapter exist."},
		"giphy-owner-key":                 {"available": false, "reason": "No owner-provided GIPHY key is configured and no GIPHY API adapter is implemented; no key is embedded."},
		"structured-content":              {"available": false, "reason": "Backend poll reads, votes, updates, and results exist; poll creation and interactive actions are not exposed. Location support is limited to received snapshots and manual static coordinates."},
		"contact-group-info":              {"available": false, "reason": "Only cached identity/counts are sourced in live mode; demo mode adds explicitly synthetic contact/group fixtures. Full live participants, about, avatar, presence, privacy, and profile details remain unavailable."},
		"shared-media-library":            {"available": true, "reason": "media.library reads bounded retained media metadata across cached conversations only; link-only messages and media not retained here are omitted, and no download/open is automatic. This is not complete WhatsApp history."},
		"edit-message":                    {"available": true, "reason": "Text edits are owner- and age-validated; live protocol acknowledgement is not live-account verified."},
		"delete-message":                  {"available": true, "reason": "Local delete is durable; live revoke is a separate owner- and age-validated protocol action."},
		"forward-message":                 {"available": true, "reason": "Text forwarding carries forwarding context; media forwarding is not implemented."},
		"star-message":                    {"available": true, "reason": "Star state is durable local metadata and is not synchronized to WhatsApp."},
		"message-pin":                     {"available": true, "reason": "Pin state is durable local metadata and is not synchronized to WhatsApp."},
		"chat-organization":               {"available": true, "reason": "Archive/pin/mute/unread/favorite/list state is durable local metadata only; no server sync is claimed."},
		"chat-creation-roster-management": {"available": true, "reason": "Cached roster reads and New chat phone lookup use the pinned SDK; chat.open requires a positive server registration result."},
		"voice-video-calls":               {"available": false, "reason": "No call transport, media session, or permission lifecycle is integrated."},
		"audio-voice-notes":               {"available": false, "reason": "Incoming audio descriptors persist and support explicit download; paused native playback controls are wired but codec/device acceptance is unverified. Validated outgoing OGG/Opus audio and voice-note transport paths plus native file selection, review and send have synthetic coverage, but live audio acceptance is unverified; microphone voice recording is not implemented."},
		"video":                           {"available": videoRuntimeAvailable(), "reason": "Validated MP4 with H.264 AVC 4:2:0 and optional AAC-LC is supported when ffmpeg/ffprobe are available; local Qt Multimedia playback starts paused and muted."},
		"stickers":                        {"available": true, "reason": "Static transparent 512x512 WebP stickers up to 100 KB can be sent. Animated sticker transport is not implemented; local sticker packs remain profile-local."},
		"polls-interactive-messages":      {"available": false, "reason": "Interactive vote encryption and poll update decoding are not implemented."},
		"contact-cards":                   {"available": a.demo, "reason": "Bounded vCard projection, local review, typed sharing, and cached rendering are available in demo; live transport remains experimental and unverified."},
		"location-messages":               {"available": a.demo, "reason": "Static location messages use manually entered coordinates; no native location provider or live updates are implemented."},
		"location-live-location":          {"available": false, "reason": "Incoming live locations are received snapshots only; active tracking and live updates are not implemented."},
		"profile-edit":                    {"available": false, "reason": "Profile mutations are not exposed without a confirmed backend path."},
		"privacy-security-settings":       {"available": false, "reason": "Privacy settings are not exposed pending explicit confirmation and transport verification."},
		"groups-management":               {"available": false, "reason": "Group mutation routes are not exposed by this backend contract."},
		"channels":                        {"available": false, "reason": "Channel read routes are not integrated into the typed cache."},
		"communities":                     {"available": false, "reason": "The broader community hub is not integrated; read-only cached community folders are available separately."},
		"community-folders":               {"available": true, "reason": "Read-only community folders use pinned SDK GetJoinedGroups metadata; live refresh is explicit and connected-session-only. Demo folders are marked synthetic; no live account compatibility was exercised."},
		"whatsapp-status-stories":         {"available": false, "reason": "Status-view data is not available through the implemented SDK event path."},
	}
	_, pollSender := a.transport.(PollVoteSender)
	features["poll-voting"] = map[string]any{"available": !a.demo && pollSender, "reason": "Explicit backend voting uses SDK encryption and durable action IDs; native voting controls and live compatibility are unverified."}
	features["poll-updates"] = map[string]any{"available": true, "reason": "The backend stores validated decoded votes; the WhatsApp adapter uses SDK decryption. No native update action or live compatibility is claimed."}
	features["poll-results"] = map[string]any{"available": true, "reason": "Backend updates use SDK decryption and private latest-voter storage; message.list exposes bounded aggregate results. Synthetic local evidence only."}
	features["polls-interactive-messages"]["reason"] = "Backend poll voting and aggregate results exist; poll creation and interactive actions are not exposed."
	features["community-folders"]["reason"] = a.communityFeatureReason()
	if !a.demo && a.transport != nil {
		_, features["location-messages"]["available"] = a.transport.(LocationSender)
		if features["location-messages"]["available"] == false {
			features["location-messages"]["reason"] = "The active transport cannot send static location messages."
		}
	}
	protocolActions := a.demo
	if !a.demo {
		_, protocolActions = a.transport.(MessageActionTransport)
	}
	for _, id := range []string{"reactions", "reply", "edit-message", "delete-message", "forward-message"} {
		if !protocolActions && id != "delete-message" {
			features[id]["available"] = false
			features[id]["reason"] = "The active transport does not implement this protocol action."
		}
	}
	if !a.demo && a.transport != nil {
		_, features["contact-cards"]["available"] = a.transport.(ContactCardSender)
		if features["contact-cards"]["available"] == false {
			features["contact-cards"]["reason"] = "The active transport cannot send typed contact cards."
		}
		_, features["list-message-replies"]["available"] = a.transport.(ListReplySender)
		_, features["button-message-replies"]["available"] = a.transport.(ButtonReplySender)
	}
	if a.demo {
		methods = append(methods, "group.create", "group.update", "group.participants", "group.join", "group.leave")
		features["groups-management"] = map[string]any{"available": true, "reason": "Locally-created groups and synthetic participants persist only in the demo profile; no live group mutation is exposed."}
	}
	if a.demo || a.transport == nil {
		features["chat-creation-roster-management"]["available"] = false
		features["chat-creation-roster-management"]["reason"] = "A live connected transport with verified phone lookup is required; demo mode does not fabricate contacts."
	} else if _, ok := a.transport.(ContactDirectory); !ok {
		features["chat-creation-roster-management"]["available"] = false
		features["chat-creation-roster-management"]["reason"] = "The active transport does not expose verified phone lookup or a cached roster."
	}
	limits := map[string]any{"frame_bytes": 256 * 1024, "hydrated_page_bytes": pageResultBudget, "message_characters": 16000, "contact_page_size": 100, "chat_page_size": 1000, "chat_page_bytes": pageResultBudget, "community_page_size": communityPageMax, "community_page_bytes": communityPageBudget}
	return map[string]any{"version": 1, "methods": methods, "features": features, "limits": limits}
}

func actionID(value string) bool { return value != "" && len(value) <= 128 }

func (a *App) loadTarget(ctx context.Context, chat, id string) (Message, error) {
	if err := a.chatExists(ctx, chat); err != nil {
		return Message{}, err
	}
	var m Message
	err := a.db.QueryRowContext(ctx, `SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=? AND id=?`, chat, id).
		Scan(&m.ID, &m.ChatID, &m.Body, &m.Timestamp, &m.Outgoing, &m.Status, &m.Kind, &m.SenderName, &m.SenderID, &m.QuotedBody, &m.QuotedSender)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, fail("not_found", "Message is not stored in this conversation")
	}
	if err != nil {
		return Message{}, err
	}
	messages := []Message{m}
	if err = a.fillMessageState(ctx, chat, messages); err != nil {
		return Message{}, err
	}
	return messages[0], nil
}

func (a *App) requireReactionReconciled(ctx context.Context, clientID, chat, target string) error {
	var unresolvedID string
	err := a.db.QueryRowContext(ctx, `SELECT client_id FROM message_actions
		WHERE chat_id=? AND target_id=? AND method='message.react'
		AND status IN ('reserved','pending','dispatched','unknown') AND client_id<>?
		ORDER BY created_at LIMIT 1`, chat, target, clientID).Scan(&unresolvedID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fail("unknown_outcome", "A prior reaction is unresolved; reconcile synchronized message state before trying again")
}

func (a *App) reserveAction(ctx context.Context, clientID, method, chat, target, payload string) (bool, error) {
	if !actionID(clientID) {
		return false, fail("invalid_params", "Client ID is invalid")
	}
	var oldChat, oldTarget, oldMethod, oldPayload, status string
	err := a.db.QueryRowContext(ctx, "SELECT chat_id,target_id,method,payload,status FROM message_actions WHERE client_id=?", clientID).Scan(&oldChat, &oldTarget, &oldMethod, &oldPayload, &status)
	if err == nil {
		if oldChat != chat || oldTarget != target || oldMethod != method || oldPayload != payload {
			return false, fail("conflict", "Client ID already used for another action")
		}
		switch status {
		case "accepted", "reconciled":
			return true, nil
		case "rejected":
			return false, fail("rejected", "Action was rejected; use a new action ID")
		case "reserved":
			return false, nil
		case "not_dispatched":
			result, updateErr := a.db.ExecContext(ctx, "UPDATE message_actions SET status='reserved' WHERE client_id=? AND status='not_dispatched'", clientID)
			if updateErr != nil {
				return false, updateErr
			}
			changed, updateErr := result.RowsAffected()
			if updateErr != nil {
				return false, updateErr
			}
			if changed != 1 {
				return false, fail("unknown_outcome", "Action state changed; reconcile before retrying")
			}
			return false, nil
		default:
			return false, fail("unknown_outcome", "Action outcome is not known; reconcile history before retrying")
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = a.db.ExecContext(ctx, "INSERT INTO message_actions(client_id,chat_id,target_id,method,payload,status) VALUES(?,?,?,?,?,'reserved')", clientID, chat, target, method, payload)
	return false, err
}

func (a *App) finishAction(ctx context.Context, clientID, status string) error {
	if status != "accepted" && status != "unknown" && status != "rejected" {
		return fail("invalid_action_state", "Unsupported action completion state")
	}
	query := "UPDATE message_actions SET status=? WHERE client_id=? AND status='dispatched'"
	if status == "unknown" {
		query = "UPDATE message_actions SET status=? WHERE client_id=? AND status IN ('dispatched','accepted')"
	}
	result, err := a.db.ExecContext(ctx, query, status, clientID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		if status == "accepted" {
			var currentStatus string
			if checkErr := a.db.QueryRowContext(context.Background(), "SELECT status FROM message_actions WHERE client_id=?", clientID).Scan(&currentStatus); checkErr == nil && currentStatus == "reconciled" {
				return nil
			}
		}
		return fail("unknown_outcome", "Dispatched action outcome could not be recorded")
	}
	return nil
}

func (a *App) reactionActionReconciled(clientID string) bool {
	var status string
	err := a.db.QueryRowContext(context.Background(), `SELECT status FROM message_actions
		WHERE client_id=? AND method='message.react'`, clientID).Scan(&status)
	return err == nil && status == "reconciled"
}

func (a *App) dispatchAction(ctx context.Context, clientID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE message_actions SET status='dispatched' WHERE client_id=? AND status='reserved'", clientID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fail("not_dispatched", "Action was not dispatched; the action state was not reserved")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE message_actions SET dispatched_at_ms=?
		WHERE client_id=? AND method='message.react' AND actor_id<>''`, time.Now().UnixMilli(), clientID); err != nil {
		return err
	}
	// Record dispatch order, rather than completion or reservation order. A late
	// response or replay must not replace the most recent self-vote attempt.
	if _, err = tx.ExecContext(ctx, `INSERT INTO poll_vote_dispatches(client_id,chat_id,message_id)
		SELECT client_id,chat_id,target_id FROM message_actions WHERE client_id=? AND method='poll.vote'`, clientID); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) markActionNotDispatched(clientID string) error {
	result, err := a.db.ExecContext(context.Background(), "UPDATE message_actions SET status='not_dispatched' WHERE client_id=? AND status='reserved'", clientID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fail("unknown_outcome", "Pre-dispatch action state could not be recorded")
	}
	return nil
}

func (a *App) liveActionTransport() (MessageActionTransport, error) {
	if a.demo {
		return nil, nil
	}
	if a.state != "connected" || a.transport == nil {
		return nil, fail("disconnected", "Connect WhatsApp before this action")
	}
	sender, ok := a.transport.(MessageActionTransport)
	if !ok {
		return nil, fail("unsupported", "This message action is unavailable in the connected transport")
	}
	return sender, nil
}

func (a *App) runAction(ctx context.Context, sender MessageActionTransport, clientID string, send func() (int64, error)) (int64, error) {
	if err := a.dispatchAction(ctx, clientID); err != nil {
		if markErr := a.markActionNotDispatched(clientID); markErr != nil {
			return 0, fail("unknown_outcome", "Action was not sent, but its durable pre-dispatch state could not be confirmed")
		}
		return 0, fail("not_dispatched", "Action was not sent; retrying this action ID is safe")
	}
	a.beginOperationLocked()
	defer a.endOperationLocked()
	a.mu.Unlock()
	stamp, sendErr := send()
	a.mu.Lock()
	if a.closing || a.closed {
		_ = a.finishAction(context.Background(), clientID, "unknown")
		return 0, fail("unknown_outcome", "Action may have reached WhatsApp while the app was closing")
	}
	if sendErr != nil {
		if rejected, ok := sendErr.(interface{ PollVoteRejected() bool }); ok && rejected.PollVoteRejected() {
			if err := a.finishAction(context.Background(), clientID, "rejected"); err != nil {
				return 0, fail("unknown_outcome", "Rejection could not be saved")
			}
			return 0, fail("rejected", "Vote was rejected before send")
		}
		if a.reactionActionReconciled(clientID) {
			return 0, nil
		}
		_ = a.finishAction(context.Background(), clientID, "unknown")
		if a.reactionActionReconciled(clientID) {
			return 0, nil
		}
		return 0, fail("unknown_outcome", "Action may have reached WhatsApp; check message history before retrying")
	}
	if err := a.finishAction(ctx, clientID, "accepted"); err != nil {
		_ = a.finishAction(context.Background(), clientID, "unknown")
		return 0, fail("unknown_outcome", "Action may have reached WhatsApp; reconcile before retrying")
	}
	return stamp, nil
}

func (a *App) messageReply(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID   string `json:"chat_id"`
		TargetID string `json:"target_id"`
		Text     string `json:"text"`
		ClientID string `json:"client_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Text) == "" || utf8.RuneCountInString(p.Text) > 16000 || !actionID(p.ClientID) {
		return nil, fail("invalid_params", "Message or client ID is invalid")
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.TargetID)
	if err != nil {
		return nil, err
	}
	if target.Deleted {
		return nil, fail("invalid_target", "Cannot reply to a deleted message")
	}
	if target.Kind != "" && target.Kind != "text" {
		return nil, fail("unsupported", "Protocol replies currently support text messages only")
	}
	var existingMessage *Message
	existing, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", p.ClientID)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		candidate := existing[0]
		if candidate.ChatID != p.ChatID || candidate.Body != p.Text {
			return nil, fail("conflict", "Client ID already used for another message")
		}
		if err = a.fillMessageState(ctx, candidate.ChatID, existing); err != nil {
			return nil, err
		}
		if existing[0].ReplyToID != p.TargetID {
			return nil, fail("conflict", "Client ID already used for another message")
		}
		candidate = existing[0]
		if a.demo {
			return map[string]any{"message": candidate}, nil
		}
		var actionExists int
		err = a.db.QueryRowContext(ctx, "SELECT 1 FROM message_actions WHERE client_id=?", p.ClientID).Scan(&actionExists)
		if errors.Is(err, sql.ErrNoRows) {
			// Historical replies had no durable action row. Return them without
			// replaying a transport call whose old outcome cannot be established.
			return map[string]any{"message": candidate}, nil
		}
		if err != nil {
			return nil, err
		}
		existingMessage = &candidate
	}
	sender, err := a.liveActionTransport()
	if err != nil {
		return nil, err
	}
	done := false
	if !a.demo {
		done, err = a.reserveAction(ctx, p.ClientID, "message.reply", p.ChatID, p.TargetID, p.Text)
		if err != nil {
			return nil, err
		}
	}
	var m Message
	if existingMessage != nil {
		m = *existingMessage
		if _, err = a.db.ExecContext(ctx, "UPDATE messages SET status='queued' WHERE id=? AND chat_id=? AND status NOT IN ('read','delivered')", m.ID, m.ChatID); err != nil {
			return nil, err
		}
	} else {
		status := "demo_sent"
		if !a.demo {
			status = "queued"
		}
		m = Message{ID: p.ClientID, ChatID: p.ChatID, Body: p.Text, Timestamp: time.Now().UnixMilli(), Outgoing: true, Status: status, Kind: "text", QuotedBody: target.Body, QuotedSender: target.SenderName, ReplyToID: p.TargetID}
		if _, err = a.db.ExecContext(ctx, "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,client_id,kind,quoted_body,quoted_sender) VALUES(?,?,?,?,?,?,?,?,?,?)", m.ID, m.ChatID, m.Body, m.Timestamp, true, m.Status, p.ClientID, m.Kind, m.QuotedBody, m.QuotedSender); err != nil {
			return nil, err
		}
	}
	if _, err = a.db.ExecContext(ctx, `INSERT INTO message_state(chat_id,message_id,reply_to_id,provenance) VALUES(?,?,?,'local_only') ON CONFLICT(chat_id,message_id) DO UPDATE SET reply_to_id=excluded.reply_to_id`, m.ChatID, m.ID, p.TargetID); err != nil {
		return nil, err
	}
	a.revision++
	if !a.demo {
		if !done {
			if _, err = a.runAction(ctx, sender, p.ClientID, func() (int64, error) {
				return sender.SendReply(ctx, m.ChatID, p.TargetID, target.Outgoing, target.SenderID, target.Body, m.Body, m.ID)
			}); err != nil {
				var actionErr *Error
				if !errors.As(err, &actionErr) || actionErr.Code != "not_dispatched" {
					_, _ = a.db.ExecContext(context.Background(), "UPDATE messages SET status='unknown' WHERE id=? AND chat_id=?", m.ID, m.ChatID)
				}
				return nil, err
			}
		}
		_, _ = a.db.ExecContext(ctx, "UPDATE messages SET status=CASE WHEN status IN ('read','delivered') THEN status ELSE 'accepted' END WHERE id=? AND chat_id=?", m.ID, m.ChatID)
	}
	rows, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE id=? AND chat_id=?", m.ID, m.ChatID)
	if err != nil || len(rows) == 0 {
		return nil, fail("unknown_outcome", "Reply was persisted; check history")
	}
	return map[string]any{"message": rows[0]}, nil
}

func (a *App) messageReact(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID   string `json:"chat_id"`
		TargetID string `json:"target_id"`
		Reaction string `json:"reaction"`
		ClientID string `json:"client_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if !actionID(p.ClientID) || utf8.RuneCountInString(p.Reaction) > 16 {
		return nil, fail("invalid_params", "Reaction or client ID is invalid")
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.TargetID)
	if err != nil {
		return nil, err
	}
	if target.Deleted {
		return nil, fail("invalid_target", "Cannot react to a deleted message")
	}
	sender, err := a.liveActionTransport()
	if err != nil {
		return nil, err
	}
	actor := "self"
	if !a.demo {
		provider, ok := a.transport.(AccountProvider)
		if !ok {
			return nil, fail("unsupported", "The connected account cannot identify the reaction sender")
		}
		epoch := a.sessionEpoch
		a.beginOperationLocked()
		defer a.endOperationLocked()
		a.mu.Unlock()
		account, e := provider.Account(ctx)
		a.mu.Lock()
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		if a.sessionEpoch != epoch || a.state != "connected" {
			return nil, fail("disconnected", "The connection changed before the reaction could be sent")
		}
		if e != nil || !account.Available || account.State != "connected" || strings.TrimSpace(account.JID) == "" {
			return nil, fail("account_unavailable", "The connected account could not be verified")
		}
		actor = account.JID
	}
	payload := p.Reaction
	done := false
	if !a.demo {
		if err = a.requireReactionReconciled(ctx, p.ClientID, p.ChatID, p.TargetID); err != nil {
			return nil, err
		}
		done, err = a.reserveAction(ctx, p.ClientID, "message.react", p.ChatID, p.TargetID, payload)
		if err != nil {
			return nil, err
		}
		if !done {
			result, updateErr := a.db.ExecContext(ctx, `UPDATE message_actions SET actor_id=?
				WHERE client_id=? AND method='message.react' AND status='reserved'
				AND (actor_id='' OR actor_id=?)`, actor, p.ClientID, actor)
			if updateErr == nil {
				var changed int64
				changed, updateErr = result.RowsAffected()
				if updateErr == nil && changed != 1 {
					updateErr = fail("conflict", "Reaction action belongs to another account or is no longer reserved")
				}
			}
			if updateErr != nil {
				if markErr := a.markActionNotDispatched(p.ClientID); markErr != nil {
					return nil, fail("unknown_outcome", "Reaction was not sent, but its pre-dispatch state could not be recorded")
				}
				return nil, updateErr
			}
		}
	}
	provenance := "local_only"
	if !a.demo {
		provenance = "accepted"
		if !done {
			if _, err = a.runAction(ctx, sender, p.ClientID, func() (int64, error) {
				return sender.SendReaction(ctx, p.ChatID, p.TargetID, target.Outgoing, target.SenderID, p.Reaction, p.ClientID)
			}); err != nil {
				return nil, err
			}
		}
	}
	if !a.demo {
		var actionStatus, actionActor string
		if err = a.db.QueryRowContext(ctx, "SELECT status,actor_id FROM message_actions WHERE client_id=?", p.ClientID).Scan(&actionStatus, &actionActor); err != nil {
			return nil, err
		}
		if actionActor != "" && actionActor != actor {
			return nil, fail("conflict", "Reaction action belongs to a different account")
		}
		if actionStatus == "reconciled" {
			var observed string
			readErr := a.db.QueryRowContext(ctx, "SELECT emoji FROM message_reactions WHERE chat_id=? AND message_id=? AND sender_id=?", p.ChatID, p.TargetID, actor).Scan(&observed)
			if errors.Is(readErr, sql.ErrNoRows) {
				observed = ""
			} else if readErr != nil {
				return nil, readErr
			}
			if observed != p.Reaction {
				return nil, fail("reconciled", "Remote reaction state changed; review the current message before creating another action")
			}
			updated, loadErr := a.loadTarget(ctx, p.ChatID, p.TargetID)
			if loadErr != nil {
				return nil, loadErr
			}
			return map[string]any{"chat_id": p.ChatID, "target_id": p.TargetID, "reaction": p.Reaction, "provenance": "remote", "message": updated}, nil
		}
	}
	if p.Reaction == "" {
		_, err = a.db.ExecContext(ctx, "DELETE FROM message_reactions WHERE chat_id=? AND message_id=? AND sender_id=?", p.ChatID, p.TargetID, actor)
	} else {
		_, err = a.db.ExecContext(ctx, `INSERT INTO message_reactions(chat_id,message_id,sender_id,emoji,timestamp,provenance) VALUES(?,?,?,?,?,?) ON CONFLICT(chat_id,message_id,sender_id) DO UPDATE SET emoji=excluded.emoji,timestamp=excluded.timestamp,provenance=excluded.provenance`, p.ChatID, p.TargetID, actor, p.Reaction, time.Now().UnixMilli(), provenance)
	}
	if err != nil {
		return nil, err
	}
	a.revision++
	updated, err := a.loadTarget(ctx, p.ChatID, p.TargetID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"chat_id": p.ChatID, "target_id": p.TargetID, "reaction": p.Reaction, "provenance": provenance, "message": updated}, nil
}

func (a *App) messageEdit(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID   string `json:"chat_id"`
		ID       string `json:"id"`
		Text     string `json:"text"`
		ClientID string `json:"client_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Text) == "" || utf8.RuneCountInString(p.Text) > 16000 || !actionID(p.ClientID) {
		return nil, fail("invalid_params", "Edit text or client ID is invalid")
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.ID)
	if err != nil {
		return nil, err
	}
	if !target.Outgoing || (target.Kind != "" && target.Kind != "text") || target.Deleted {
		return nil, fail("not_allowed", "Only your own undeleted text messages can be edited")
	}
	if !a.demo {
		age := time.Since(time.UnixMilli(target.Timestamp))
		if age < 0 || age > editWindow {
			return nil, fail("expired", "Message is outside the edit window")
		}
	}
	sender, err := a.liveActionTransport()
	if err != nil {
		return nil, err
	}
	done := false
	if !a.demo {
		done, err = a.reserveAction(ctx, p.ClientID, "message.edit", p.ChatID, p.ID, p.Text)
		if err != nil {
			return nil, err
		}
	}
	if !a.demo && !done {
		if _, err = a.runAction(ctx, sender, p.ClientID, func() (int64, error) { return sender.EditMessage(ctx, p.ChatID, p.ID, p.Text, p.ClientID) }); err != nil {
			return nil, err
		}
	}
	provenance := "local_only"
	if !a.demo {
		provenance = "accepted"
	}
	if a.demo || !done {
		result, updateErr := a.db.ExecContext(ctx, "UPDATE messages SET body=? WHERE chat_id=? AND id=? AND body=?", p.Text, p.ChatID, p.ID, target.Body)
		if updateErr != nil {
			return nil, updateErr
		}
		changed, updateErr := result.RowsAffected()
		if updateErr != nil {
			return nil, updateErr
		}
		if changed > 0 {
			if _, updateErr = a.db.ExecContext(ctx, `INSERT INTO message_state(chat_id,message_id,edited,provenance) VALUES(?,?,1,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET edited=1,provenance=excluded.provenance`, p.ChatID, p.ID, provenance); updateErr != nil {
				return nil, updateErr
			}
			a.revision++
		}
	}
	m, err := a.loadTarget(ctx, p.ChatID, p.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"message": m}, nil
}

func (a *App) applyMessageDeletion(ctx context.Context, chatID, messageID, provenance string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE messages SET body='[Message deleted]' WHERE chat_id=? AND id=?", chatID, messageID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fail("not_found", "Message is not stored in this conversation")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_state(chat_id,message_id,deleted,provenance) VALUES(?,?,1,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET deleted=1,provenance=excluded.provenance`, chatID, messageID, provenance); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM attachments WHERE chat_id=? AND message_id=?", chatID, messageID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM message_albums WHERE chat_id=? AND message_id=?", chatID, messageID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM message_reactions WHERE chat_id=? AND message_id=?", chatID, messageID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM message_link_previews WHERE chat_id=? AND message_id=?", chatID, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) messageDelete(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID   string `json:"chat_id"`
		ID       string `json:"id"`
		Scope    string `json:"scope"`
		ClientID string `json:"client_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Scope != "local" && p.Scope != "everyone" {
		return nil, fail("invalid_params", "Delete scope must be local or everyone")
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.ID)
	if err != nil {
		return nil, err
	}
	provenance := "local_only"
	if p.Scope == "everyone" {
		if !target.Outgoing {
			return nil, fail("not_allowed", "Only your own messages can be revoked")
		}
		if !a.demo {
			age := time.Since(time.UnixMilli(target.Timestamp))
			if age < 0 || age > revokeWindow {
				return nil, fail("expired", "Message is outside the revoke window")
			}
		}
		sender, e := a.liveActionTransport()
		if e != nil {
			return nil, e
		}
		if a.demo {
			return nil, fail("unsupported", "Demo mode cannot revoke a message for everyone")
		}
		done, e := a.reserveAction(ctx, p.ClientID, "message.delete", p.ChatID, p.ID, p.Scope)
		if e != nil {
			return nil, e
		}
		if !done {
			if _, e = a.runAction(ctx, sender, p.ClientID, func() (int64, error) { return sender.RevokeMessage(ctx, p.ChatID, p.ID, p.ClientID) }); e != nil {
				return nil, e
			}
		}
		provenance = "accepted"
	}
	if err = a.applyMessageDeletion(ctx, p.ChatID, p.ID, provenance); err != nil {
		return nil, err
	}
	a.revision++
	updated, err := a.loadTarget(ctx, p.ChatID, p.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"chat_id": p.ChatID, "id": p.ID, "scope": p.Scope, "provenance": provenance, "message": updated}, nil
}

func (a *App) messageForward(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID       string `json:"chat_id"`
		ID           string `json:"id"`
		TargetChatID string `json:"target_chat_id"`
		ClientID     string `json:"client_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if !actionID(p.ClientID) {
		return nil, fail("invalid_params", "Client ID is invalid")
	}
	if scope, ok := a.transport.(ScopedLiveTestProvider); ok && scope.ScopedLiveTest() {
		return nil, fail("unsupported", "Historical message forwarding is disabled in scoped live-test mode")
	}
	if err := a.chatExists(ctx, p.TargetChatID); err != nil {
		return nil, err
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.ID)
	if err != nil {
		return nil, err
	}
	if target.Deleted {
		return nil, fail("invalid_target", "Cannot forward a deleted message")
	}
	if target.Attachment != nil || (target.Kind != "" && target.Kind != "text") {
		return nil, fail("unsupported", "Only plain text forwarding is supported")
	}
	var existingMessage *Message
	existing, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", p.ClientID)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		candidate := existing[0]
		if candidate.ChatID != p.TargetChatID || candidate.Body != target.Body {
			return nil, fail("conflict", "Client ID already used for another message")
		}
		if a.demo {
			return map[string]any{"message": candidate}, nil
		}
		var actionExists int
		err = a.db.QueryRowContext(ctx, "SELECT 1 FROM message_actions WHERE client_id=?", p.ClientID).Scan(&actionExists)
		if errors.Is(err, sql.ErrNoRows) {
			// Older forwarding rows have no outcome record. Never replay them.
			return map[string]any{"message": candidate}, nil
		}
		if err != nil {
			return nil, err
		}
		existingMessage = &candidate
	}
	sender, err := a.liveActionTransport()
	if err != nil {
		return nil, err
	}
	status := "demo_sent"
	provenance := "local_only"
	done := false
	if !a.demo {
		status = "queued"
		done, err = a.reserveAction(ctx, p.ClientID, "message.forward", p.TargetChatID, p.ID, target.Body)
		if err != nil {
			return nil, err
		}
	}
	var m Message
	if existingMessage != nil {
		m = *existingMessage
		if _, err = a.db.ExecContext(ctx, "UPDATE messages SET status='queued' WHERE id=? AND chat_id=? AND status NOT IN ('read','delivered')", m.ID, m.ChatID); err != nil {
			return nil, err
		}
	} else {
		m = Message{ID: p.ClientID, ChatID: p.TargetChatID, Body: target.Body, Timestamp: time.Now().UnixMilli(), Outgoing: true, Status: status, Kind: "text", Forwarded: true, MutationProvenance: provenance}
		if _, err = a.db.ExecContext(ctx, "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,client_id,kind) VALUES(?,?,?,?,?,?,?,?)", m.ID, m.ChatID, m.Body, m.Timestamp, true, m.Status, p.ClientID, m.Kind); err != nil {
			return nil, err
		}
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO message_state(chat_id,message_id,forwarded,provenance) VALUES(?,?,1,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET forwarded=1,provenance=excluded.provenance`, m.ChatID, m.ID, provenance)
	if err != nil {
		return nil, err
	}
	a.revision++
	if !a.demo {
		if !done {
			if _, err = a.runAction(ctx, sender, p.ClientID, func() (int64, error) { return sender.ForwardText(ctx, p.TargetChatID, target.Body, p.ClientID) }); err != nil {
				var actionErr *Error
				if !errors.As(err, &actionErr) || actionErr.Code != "not_dispatched" {
					_, _ = a.db.ExecContext(context.Background(), "UPDATE messages SET status='unknown' WHERE chat_id=? AND id=?", m.ChatID, m.ID)
					_, _ = a.db.ExecContext(context.Background(), "UPDATE message_state SET provenance='unknown' WHERE chat_id=? AND message_id=?", m.ChatID, m.ID)
				}
				return nil, err
			}
		}
		_, _ = a.db.ExecContext(ctx, "UPDATE message_state SET provenance='accepted' WHERE chat_id=? AND message_id=?", m.ChatID, m.ID)
		_, _ = a.db.ExecContext(ctx, "UPDATE messages SET status='accepted' WHERE chat_id=? AND id=? AND status NOT IN ('read','delivered')", m.ChatID, m.ID)
	}
	rows, err := a.queryMessages(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=? AND id=?", m.ChatID, m.ID)
	if err != nil || len(rows) == 0 {
		return nil, fail("unknown_outcome", "Forward was persisted; check destination history")
	}
	return map[string]any{"message": rows[0]}, nil
}

func (a *App) messageFlag(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID   string `json:"chat_id"`
		ID       string `json:"id"`
		Starred  *bool  `json:"starred"`
		Pinned   *bool  `json:"pinned"`
		Duration int    `json:"duration_seconds"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.ID)
	if err != nil {
		return nil, err
	}
	if target.Deleted {
		return nil, fail("invalid_target", "Cannot change flags on a deleted message")
	}
	column := "starred"
	value := p.Starred
	if method == "message.pin" {
		column = "pinned"
		value = p.Pinned
		if p.Duration < 0 || p.Duration > 86400 {
			return nil, fail("invalid_params", "Pin duration must be between 0 and 86400 seconds")
		}
	}
	if value == nil {
		return nil, fail("invalid_params", "Flag value is required")
	}
	_, err = a.db.ExecContext(ctx, "INSERT INTO message_state(chat_id,message_id,"+column+",provenance) VALUES(?,?,?,'local_only') ON CONFLICT(chat_id,message_id) DO UPDATE SET "+column+"=excluded."+column+",provenance='local_only'", p.ChatID, p.ID, *value)
	if err != nil {
		return nil, err
	}
	a.revision++
	return map[string]any{"chat_id": p.ChatID, "id": p.ID, "value": *value, "provenance": "local_only"}, nil
}

func (a *App) chatUpdate(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID   string    `json:"chat_id"`
		Archived *bool     `json:"archived"`
		Pinned   *bool     `json:"pinned"`
		Muted    *bool     `json:"muted"`
		Unread   *bool     `json:"unread"`
		Favorite *bool     `json:"favorite"`
		ListIDs  *[]string `json:"list_ids"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if err := a.chatExists(ctx, p.ChatID); err != nil {
		return nil, err
	}
	if p.Archived == nil && p.Pinned == nil && p.Muted == nil && p.Unread == nil && p.Favorite == nil && p.ListIDs == nil {
		return nil, fail("invalid_params", "At least one chat field must be provided")
	}
	var lists []string
	if p.ListIDs != nil {
		seen := map[string]bool{}
		for _, id := range *p.ListIDs {
			if strings.TrimSpace(id) == "" || len(id) > 64 || strings.ContainsAny(id, "\x00\r\n") || seen[id] {
				return nil, fail("invalid_params", "List IDs are invalid")
			}
			seen[id] = true
			lists = append(lists, id)
			if len(lists) > 32 {
				return nil, fail("invalid_params", "At most 32 list IDs are allowed")
			}
		}
	}
	var current Chat
	var rawLists string
	err := a.db.QueryRowContext(ctx, "SELECT archived,pinned,muted,unread_mark,favorite,list_ids FROM chat_organization WHERE chat_id=?", p.ChatID).Scan(&current.Archived, &current.Pinned, &current.Muted, &current.UnreadMark, &current.Favorite, &rawLists)
	if errors.Is(err, sql.ErrNoRows) {
		rawLists = "[]"
		err = nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(rawLists), &current.ListIDs)
	if p.Archived != nil {
		current.Archived = *p.Archived
	}
	if p.Pinned != nil {
		current.Pinned = *p.Pinned
	}
	if p.Muted != nil {
		current.Muted = *p.Muted
	}
	if p.Unread != nil {
		current.UnreadMark = *p.Unread
	}
	if p.Favorite != nil {
		current.Favorite = *p.Favorite
	}
	if p.ListIDs != nil {
		current.ListIDs = lists
	}
	encoded, _ := json.Marshal(current.ListIDs)
	_, err = a.db.ExecContext(ctx, `INSERT INTO chat_organization(chat_id,archived,pinned,muted,unread_mark,favorite,list_ids,provenance) VALUES(?,?,?,?,?,?,?,'local_only') ON CONFLICT(chat_id) DO UPDATE SET archived=excluded.archived,pinned=excluded.pinned,muted=excluded.muted,unread_mark=excluded.unread_mark,favorite=excluded.favorite,list_ids=excluded.list_ids,provenance='local_only'`, p.ChatID, current.Archived, current.Pinned, current.Muted, current.UnreadMark, current.Favorite, string(encoded))
	if err != nil {
		return nil, err
	}
	current.OrganizationProvenance = "local_only"
	a.revision++
	return map[string]any{"chat": current, "provenance": "local_only"}, nil
}

func (a *App) fillChatOrganization(ctx context.Context, chats []Chat) error {
	for i := range chats {
		var raw string
		err := a.db.QueryRowContext(ctx, "SELECT archived,pinned,muted,unread_mark,favorite,list_ids,provenance FROM chat_organization WHERE chat_id=?", chats[i].ID).Scan(&chats[i].Archived, &chats[i].Pinned, &chats[i].Muted, &chats[i].UnreadMark, &chats[i].Favorite, &raw, &chats[i].OrganizationProvenance)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &chats[i].ListIDs); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) fillMessageState(ctx context.Context, chat string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]string, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	slots := make([]string, len(ids))
	args := []any{chat}
	for i, id := range ids {
		slots[i] = "?"
		args = append(args, id)
	}
	rows, err := a.db.QueryContext(ctx, "SELECT message_id,reply_to_id,edited,deleted,starred,pinned,forwarded,provenance FROM message_state WHERE chat_id=? AND message_id IN ("+strings.Join(slots, ",")+")", args...)
	if err != nil {
		return err
	}
	byID := map[string]int{}
	for i := range messages {
		byID[messages[i].ID] = i
	}
	for rows.Next() {
		var id, reply, provenance string
		var edited, deleted, starred, pinned, forwarded bool
		if err = rows.Scan(&id, &reply, &edited, &deleted, &starred, &pinned, &forwarded, &provenance); err != nil {
			rows.Close()
			return err
		}
		if i, ok := byID[id]; ok {
			messages[i].ReplyToID = reply
			messages[i].Edited = edited
			messages[i].Deleted = deleted
			messages[i].Starred = starred
			messages[i].Pinned = pinned
			messages[i].Forwarded = forwarded
			messages[i].MutationProvenance = provenance
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = a.db.QueryContext(ctx, "SELECT message_id,sender_id,emoji,timestamp,provenance FROM message_reactions WHERE chat_id=? AND message_id IN ("+strings.Join(slots, ",")+")", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var reaction Reaction
		if err = rows.Scan(&id, &reaction.SenderID, &reaction.Emoji, &reaction.Timestamp, &reaction.Provenance); err != nil {
			return err
		}
		if i, ok := byID[id]; ok {
			if messages[i].Reactions == nil {
				initial := []Reaction{}
				messages[i].Reactions = &initial
			}
			*messages[i].Reactions = append(*messages[i].Reactions, reaction)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		if messages[i].Reactions != nil {
			sort.Slice(*messages[i].Reactions, func(x, y int) bool {
				return (*messages[i].Reactions)[x].SenderID < (*messages[i].Reactions)[y].SenderID
			})
		}
	}
	return nil
}

func (a *App) ApplyIncomingReaction(ctx context.Context, chat, target, sender, emoji string, timestamp int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return ctx.Err()
		}
		return sql.ErrConnDone
	}
	if chat == "" || target == "" || sender == "" || utf8.RuneCountInString(emoji) > 16 {
		return fail("invalid_event", "Reaction event is invalid")
	}
	if _, err := a.loadTarget(ctx, chat, target); err != nil {
		return err
	}
	var err error
	if emoji == "" {
		_, err = a.db.ExecContext(ctx, "DELETE FROM message_reactions WHERE chat_id=? AND message_id=? AND sender_id=?", chat, target, sender)
	} else {
		_, err = a.db.ExecContext(ctx, `INSERT INTO message_reactions(chat_id,message_id,sender_id,emoji,timestamp,provenance) VALUES(?,?,?,?,?,'remote') ON CONFLICT(chat_id,message_id,sender_id) DO UPDATE SET emoji=excluded.emoji,timestamp=excluded.timestamp,provenance='remote'`, chat, target, sender, emoji, timestamp)
	}
	if err == nil {
		_, err = a.db.ExecContext(ctx, `UPDATE message_actions SET status='reconciled'
			WHERE method='message.react' AND chat_id=? AND target_id=? AND actor_id=?
			AND status IN ('pending','dispatched','unknown') AND dispatched_at_ms>0 AND ? > dispatched_at_ms`, chat, target, sender, timestamp)
	}
	if err == nil {
		a.revision++
	}
	return err
}

func (a *App) ApplyIncomingEdit(ctx context.Context, chat, id, body, actor string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return ctx.Err()
		}
		return sql.ErrConnDone
	}
	var senderID string
	var kind string
	if err := a.db.QueryRowContext(ctx, "SELECT sender_id,kind FROM messages WHERE chat_id=? AND id=?", chat, id).Scan(&senderID, &kind); err != nil {
		return err
	}
	if actor == "" || senderID == "" || actor != senderID || kind != "text" || utf8.RuneCountInString(body) > 16000 {
		return fail("invalid_event", "Edit event does not match the original message sender")
	}
	if _, err := a.db.ExecContext(ctx, "UPDATE messages SET body=? WHERE chat_id=? AND id=?", body, chat, id); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, `INSERT INTO message_state(chat_id,message_id,edited,provenance) VALUES(?,?,1,'remote') ON CONFLICT(chat_id,message_id) DO UPDATE SET edited=1,provenance='remote'`, chat, id)
	if err == nil {
		a.revision++
	}
	return err
}

func (a *App) ApplyIncomingRevoke(ctx context.Context, chat, id, actor string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return ctx.Err()
		}
		return sql.ErrConnDone
	}
	var senderID string
	if err := a.db.QueryRowContext(ctx, "SELECT sender_id FROM messages WHERE chat_id=? AND id=?", chat, id).Scan(&senderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fail("not_found", "Revoked message is not stored in this conversation")
		}
		return err
	}
	if actor == "" || senderID == "" || actor != senderID {
		return fail("invalid_event", "Revoke event does not match the original message sender")
	}
	if err := a.applyMessageDeletion(ctx, chat, id, "remote"); err != nil {
		return err
	}
	a.revision++
	return nil
}

func validPhoneRecipient(value string) bool {
	if len(value) < 9 || len(value) > 16 || value[0] != '+' {
		return false
	}
	for _, r := range value[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func selfPhoneRecipient(account Account, selfID string) string {
	phone := strings.TrimPrefix(account.Phone, "+")
	if !validPhoneRecipient("+" + phone) {
		return ""
	}
	parts := strings.SplitN(selfID, "@", 2)
	if len(parts) != 2 || (parts[1] != "s.whatsapp.net" && parts[1] != "c.us") || parts[0] != phone {
		return ""
	}
	return "+" + phone
}

func (a *App) contactList(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
		Cursor string `json:"cursor"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(p.Query) > 512 || len(p.Cursor) > 256 {
		return nil, fail("invalid_params", "Contact query or cursor is invalid")
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 100 {
		p.Limit = 100
	}
	if a.demo {
		return map[string]any{"contacts": []Contact{}, "has_more": false, "next_cursor": "", "provenance": "demo_empty"}, nil
	}
	transport := a.transport
	directory, ok := transport.(ContactDirectory)
	if !ok || a.state != "connected" {
		return nil, fail("unsupported", "Contact roster is unavailable in this transport state")
	}
	a.beginOperationLocked()
	defer a.endOperationLocked()
	a.mu.Unlock()
	contacts, err := directory.ListContacts(ctx)
	a.mu.Lock()
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	if a.transport != transport || a.state != "connected" {
		return nil, fail("disconnected", "Connection changed while contacts were loading")
	}
	if err != nil {
		return nil, fail("contact_list_failed", "Cached contacts could not be loaded")
	}
	sort.Slice(contacts, func(i, j int) bool { return contacts[i].JID < contacts[j].JID })
	filtered := make([]Contact, 0)
	query := strings.ToLower(strings.TrimSpace(p.Query))
	for _, c := range contacts {
		if c.JID <= p.Cursor {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(c.Name+" "+c.Phone+" "+c.JID), query) {
			continue
		}
		filtered = append(filtered, c)
	}
	more := len(filtered) > p.Limit
	if more {
		filtered = filtered[:p.Limit]
	}
	next := ""
	if more && len(filtered) > 0 {
		next = filtered[len(filtered)-1].JID
	}
	return map[string]any{"contacts": filtered, "has_more": more, "next_cursor": next}, nil
}

func (a *App) chatOpen(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Recipient string `json:"recipient"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if !validPhoneRecipient(p.Recipient) {
		return nil, fail("invalid_params", "Use an international phone number such as +14155550123")
	}
	if a.demo {
		return nil, fail("unsupported", "Demo mode cannot verify WhatsApp phone numbers")
	}
	transport := a.transport
	directory, ok := transport.(ContactDirectory)
	if !ok || a.state != "connected" {
		return nil, fail("disconnected", "Connect WhatsApp before verifying a new recipient")
	}
	accountProvider, ok := transport.(AccountProvider)
	if !ok {
		return nil, fail("account_unavailable", "The connected account could not be verified")
	}
	a.beginOperationLocked()
	defer a.endOperationLocked()
	a.mu.Unlock()
	account, accountErr := accountProvider.Account(ctx)
	a.mu.Lock()
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	if a.transport != transport || a.state != "connected" {
		return nil, fail("disconnected", "Connection changed during account verification")
	}
	if accountErr != nil || !account.Available || account.State == "disconnected" || account.State == "closing" {
		return nil, fail("account_unavailable", "The connected account could not be verified")
	}
	selfID, err := validSelfJID(account.JID)
	if err != nil {
		return nil, err
	}
	a.mu.Unlock()
	contact, verified, err := directory.ResolveContact(ctx, p.Recipient)
	a.mu.Lock()
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	if a.transport != transport || a.state != "connected" {
		return nil, fail("disconnected", "Connection changed during phone verification")
	}
	if err != nil {
		return nil, fail("contact_lookup_failed", "WhatsApp phone verification failed")
	}
	if !verified || !contact.Verified || contact.JID == "" {
		return nil, fail("not_registered", "This phone number is not registered with WhatsApp")
	}
	id, jidErr := validSelfJID(contact.JID)
	if jidErr != nil {
		return nil, fail("contact_invalid", "WhatsApp returned an unsupported contact identity")
	}
	isSelf := p.Recipient == selfPhoneRecipient(account, selfID)
	if isSelf {
		id = selfID
		if account.DisplayName != "" {
			contact.Name = account.DisplayName
		}
	}
	contact.JID = id
	contact.Phone = p.Recipient
	title := strings.TrimSpace(contact.Name)
	if isSelf {
		title = "(You)"
	} else if title == "" {
		title = p.Recipient
	}
	initials := string([]rune(title)[0:1])
	if _, err = a.db.ExecContext(ctx, "INSERT OR IGNORE INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,'#54656f',0)", id, title, initials); err != nil {
		return nil, err
	}
	a.revision++
	var c Chat
	err = a.db.QueryRowContext(ctx, "SELECT id,title,initials,color,is_group,COALESCE((SELECT substr(body,1,120) FROM messages WHERE chat_id=chats.id ORDER BY timestamp DESC,id DESC LIMIT 1),''),COALESCE((SELECT MAX(timestamp) FROM messages WHERE chat_id=chats.id),0) FROM chats WHERE id=?", id).Scan(&c.ID, &c.Title, &c.Initials, &c.Color, &c.IsGroup, &c.Preview, &c.Timestamp)
	if err != nil {
		return nil, err
	}
	c.OrganizationProvenance = "local_only"
	return map[string]any{"chat": c, "contact": contact, "verified": true}, nil
}

func sortedContactNames(contacts []Contact) []string {
	out := make([]string, len(contacts))
	for i, c := range contacts {
		out[i] = c.Name
	}
	sort.Strings(out)
	return out
}

var _ = fmt.Sprintf
var _ = sortedContactNames
