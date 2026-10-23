package app

import (
	"context"
	"encoding/json"
)

// pollVoteMessage keeps the durable send path and returns the original poll.
func (a *App) pollVoteMessage(ctx context.Context, raw json.RawMessage) (any, error) {
	if _, err := a.pollVote(ctx, raw); err != nil {
		return nil, err
	}
	var p struct {
		ChatID   string `json:"chat_id"`
		TargetID string `json:"target_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.TargetID)
	if err != nil {
		return nil, fail("unknown_outcome", "Vote was accepted; reload the poll")
	}
	messages := []Message{target}
	if err = a.fillPollProjections(ctx, p.ChatID, messages); err == nil {
		err = a.fillVotingResults(ctx, p.ChatID, messages)
	}
	if err == nil {
		err = a.fillPollSelfVotes(ctx, p.ChatID, messages)
	}
	if err != nil {
		return nil, fail("unknown_outcome", "Vote was accepted; reload the poll")
	}
	return map[string]any{"message": messages[0]}, nil
}
