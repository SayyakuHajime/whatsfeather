package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ButtonsProjection is the inert display-only subset of a legacy
// ButtonsMessage. It deliberately excludes button IDs, protocol types, native
// flow parameters, context and all other protocol metadata.
type ButtonsProjection struct {
	Header  string   `json:"header,omitempty"`
	Body    string   `json:"body,omitempty"`
	Footer  string   `json:"footer,omitempty"`
	Buttons []string `json:"buttons"`
}

// ButtonActionRow keeps a supported legacy response ID private to SQLite. It
// is never included in the hydrated RPC message projection.
type ButtonActionRow struct {
	ButtonIndex int
	ButtonID    string
}

const (
	ButtonsHeaderMaxRunes     = 256
	ButtonsBodyMaxRunes       = 1024
	ButtonsFooterMaxRunes     = 256
	ButtonsLabelMaxRunes      = 256
	ButtonsMaxCount           = 3
	ButtonsProjectionMaxBytes = 4 << 10
)

func buttonsTextValid(value string, maxRunes int, required bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes ||
		(required && strings.TrimSpace(value) == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ButtonsProjectionValid is shared by the protocol adapter, persistence and
// hydration boundaries so malformed content always fails closed atomically.
func ButtonsProjectionValid(buttons ButtonsProjection) bool {
	if !buttonsTextValid(buttons.Header, ButtonsHeaderMaxRunes, false) ||
		!buttonsTextValid(buttons.Body, ButtonsBodyMaxRunes, false) ||
		!buttonsTextValid(buttons.Footer, ButtonsFooterMaxRunes, false) ||
		(strings.TrimSpace(buttons.Header) == "" && strings.TrimSpace(buttons.Body) == "") ||
		len(buttons.Buttons) == 0 || len(buttons.Buttons) > ButtonsMaxCount {
		return false
	}
	for _, label := range buttons.Buttons {
		if !buttonsTextValid(label, ButtonsLabelMaxRunes, true) {
			return false
		}
	}
	encoded, err := json.Marshal(buttons)
	return err == nil && len(encoded) <= ButtonsProjectionMaxBytes
}

func ButtonActionRowsValid(buttons ButtonsProjection, rows []ButtonActionRow) bool {
	if !ButtonsProjectionValid(buttons) || len(rows) != len(buttons.Buttons) {
		return false
	}
	var seenIndex [ButtonsMaxCount]bool
	seenID := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.ButtonIndex < 0 || row.ButtonIndex >= len(buttons.Buttons) || seenIndex[row.ButtonIndex] ||
			len(row.ButtonID) == 0 || len(row.ButtonID) > 256 || !buttonsTextValid(row.ButtonID, 256, true) {
			return false
		}
		if _, exists := seenID[row.ButtonID]; exists {
			return false
		}
		seenIndex[row.ButtonIndex] = true
		seenID[row.ButtonID] = struct{}{}
	}
	return true
}

func decodeStoredButtonsProjection(encoded string) (ButtonsProjection, error) {
	var buttons ButtonsProjection
	if len(encoded) > ButtonsProjectionMaxBytes || !utf8.ValidString(encoded) {
		return buttons, fmt.Errorf("stored ButtonsMessage projection is invalid")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&buttons); err != nil {
		return ButtonsProjection{}, fmt.Errorf("stored ButtonsMessage projection is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || !ButtonsProjectionValid(buttons) {
		return ButtonsProjection{}, fmt.Errorf("stored ButtonsMessage projection is invalid")
	}
	return buttons, nil
}

func (a *App) fillButtonsProjections(ctx context.Context, chat string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	placeholders := make([]string, len(messages))
	args := make([]any, 0, len(messages)+1)
	args = append(args, chat)
	for i, message := range messages {
		placeholders[i] = "?"
		args = append(args, message.ID)
	}
	query := "SELECT message_id,projection FROM message_buttons WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]ButtonsProjection, len(messages))
	for rows.Next() {
		var id, encoded string
		if err = rows.Scan(&id, &encoded); err != nil {
			return err
		}
		buttons, decodeErr := decodeStoredButtonsProjection(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		byID[id] = buttons
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	actionQuery := "SELECT message_id,count(*) FROM message_button_actions WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ") GROUP BY message_id"
	actionRows, err := a.db.QueryContext(ctx, actionQuery, args...)
	if err != nil {
		return err
	}
	defer actionRows.Close()
	actionCounts := make(map[string]int, len(messages))
	for actionRows.Next() {
		var id string
		var count int
		if err = actionRows.Scan(&id, &count); err != nil {
			return err
		}
		actionCounts[id] = count
	}
	if err = actionRows.Err(); err != nil {
		return err
	}
	if err = actionRows.Close(); err != nil {
		return err
	}
	stateQuery := "SELECT target_id,status FROM message_actions WHERE chat_id=? AND method='button.reply' AND status IN ('reserved','dispatched','pending','unknown','accepted','demo_local') AND target_id IN (" + strings.Join(placeholders, ",") + ") ORDER BY rowid DESC"
	stateRows, err := a.db.QueryContext(ctx, stateQuery, args...)
	if err != nil {
		return err
	}
	defer stateRows.Close()
	replyStates := make(map[string]string, len(messages))
	for stateRows.Next() {
		var id, status string
		if err = stateRows.Scan(&id, &status); err != nil {
			return err
		}
		if _, exists := replyStates[id]; exists {
			continue
		}
		switch status {
		case "accepted":
			replyStates[id] = "accepted"
		case "demo_local":
			replyStates[id] = "local"
		case "unknown":
			replyStates[id] = "unknown"
		default:
			replyStates[id] = "pending"
		}
	}
	if err = stateRows.Err(); err != nil {
		return err
	}
	for i := range messages {
		buttons, ok := byID[messages[i].ID]
		if messages[i].Kind == "buttons" && !ok {
			return fmt.Errorf("stored ButtonsMessage projection is missing")
		}
		if ok {
			if messages[i].Kind != "buttons" {
				return fmt.Errorf("stored ButtonsMessage projection does not match message kind")
			}
			messages[i].Buttons = &buttons
			messages[i].ButtonReplyState = replyStates[messages[i].ID]
			messages[i].ButtonReplyAvailable = !messages[i].Outgoing && messages[i].ButtonReplyState == "" && actionCounts[messages[i].ID] == len(buttons.Buttons)
		}
	}
	return nil
}
