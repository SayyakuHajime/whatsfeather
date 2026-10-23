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

// PollProjection is the normalized, read-only subset of a poll creation shown
// in history. It deliberately excludes protocol identifiers and vote data.
type PollProjection struct {
	Question               string   `json:"question"`
	Options                []string `json:"options"`
	SelectableOptionsCount *uint32  `json:"selectable_options_count,omitempty"`
}

const (
	PollQuestionMaxRunes   = 256
	PollOptionMaxRunes     = 100
	PollOptionMaxCount     = 12
	PollProjectionMaxBytes = 4 << 10
)

func pollTextValid(value string, maxRunes int, required bool) bool {
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

// PollProjectionValid checks the same limits at protocol-input and SQLite
// boundaries, including the serialized projection size consumed by message.list.
func PollProjectionValid(poll PollProjection) bool {
	if !pollTextValid(poll.Question, PollQuestionMaxRunes, true) || len(poll.Options) == 0 || len(poll.Options) > PollOptionMaxCount {
		return false
	}
	for _, option := range poll.Options {
		if !pollTextValid(option, PollOptionMaxRunes, true) {
			return false
		}
	}
	if poll.SelectableOptionsCount != nil && (*poll.SelectableOptionsCount == 0 || *poll.SelectableOptionsCount > uint32(len(poll.Options))) {
		return false
	}
	encoded, err := json.Marshal(poll)
	return err == nil && len(encoded) <= PollProjectionMaxBytes
}

func decodeStoredPollProjection(encoded string) (PollProjection, error) {
	var poll PollProjection
	if len(encoded) > PollProjectionMaxBytes || !utf8.ValidString(encoded) {
		return poll, fmt.Errorf("stored poll projection is invalid")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&poll); err != nil {
		return poll, fmt.Errorf("stored poll projection is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || !PollProjectionValid(poll) {
		return PollProjection{}, fmt.Errorf("stored poll projection is invalid")
	}
	return poll, nil
}

func (a *App) fillPollProjections(ctx context.Context, chat string, messages []Message) error {
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
	query := "SELECT message_id,projection FROM message_polls WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]PollProjection, len(messages))
	for rows.Next() {
		var id, encoded string
		if err = rows.Scan(&id, &encoded); err != nil {
			return err
		}
		poll, decodeErr := decodeStoredPollProjection(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		byID[id] = poll
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		if poll, ok := byID[messages[i].ID]; ok {
			if messages[i].Kind != "poll" {
				return fmt.Errorf("stored poll projection does not match message kind")
			}
			messages[i].Poll = &poll
		}
	}
	return nil
}
