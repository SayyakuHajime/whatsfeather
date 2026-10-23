package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type pollSendProbe struct {
	actionProbe
	calls  int
	labels []string
	app    *App
	status string
}

func (p *pollSendProbe) SendPollVote(ctx context.Context, target Message, labels []string, id string) (int64, error) {
	p.calls++
	p.labels = labels
	if err := p.app.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", id).Scan(&p.status); err != nil {
		return 0, err
	}
	if p.status != "dispatched" {
		return 0, errors.New("not dispatched")
	}
	if p.fail {
		return 0, errors.New("ambiguous")
	}
	return 1234, nil
}
func votingFixture(t *testing.T) (*App, *pollSendProbe, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	max := uint32(1)
	if err = a.Record(context.Background(), Message{ID: "poll", ChatID: "chat", Kind: "poll", Body: "Pick?", SenderID: "15550000001@s.whatsapp.net", Status: "received", Poll: &PollProjection{Question: "Pick?", Options: []string{"A", "B"}, SelectableOptionsCount: &max}}, "Synthetic"); err != nil {
		t.Fatal(err)
	}
	p := &pollSendProbe{app: a}
	a.SetTransport(p)
	a.state = "connected"
	return a, p, dir
}
func TestPollVotingAction(t *testing.T) {
	a, p, _ := votingFixture(t)
	ctx := context.Background()
	for _, indices := range []string{"[2]", "[-1]", "[0,0]", "[0,1]", "null"} {
		_, err := a.Handle(ctx, "poll.vote", json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":`+indices+`,"client_id":"bad"}`))
		if err == nil {
			t.Fatalf("accepted %s", indices)
		}
	}
	raw := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[1],"client_id":"vote-1"}`)
	if _, err := a.Handle(ctx, "poll.vote", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(ctx, "poll.vote", raw); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || len(p.labels) != 1 || p.labels[0] != "B" || p.status != "dispatched" {
		t.Fatalf("calls=%d labels=%v status=%s", p.calls, p.labels, p.status)
	}
	p.fail = true
	raw = json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[],"client_id":"vote-2"}`)
	for i := 0; i < 2; i++ {
		if _, err := a.Handle(ctx, "poll.vote", raw); err == nil {
			t.Fatal("unknown accepted")
		}
	}
	if p.calls != 2 {
		t.Fatalf("unknown resent: %d", p.calls)
	}
	var status string
	if err := a.db.QueryRow("SELECT status FROM message_actions WHERE client_id='vote-2'").Scan(&status); err != nil || status != "unknown" {
		t.Fatalf("%s %v", status, err)
	}
}

type pollUpdater interface {
	ApplyPollVote(context.Context, string, string, string, string, int64, []int) error
}

