package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// PollSelfVoteProjection describes this profile's latest dispatched vote whose
// outcome may affect the remote poll. It does not claim a remote tally or receipt.
type PollSelfVoteProjection struct {
	State         string `json:"state"`
	OptionIndices []int  `json:"option_indices"`
}

func (a *App) fillPollSelfVotes(ctx context.Context, chat string, messages []Message) error {
	for i := range messages {
		m := &messages[i]
		m.PollSelfVote = nil
		if m.Kind != "poll" || m.Deleted || m.Poll == nil {
			continue
		}
		var state, payload string
		err := a.db.QueryRowContext(ctx, `SELECT a.status,a.payload FROM poll_vote_dispatches d
			JOIN message_actions a ON a.client_id=d.client_id
			WHERE d.chat_id=? AND d.message_id=? AND a.method='poll.vote'
			AND a.status IN ('accepted','unknown','pending','dispatched')
			ORDER BY d.sequence DESC LIMIT 1`, chat, m.ID).Scan(&state, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var indices []int
		if len(payload) > 64 {
			return fmt.Errorf("unbounded self vote")
		}
		if err = json.Unmarshal([]byte(payload), &indices); err != nil {
			return err
		}
		if _, err = pollSelection(*m.Poll, indices); err != nil {
			return err
		}
		if !sort.IntsAreSorted(indices) {
			return fmt.Errorf("noncanonical self vote")
		}
		if state != "accepted" {
			state = "unknown"
		}
		m.PollSelfVote = &PollSelfVoteProjection{State: state, OptionIndices: indices}
	}
	return nil
}
