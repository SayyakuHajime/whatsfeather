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

// ListProjection is the inert display-only subset of an incoming WhatsApp list.
// It intentionally has no row IDs, action targets, URLs, or protocol metadata.
type ListProjection struct {
	Title       string        `json:"title"`
	Description string        `json:"description,omitempty"`
	Footer      string        `json:"footer,omitempty"`
	ButtonLabel string        `json:"button_label"`
	Sections    []ListSection `json:"sections"`
}

type ListSection struct {
	Title string    `json:"title,omitempty"`
	Rows  []ListRow `json:"rows"`
}

type ListRow struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

type ListActionRow struct {
	SectionIndex int
	RowIndex     int
	RowID        string
}

const (
	ListTitleMaxRunes       = 256
	ListDescriptionMaxRunes = 512
	ListFooterMaxRunes      = 256
	ListButtonMaxRunes      = 128
	ListSectionMaxRunes     = 128
	ListRowTitleMaxRunes    = 256
	ListRowDescriptionMax   = 512
	ListSectionMaxCount     = 10
	ListRowsPerSectionMax   = 10
	ListRowMaxCount         = 50
	ListProjectionMaxBytes  = 4 << 10
)

func listTextValid(value string, maxRunes int, required bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes || (required && strings.TrimSpace(value) == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ListProjectionValid is shared by the protocol adapter, persistence and
// hydration boundaries so malformed content always fails closed atomically.
func ListProjectionValid(list ListProjection) bool {
	if !listTextValid(list.Title, ListTitleMaxRunes, true) ||
		!listTextValid(list.Description, ListDescriptionMaxRunes, false) ||
		!listTextValid(list.Footer, ListFooterMaxRunes, false) ||
		!listTextValid(list.ButtonLabel, ListButtonMaxRunes, true) ||
		len(list.Sections) == 0 || len(list.Sections) > ListSectionMaxCount {
		return false
	}
	totalRows := 0
	for _, section := range list.Sections {
		if !listTextValid(section.Title, ListSectionMaxRunes, false) || len(section.Rows) == 0 || len(section.Rows) > ListRowsPerSectionMax {
			return false
		}
		totalRows += len(section.Rows)
		if totalRows > ListRowMaxCount {
			return false
		}
		for _, row := range section.Rows {
			if !listTextValid(row.Title, ListRowTitleMaxRunes, true) || !listTextValid(row.Description, ListRowDescriptionMax, false) {
				return false
			}
		}
	}
	encoded, err := json.Marshal(list)
	return err == nil && len(encoded) <= ListProjectionMaxBytes
}

func ListActionRowsValid(list ListProjection, actions []ListActionRow) bool {
	if !ListProjectionValid(list) || len(actions) == 0 {
		return false
	}
	totalRows := 0
	for _, section := range list.Sections {
		totalRows += len(section.Rows)
	}
	if len(actions) != totalRows {
		return false
	}
	seen := make(map[string]struct{}, len(actions))
	index := 0
	for sectionIndex, section := range list.Sections {
		for rowIndex := range section.Rows {
			action := actions[index]
			if action.SectionIndex != sectionIndex || action.RowIndex != rowIndex || len(action.RowID) > 256 || !listTextValid(action.RowID, 256, true) {
				return false
			}
			if _, exists := seen[action.RowID]; exists {
				return false
			}
			seen[action.RowID] = struct{}{}
			index++
		}
	}
	return true
}

func decodeStoredListProjection(encoded string) (ListProjection, error) {
	var list ListProjection
	if len(encoded) > ListProjectionMaxBytes || !utf8.ValidString(encoded) {
		return list, fmt.Errorf("stored list projection is invalid")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&list); err != nil {
		return ListProjection{}, fmt.Errorf("stored list projection is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || !ListProjectionValid(list) {
		return ListProjection{}, fmt.Errorf("stored list projection is invalid")
	}
	return list, nil
}

func (a *App) fillListProjections(ctx context.Context, chat string, messages []Message) error {
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
	query := "SELECT message_id,projection FROM message_lists WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]ListProjection, len(messages))
	for rows.Next() {
		var id, encoded string
		if err = rows.Scan(&id, &encoded); err != nil {
			return err
		}
		list, decodeErr := decodeStoredListProjection(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		byID[id] = list
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	actionQuery := "SELECT message_id,count(*) FROM message_list_actions WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ") GROUP BY message_id"
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
	stateQuery := "SELECT target_id,status FROM message_actions WHERE chat_id=? AND method='list.reply' AND status IN ('reserved','dispatched','pending','unknown','accepted') AND target_id IN (" + strings.Join(placeholders, ",") + ") ORDER BY rowid DESC"
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
		if _, exists := replyStates[id]; !exists {
			switch status {
			case "accepted":
				replyStates[id] = "accepted"
			case "unknown":
				replyStates[id] = "unknown"
			default:
				replyStates[id] = "pending"
			}
		}
	}
	if err = stateRows.Err(); err != nil {
		return err
	}
	for i := range messages {
		list, ok := byID[messages[i].ID]
		if messages[i].Kind == "list" && !ok {
			return fmt.Errorf("stored list projection is missing")
		}
		if ok {
			if messages[i].Kind != "list" {
				return fmt.Errorf("stored list projection does not match message kind")
			}
			messages[i].List = &list
			totalRows := 0
			for _, section := range list.Sections {
				totalRows += len(section.Rows)
			}
			messages[i].ListReplyState = replyStates[messages[i].ID]
			messages[i].ListReplyAvailable = !messages[i].Outgoing && messages[i].ListReplyState == "" && actionCounts[messages[i].ID] == totalRows
		}
	}
	return nil
}
