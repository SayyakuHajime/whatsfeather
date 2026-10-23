package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// PollVoteSender receives only a validated, locally stored original poll.
type PollVoteSender interface {
	SendPollVote(context.Context, Message, []string, string) (int64, error)
}

func pollSelection(p PollProjection, indices []int) ([]string, error) {
	if !PollProjectionValid(p) || indices == nil || len(indices) > len(p.Options) {
		return nil, fmt.Errorf("invalid poll selection")
	}
	seen := map[int]bool{}
	names := map[string]bool{}
	for _, name := range p.Options {
		if names[name] {
			return nil, fmt.Errorf("ambiguous poll options")
		}
		names[name] = true
	}
	if p.SelectableOptionsCount != nil && len(indices) > int(*p.SelectableOptionsCount) {
		return nil, fmt.Errorf("too many selections")
	}
	labels := make([]string, 0, len(indices))
	for _, i := range indices {
		if i < 0 || i >= len(p.Options) || seen[i] {
			return nil, fmt.Errorf("invalid option index")
		}
		seen[i] = true
		labels = append(labels, p.Options[i])
	}
	return labels, nil
}

func (a *App) rejectUnresolvedPollVote(ctx context.Context, chat, target, clientID string) error {
	// Existing IDs go through reserveAction below, which verifies the complete
	// action tuple and never resends accepted or ambiguous outcomes. The target
	// guard is for a new ID that would otherwise evade that idempotency record.
	var existing int
	err := a.db.QueryRowContext(ctx, "SELECT 1 FROM message_actions WHERE client_id=?", clientID).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var unresolved int
	err = a.db.QueryRowContext(ctx, `SELECT 1
		FROM poll_vote_dispatches AS dispatch
		JOIN message_actions AS action ON action.client_id=dispatch.client_id
		WHERE dispatch.chat_id=? AND dispatch.message_id=? AND action.method='poll.vote'
		AND action.status IN ('unknown','pending','dispatched') LIMIT 1`, chat, target).Scan(&unresolved)
	if err == nil {
		return fail("unknown_outcome", "A previous vote on this poll has an unknown outcome; reconcile it before voting again")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (a *App) pollVote(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID   string `json:"chat_id"`
		TargetID string `json:"target_id"`
		Indices  []int  `json:"option_indices"`
		ClientID string `json:"client_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if !actionID(p.ClientID) {
		return nil, fail("invalid_params", "Client ID is invalid")
	}
	target, err := a.loadTarget(ctx, p.ChatID, p.TargetID)
	if err != nil {
		return nil, err
	}
	if target.Kind != "poll" || target.Deleted {
		return nil, fail("invalid_target", "An original undeleted poll is required")
	}
	var encoded string
	if err = a.db.QueryRowContext(ctx, "SELECT projection FROM message_polls WHERE chat_id=? AND message_id=?", p.ChatID, p.TargetID).Scan(&encoded); err != nil {
		return nil, err
	}
	poll, err := decodeStoredPollProjection(encoded)
	if err != nil {
		return nil, err
	}
	sort.Ints(p.Indices)
	labels, err := pollSelection(poll, p.Indices)
	if err != nil {
		return nil, fail("invalid_params", err.Error())
	}
	sender, ok := a.transport.(PollVoteSender)
	if a.demo || !ok {
		return nil, fail("unsupported", "Poll voting requires a poll transport")
	}
	if a.state != "connected" {
		return nil, fail("disconnected", "Connect before voting")
	}
	if err = a.rejectUnresolvedPollVote(ctx, p.ChatID, p.TargetID, p.ClientID); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(p.Indices)
	done, err := a.reserveAction(ctx, p.ClientID, "poll.vote", p.ChatID, p.TargetID, string(payload))
	if err != nil {
		return nil, err
	}
	if !done {
		if _, err = a.runAction(ctx, nil, p.ClientID, func() (int64, error) { return sender.SendPollVote(ctx, target, labels, p.ClientID) }); err != nil {
			return nil, err
		}
	}
	return map[string]any{"chat_id": p.ChatID, "target_id": p.TargetID, "status": "accepted"}, nil
}

// ApplyPollVote stores the latest decoded choice privately. Transport callers must
// authenticate/decrypt the event with the SDK before calling this method.
func (a *App) ApplyPollVote(ctx context.Context, chat, target, voter, event string, revision int64, indices []int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx == nil || a.closed {
		return fmt.Errorf("poll store unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !actionID(voter) || !actionID(event) || revision <= 0 {
		return fmt.Errorf("invalid vote metadata")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var encoded string
	err = tx.QueryRowContext(ctx, `SELECT p.projection FROM message_polls p JOIN messages m ON m.chat_id=p.chat_id AND m.id=p.message_id LEFT JOIN message_state s ON s.chat_id=m.chat_id AND s.message_id=m.id WHERE p.chat_id=? AND p.message_id=? AND m.kind='poll' AND COALESCE(s.deleted,0)=0`, chat, target).Scan(&encoded)
	if err != nil {
		return err
	}
	poll, err := decodeStoredPollProjection(encoded)
	if err != nil {
		return err
	}
	if _, err = pollSelection(poll, indices); err != nil {
		return err
	}
	canonical := append([]int{}, indices...)
	sort.Ints(canonical)
	choices, _ := json.Marshal(canonical)
	result, err := tx.ExecContext(ctx, `INSERT INTO poll_latest_votes(chat_id,message_id,voter,event_id,revision,choices) VALUES(?,?,?,?,?,?) ON CONFLICT(chat_id,message_id,voter) DO UPDATE SET event_id=excluded.event_id,revision=excluded.revision,choices=excluded.choices WHERE excluded.revision>poll_latest_votes.revision AND excluded.event_id<>poll_latest_votes.event_id`, chat, target, voter, event, revision, string(choices))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("duplicate or stale poll vote")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.revision++
	return nil
}

func (a *App) fillVotingResults(ctx context.Context, chat string, messages []Message) error {
	// Preserve the existing standalone results decoder and its corruption checks.
	compat := append([]Message(nil), messages...)
	if err := a.fillPollResultsProjections(ctx, chat, compat); err != nil {
		return err
	}
	for i := range messages {
		messages[i].PollResults = compat[i].PollResults
		if messages[i].Kind != "poll" {
			continue
		}
		if messages[i].Poll == nil {
			continue
		}
		poll := *messages[i].Poll
		r := PollResultsProjection{Question: poll.Question, Options: make([]PollResultOption, len(poll.Options))}
		for j, name := range poll.Options {
			r.Options[j].Name = name
		}
		// Validate stored choices before counting. Only bounded aggregates cross RPC.
		rows, err := a.db.QueryContext(ctx, `SELECT choices,COUNT(*) FROM poll_latest_votes WHERE chat_id=? AND message_id=? GROUP BY choices LIMIT 4097`, chat, messages[i].ID)
		if err != nil {
			return err
		}
		groups := 0
		for rows.Next() {
			groups++
			if groups > 1<<PollOptionMaxCount {
				err = fmt.Errorf("unbounded stored vote groups")
				break
			}
			var choices string
			var count int64
			if err = rows.Scan(&choices, &count); err != nil {
				break
			}
			var indices []int
			if len(choices) > 64 {
				err = fmt.Errorf("unbounded stored vote")
				break
			}
			if err = json.Unmarshal([]byte(choices), &indices); err != nil {
				break
			}
			if _, err = pollSelection(poll, indices); err != nil {
				break
			}
			if !sort.IntsAreSorted(indices) {
				err = fmt.Errorf("noncanonical stored vote")
				break
			}
			for _, j := range indices {
				if count > PollVoteCountMax-r.Options[j].VoteCount {
					err = fmt.Errorf("unbounded poll aggregate")
					break
				}
				r.Options[j].VoteCount += count
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if !PollResultsProjectionValid(r) {
			return fmt.Errorf("invalid aggregate")
		}
		messages[i].PollResults = &r
	}
	return a.fillPollVoteStates(ctx, chat, messages)
}

func (a *App) fillPollVoteStates(ctx context.Context, chat string, messages []Message) error {
	for i := range messages {
		message := &messages[i]
		message.PollVoteState = ""
		if message.Kind != "poll" || message.Deleted || message.Poll == nil {
			continue
		}
		var state string
		err := a.db.QueryRowContext(ctx, `SELECT CASE
			WHEN MAX(CASE WHEN action.status IN ('pending','dispatched') THEN 1 ELSE 0 END)=1 THEN 'pending'
			WHEN MAX(CASE WHEN action.status='unknown' THEN 1 ELSE 0 END)=1 THEN 'unknown'
			ELSE '' END
			FROM poll_vote_dispatches AS dispatch
			JOIN message_actions AS action ON action.client_id=dispatch.client_id
			WHERE dispatch.chat_id=? AND dispatch.message_id=? AND action.method='poll.vote'`, chat, message.ID).Scan(&state)
		if err != nil {
			return err
		}
		message.PollVoteState = state
	}
	return nil
}

// PollVoteTarget resolves stored metadata for the adapter without an RPC route.
func (a *App) PollVoteTarget(ctx context.Context, chat, id string) (Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	target, err := a.loadTarget(ctx, chat, id)
	if err != nil {
		return Message{}, err
	}
	if target.Kind != "poll" || target.Deleted {
		return Message{}, fmt.Errorf("invalid original poll")
	}
	rows := []Message{target}
	if err = a.fillPollProjections(ctx, chat, rows); err != nil {
		return Message{}, err
	}
	if rows[0].Poll == nil {
		return Message{}, fmt.Errorf("missing original poll")
	}
	return rows[0], nil
}