func TestPollVotingUpdates(t *testing.T) {
	a, _, dir := votingFixture(t)
	ctx := context.Background()
	u, ok := any(a).(pollUpdater)
	if !ok {
		t.Fatal("private poll update path missing")
	}
	apply := func(voter, event string, rev int64, indices []int) error {
		return u.ApplyPollVote(ctx, "chat", "poll", voter, event, rev, indices)
	}
	if err := apply("private-voter-1", "event-1", 10, []int{0}); err != nil {
		t.Fatal(err)
	}
	if err := apply("private-voter-2", "event-2", 11, []int{1}); err != nil {
		t.Fatal(err)
	}
	if err := apply("private-voter-1", "event-3", 12, []int{1}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		event   string
		rev     int64
		indices []int
	}{{"event-3", 12, []int{0}}, {"stale", 9, []int{0}}, {"bad", 13, []int{2}}, {"repeat", 13, []int{1, 1}}, {"excess", 13, []int{0, 1}}} {
		if err := apply("private-voter-1", tc.event, tc.rev, tc.indices); err == nil {
			t.Fatalf("accepted %s", tc.event)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := u.ApplyPollVote(canceled, "chat", "poll", "private-voter-1", "cancel", 14, []int{0}); err == nil {
		t.Fatal("canceled update")
	}
	if err := u.ApplyPollVote(ctx, "chat", "missing", "v", "unknown", 14, []int{0}); err == nil {
		t.Fatal("unknown target")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	var page struct {
		Messages []Message `json:"messages"`
	}
	if err = json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].PollResults == nil {
		t.Fatalf("missing aggregate: %s", data)
	}
	r := page.Messages[0].PollResults
	if r.Options[0].VoteCount != 0 || r.Options[1].VoteCount != 2 {
		t.Fatalf("wrong aggregate: %s", data)
	}
	for _, secret := range []string{"private-voter", "event-", "option_indices", "selected_options"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("private data exposed: %s", data)
		}
	}
	u = any(a).(pollUpdater)
	if _, err = a.db.Exec("INSERT INTO message_state(chat_id,message_id,deleted) VALUES('chat','poll',1)"); err != nil {
		t.Fatal(err)
	}
	if err = u.ApplyPollVote(ctx, "chat", "poll", "v", "deleted", 15, []int{0}); err == nil {
		t.Fatal("deleted target")
	}
}

func TestPollVotingCapabilitiesAndGuards(t *testing.T) {
	a, p, _ := votingFixture(t)
	ctx := context.Background()
	caps := a.capabilities()
	features := caps["features"].(map[string]map[string]any)
	if f, ok := features["poll-voting"]; !ok || f["available"] != true {
		t.Fatal("poll voting capability missing")
	}
	if f, ok := features["poll-results"]; !ok || f["available"] != true {
		t.Fatal("poll results capability missing")
	}
	for _, target := range []string{"missing", "protected", "deleted"} {
		if target != "missing" {
			kind := "unsupported"
			if target == "deleted" {
				kind = "poll"
			}
			m := Message{ID: target, ChatID: "chat", Kind: kind, Status: "received"}
			if kind == "poll" {
				m.Poll = &PollProjection{Question: "Pick?", Options: []string{"A"}}
			}
			if err := a.Record(ctx, m, "Synthetic"); err != nil {
				t.Fatal(err)
			}
			if target == "deleted" {
				if err := a.applyMessageDeletion(ctx, "chat", target, "local_only"); err != nil {
					t.Fatal(err)
				}
			}
		}
		raw, _ := json.Marshal(map[string]any{"chat_id": "chat", "target_id": target, "option_indices": []int{0}, "client_id": target})
		if _, err := a.Handle(ctx, "poll.vote", raw); err == nil {
			t.Fatalf("voted on %s", target)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.Handle(canceled, "poll.vote", json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[0],"client_id":"cancel"}`)); err == nil {
		t.Fatal("canceled vote")
	}
	if p.calls != 0 {
		t.Fatal("invalid vote reached transport")
	}
}

type definitePollRejection struct{}

func (definitePollRejection) Error() string          { return "vote was rejected before send" }
func (definitePollRejection) PollVoteRejected() bool { return true }

type rejectingPollProbe struct{ pollSendProbe }

func (p *rejectingPollProbe) SendPollVote(context.Context, Message, []string, string) (int64, error) {
	p.calls++
	return 0, definitePollRejection{}
}
func TestPollVotingRejectedDurable(t *testing.T) {
	a, _, dir := votingFixture(t)
	p := &rejectingPollProbe{}
	a.SetTransport(p)
	a.state = "connected"
	raw := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[0],"client_id":"rejected"}`)
	if _, err := a.Handle(context.Background(), "poll.vote", raw); err == nil {
		t.Fatal("accepted rejection")
	}
	var status string
	if err := a.db.QueryRow("SELECT status FROM message_actions WHERE client_id='rejected'").Scan(&status); err != nil || status != "rejected" {
		t.Fatalf("rejection state=%s err=%v", status, err)
	}
	a.Close()
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(p)
	a.state = "connected"
	if _, err = a.Handle(context.Background(), "poll.vote", raw); err == nil {
		t.Fatal("rejected ID accepted")
	}
	if p.calls != 1 {
		t.Fatal("rejected ID resent")
	}
}

func TestPollVotingCanonicalStorageAndCorruption(t *testing.T) {
	a, _, _ := votingFixture(t)
	ctx := context.Background()
	if _, err := a.db.Exec(`UPDATE message_polls SET projection='{"question":"Pick?","options":["A","B"]}' WHERE message_id='poll'`); err != nil {
		t.Fatal(err)
	}
	u, ok := any(a).(pollUpdater)
	if !ok {
		t.Fatal("missing updater")
	}
	if err := u.ApplyPollVote(ctx, "chat", "poll", "voter", "event", 10, []int{1, 0}); err != nil {
		t.Fatal(err)
	}
	var choices string
	if err := a.db.QueryRow("SELECT choices FROM poll_latest_votes").Scan(&choices); err != nil {
		t.Fatal(err)
	}
	if choices != "[0,1]" {
		t.Fatalf("stored noncanonical choices: %s", choices)
	}
	if _, err := a.db.Exec("UPDATE poll_latest_votes SET choices='[12]'"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"chat","limit":10}`)); err == nil {
		t.Fatal("corrupt choices hydrated")
	}
}

func TestPollVotingUnknownReopenAndProtectedUpdate(t *testing.T) {
	a, p, dir := votingFixture(t)
	ctx := context.Background()
	p.fail = true
	raw := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[0],"client_id":"unknown-reopen"}`)
	if _, err := a.Handle(ctx, "poll.vote", raw); err == nil {
		t.Fatal("unknown accepted")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p.app = a
	a.SetTransport(p)
	a.state = "connected"
	if _, err = a.Handle(ctx, "poll.vote", raw); err == nil {
		t.Fatal("unknown retried")
	}
	if p.calls != 1 {
		t.Fatal("unknown resent after reopen")
	}
	invalidID := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[1]}`)
	_, err = a.Handle(ctx, "poll.vote", invalidID)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_params" {
		t.Fatalf("missing client ID error=%v, want invalid_params", err)
	}
	if p.calls != 1 {
		t.Fatalf("invalid client ID reached transport: calls=%d", p.calls)
	}
	// A fresh random ID must not bypass a durable ambiguous outcome for this
	// poll. The UI uses this state to require explicit reconciliation.
	fresh := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[1],"client_id":"fresh-after-reopen"}`)
	if _, err = a.Handle(ctx, "poll.vote", fresh); err == nil {
		t.Fatal("fresh client ID bypassed unknown poll outcome")
	}
	if p.calls != 1 {
		t.Fatalf("unknown poll resent with fresh ID: calls=%d", p.calls)
	}
	result, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), `"poll_self_vote":{"state":"unknown","option_indices":[0]}`) {
		t.Fatalf("cross-restart unknown state was not projected: %s", data)
	}
	if err = a.Record(ctx, Message{ID: "protected", ChatID: "chat", Kind: "unsupported", Body: "[Disappearing message not retained]", Status: "received"}, "Synthetic"); err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyPollVote(ctx, "chat", "protected", "voter", "event", 10, []int{0}); err == nil {
		t.Fatal("protected update accepted")
	}
	var count int
	if err = a.db.QueryRow("SELECT COUNT(*) FROM poll_latest_votes").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial write: %d %v", count, err)
	}
}

