package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPollVoteStandardEnvelope(t *testing.T) {
	a, p, _ := votingFixture(t)
	if err := a.ApplyPollVote(context.Background(), "chat", "poll", "private-voter", "event", 1, []int{1}); err != nil {
		t.Fatal(err)
	}
	for _, indices := range []string{"[1]", "[]"} {
		raw := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":` + indices + `,"client_id":"vote-` + indices + `"}`)
		for replay := 0; replay < 2; replay++ {
			result, err := a.Handle(context.Background(), "poll.vote", raw)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(result)
			var envelope struct {
				Message Message `json:"message"`
			}
			if err = json.Unmarshal(data, &envelope); err != nil {
				t.Fatal(err)
			}
			m := envelope.Message
			if m.ID != "poll" || m.ChatID != "chat" || m.Kind != "poll" || m.Poll == nil || m.PollResults == nil || m.PollResults.Options[1].VoteCount != 1 {
				t.Fatalf("missing normalized original poll envelope: %s", data)
			}
		}
	}
	if p.calls != 2 {
		t.Fatalf("sent %d times", p.calls)
	}
}

func TestPollVoteSelfHydration(t *testing.T) {
	a, p, dir := votingFixture(t)
	ctx := context.Background()
	check := func(result any, state, indices string) {
		t.Helper()
		data, _ := json.Marshal(result)
		var page struct {
			Messages []map[string]json.RawMessage `json:"messages"`
			Message  map[string]json.RawMessage   `json:"message"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		m := page.Message
		if len(page.Messages) > 0 {
			m = page.Messages[0]
		}
		vote := m["poll_self_vote"]
		if state == "" {
			if len(vote) != 0 {
				t.Fatalf("no vote must omit projection: %s", data)
			}
			return
		}
		var got struct {
			State   string          `json:"state"`
			Indices json.RawMessage `json:"option_indices"`
		}
		if err := json.Unmarshal(vote, &got); err != nil {
			t.Fatalf("missing self vote: %s (%v)", data, err)
		}
		if got.State != state || string(got.Indices) != indices {
			t.Fatalf("wrong self vote: %s", data)
		}
		var fields map[string]any
		json.Unmarshal(vote, &fields)
		if len(fields) != 2 {
			t.Fatalf("extra self vote fields: %s", vote)
		}
		for _, private := range []string{"private-client", "private-voter", "client_id", "voter", "poll_key", "option_hash", "selected_options"} {
			if strings.Contains(string(data), private) {
				t.Fatalf("private data exposed: %s", data)
			}
		}
	}
	list := func(state, indices string) {
		t.Helper()
		result, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"chat","limit":10}`))
		if err != nil {
			t.Fatal(err)
		}
		check(result, state, indices)
	}
	list("", "")
	if err := a.ApplyPollVote(ctx, "chat", "poll", "private-voter", "private-event", 1, []int{0}); err != nil {
		t.Fatal(err)
	}
	for i, selection := range []string{"[1]", "[]", "[0]"} {
		state := "accepted"
		if i == 2 {
			p.fail = true
			state = "unknown"
		}
		raw := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":` + selection + `,"client_id":"private-client-` + selection + `"}`)
		result, err := a.Handle(ctx, "poll.vote", raw)
		if i < 2 {
			if err != nil {
				t.Fatal(err)
			}
			check(result, state, selection)
		} else if err == nil {
			t.Fatal("unknown accepted")
		}
		list(state, selection)
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		a, err = Open(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		p.app = a
		a.SetTransport(p)
		a.state = "connected"
		list(state, selection)
	}
	// Replaying an older accepted action cannot replace the current unknown vote.
	_, err := a.Handle(ctx, "poll.vote", json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[1],"client_id":"private-client-[1]"}`))
	if err != nil {
		t.Fatal(err)
	}
	list("unknown", "[0]")
}

func TestPollVotePublicUnknownOutcome(t *testing.T) {
	a, p, dir := votingFixture(t)
	p.fail = true
	raw := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[],"client_id":"uncertain"}`)
	for attempt := 0; attempt < 3; attempt++ {
		result, err := a.Handle(context.Background(), "poll.vote", raw)
		var apiErr *Error
		if result != nil || !errors.As(err, &apiErr) || apiErr.Code != "unknown_outcome" {
			t.Fatalf("result=%v err=%v; want unknown_outcome", result, err)
		}
		if attempt == 1 {
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			a, err = Open(dir, false)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			p.app = a
			a.SetTransport(p)
			a.state = "connected"
		}
	}
	if p.calls != 1 {
		t.Fatalf("unknown send repeated: %d", p.calls)
	}
}

func TestPollVoteDemoUnavailable(t *testing.T) {
	a, p, _ := votingFixture(t)
	a.demo = true
	result, err := a.Handle(context.Background(), "poll.vote", json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[],"client_id":"demo"}`))
	var apiErr *Error
	if result != nil || !errors.As(err, &apiErr) || apiErr.Code != "unsupported" || p.calls != 0 {
		t.Fatalf("demo result=%v err=%v calls=%d", result, err, p.calls)
	}
}

func TestPollVoteSelfDispatchOrderAndBounds(t *testing.T) {
	a, _, dir := votingFixture(t)
	ctx := context.Background()
	// Reserve in one order, dispatch in the other, then finish out of order.
	for _, id := range []string{"first-reserved", "second-reserved"} {
		if _, err := a.reserveAction(ctx, id, "poll.vote", "chat", "poll", "[1]"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"second-reserved", "first-reserved"} {
		if err := a.dispatchAction(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.finishAction(ctx, "second-reserved", "accepted"); err != nil {
		t.Fatal(err)
	}
	// An in-flight latest attempt must be unknown even before reopen.
	check := func(state string) {
		t.Helper()
		result, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"chat"}`))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(result)
		var page struct {
			Messages []struct {
				Vote *struct {
					State string `json:"state"`
				} `json:"poll_self_vote"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) != 1 || page.Messages[0].Vote == nil || page.Messages[0].Vote.State != state {
			t.Fatalf("wrong latest dispatch: %s", data)
		}
	}
	check("unknown")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	check("unknown")
	// Proven rejection cannot replace the prior accepted outcome.
	if _, err := a.db.Exec("UPDATE message_actions SET status='rejected' WHERE client_id='first-reserved'"); err != nil {
		t.Fatal(err)
	}
	check("accepted")
	for _, payload := range []string{"null", "[2]", "[0,1]", "[1,1]", strings.Repeat(" ", 65) + "[]"} {
		if _, err := a.db.Exec("UPDATE message_actions SET payload=? WHERE client_id='second-reserved'", payload); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"chat"}`)); err == nil {
			t.Fatalf("corrupt self vote hydrated: %q", payload)
		}
	}
}

func TestPollVoteExistingOutcomesHydrateOnReopen(t *testing.T) {
	a, _, dir := votingFixture(t)
	// Model a profile written before dispatch-order tracking existed.
	if _, err := a.db.Exec(`INSERT INTO message_actions(client_id,chat_id,target_id,method,payload,status)
		VALUES('old-accepted','chat','poll','poll.vote','[]','accepted'),
		('old-unknown','chat','poll','poll.vote','[1]','unknown')`); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "message.list", json.RawMessage(`{"chat_id":"chat"}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), `"poll_self_vote":{"state":"unknown","option_indices":[1]}`) {
		t.Fatalf("existing unknown outcome not hydrated: %s", data)
	}
}
