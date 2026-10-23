package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// PollResultsProjection contains only aggregate option labels and vote counts.
// It intentionally excludes poll keys, participant identity, and voter state.
type PollResultsProjection struct {
	Question string             `json:"question"`
	Options  []PollResultOption `json:"options"`
}

type PollResultOption struct {
	Name      string `json:"name"`
	VoteCount int64  `json:"vote_count"`
}

const (
	PollResultsProjectionMaxBytes       = 4 << 10
	PollVoteCountMax              int64 = 1<<53 - 1 // exact in the Qt/JavaScript JSON boundary
)

func PollResultsProjectionValid(results PollResultsProjection) bool {
	if !pollTextValid(results.Question, PollQuestionMaxRunes, true) || len(results.Options) == 0 || len(results.Options) > PollOptionMaxCount {
		return false
	}
	for _, option := range results.Options {
		if !pollTextValid(option.Name, PollOptionMaxRunes, true) || option.VoteCount < 0 || option.VoteCount > PollVoteCountMax {
			return false
		}
	}
	encoded, err := json.Marshal(results)
	return err == nil && len(encoded) <= PollResultsProjectionMaxBytes
}

func decodeStoredPollResultsProjection(encoded string) (PollResultsProjection, error) {
	var results PollResultsProjection
	if len(encoded) > PollResultsProjectionMaxBytes || !utf8.ValidString(encoded) {
		return results, fmt.Errorf("stored poll results projection is invalid")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&results); err != nil {
		return results, fmt.Errorf("stored poll results projection is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || !PollResultsProjectionValid(results) {
		return PollResultsProjection{}, fmt.Errorf("stored poll results projection is invalid")
	}
	return results, nil
}

func (a *App) fillPollResultsProjections(ctx context.Context, chat string, messages []Message) error {
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
	query := "SELECT message_id,projection FROM message_poll_results WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]PollResultsProjection, len(messages))
	for rows.Next() {
		var id, encoded string
		if err = rows.Scan(&id, &encoded); err != nil {
			return err
		}
		results, decodeErr := decodeStoredPollResultsProjection(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		byID[id] = results
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		if results, ok := byID[messages[i].ID]; ok {
			if messages[i].Kind != "poll_results" {
				return fmt.Errorf("stored poll results projection does not match message kind")
			}
			messages[i].PollResults = &results
		} else if messages[i].Kind == "poll_results" {
			return fmt.Errorf("stored poll results projection is missing")
		}
	}
	return nil
}