func TestPollVotingUnknownCannotBeClearedByLaterRejectedAttempt(t *testing.T) {
	a, p, _ := votingFixture(t)
	ctx := context.Background()
	for _, action := range []struct {
		id     string
		status string
	}{
		{id: "legacy-unknown", status: "unknown"},
		{id: "later-rejected", status: "rejected"},
	} {
		if _, err := a.reserveAction(ctx, action.id, "poll.vote", "chat", "poll", "[0]"); err != nil {
			t.Fatal(err)
		}
		if err := a.dispatchAction(ctx, action.id); err != nil {
			t.Fatal(err)
		}
		if err := a.finishAction(ctx, action.id, action.status); err != nil {
			t.Fatal(err)
		}
	}
	fresh := json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[1],"client_id":"new-after-legacy-rejection"}`)
	if _, err := a.Handle(ctx, "poll.vote", fresh); err == nil {
		t.Fatal("later proven rejection cleared an older unresolved unknown vote")
	}
	if p.calls != 0 {
		t.Fatalf("unresolved vote reached transport after later rejection: calls=%d", p.calls)
	}
	result, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), `"poll_vote_state":"unknown"`) {
		t.Fatalf("legacy unresolved state was not projected: %s", data)
	}
}

func TestPollVotingPendingProjectionSurvivesRestartAsUnknown(t *testing.T) {
	a, p, dir := votingFixture(t)
	ctx := context.Background()
	if _, err := a.reserveAction(ctx, "pending-state", "poll.vote", "chat", "poll", "[1]"); err != nil {
		t.Fatal(err)
	}
	if err := a.dispatchAction(ctx, "pending-state"); err != nil {
		t.Fatal(err)
	}
	readState := func(want string) {
		t.Helper()
		result, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"chat","limit":10}`))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(result)
		var page struct {
			Messages []Message `json:"messages"`
		}
		if err = json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) != 1 {
			t.Fatalf("message.list returned %d poll rows: %s", len(page.Messages), data)
		}
		if page.Messages[0].PollVoteState != want {
			t.Fatalf("poll_vote_state=%q, want %q: %s", page.Messages[0].PollVoteState, want, data)
		}
	}
	readState("pending")
	if _, err := a.Handle(ctx, "poll.vote", json.RawMessage(`{"chat_id":"chat","target_id":"poll","option_indices":[0],"client_id":"fresh-while-pending"}`)); err == nil {
		t.Fatal("fresh client ID bypassed a pending dispatch")
	}
	if p.calls != 0 {
		t.Fatalf("pending poll action reached transport: calls=%d", p.calls)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	readState("unknown")
}

func TestPollVotingUpdateCapability(t *testing.T) {
	a, _, _ := votingFixture(t)
	feature, ok := a.capabilities()["features"].(map[string]map[string]any)["poll-updates"]
	if !ok || feature["available"] != true {
		t.Fatal("backend poll update capability missing")
	}
}
