package whatsapp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestIncomingPollResultSnapshotPersistsOnlyAggregateProjection(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	history, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	chat, err := types.ParseJID("synthetic-poll-results@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(history, t.TempDir())
	snapshot := &waE2E.PollResultSnapshotMessage{
		Name: proto.String("Weekend plan?"),
		PollVotes: []*waE2E.PollResultSnapshotMessage_PollVote{
			{OptionName: proto.String("Beach"), OptionVoteCount: proto.Int64(2)},
			{OptionName: proto.String("Hiking"), OptionVoteCount: proto.Int64(0)},
		},
		ContextInfo: &waE2E.ContextInfo{
			StanzaID:      proto.String("private-stanza-sentinel"),
			Participant:   proto.String("private-participant-sentinel"),
			QuotedMessage: &waE2E.Message{Conversation: proto.String("private-quoted-text-sentinel")},
		},
	}
	adapter.record(context.Background(), &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat},
			ID:            "synthetic-poll-result-snapshot",
			Timestamp:     time.Unix(1234, 0),
		},
		Message: &waE2E.Message{PollResultSnapshotMessage: snapshot},
	}, "Synthetic poll results")

	result, err := history.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-poll-results@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-stanza-sentinel", "private-participant-sentinel", "private-quoted-text-sentinel", "ContextInfo", "Participant", "StanzaID"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("poll-results projection leaked private protocol metadata %q: %s", secret, encoded)
		}
	}
	var page struct {
		Messages []struct {
			Kind        string          `json:"kind"`
			PollResults json.RawMessage `json:"poll_results"`
		} `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Kind != "poll_results" {
		t.Fatalf("incoming result snapshot was not projected: %s", encoded)
	}
	var projection struct {
		Question string `json:"question"`
		Options  []struct {
			Name      string `json:"name"`
			VoteCount int64  `json:"vote_count"`
		} `json:"options"`
	}
	if err = json.Unmarshal(page.Messages[0].PollResults, &projection); err != nil {
		t.Fatalf("message.list omitted the typed poll-results projection: %s", encoded)
	}
	if projection.Question != "Weekend plan?" || len(projection.Options) != 2 ||
		projection.Options[0].Name != "Beach" || projection.Options[0].VoteCount != 2 ||
		projection.Options[1].Name != "Hiking" || projection.Options[1].VoteCount != 0 {
		t.Fatalf("aggregate poll results changed across the adapter boundary: %#v", projection)
	}
}

func TestMalformedPollResultSnapshotsFailClosed(t *testing.T) {
	vote := func(name string, count int64) *waE2E.PollResultSnapshotMessage_PollVote {
		return &waE2E.PollResultSnapshotMessage_PollVote{OptionName: proto.String(name), OptionVoteCount: proto.Int64(count)}
	}
	valid := func(question string) *waE2E.PollResultSnapshotMessage {
		return &waE2E.PollResultSnapshotMessage{Name: proto.String(question), PollVotes: []*waE2E.PollResultSnapshotMessage_PollVote{vote("One", 1)}}
	}
	oversized := &waE2E.PollResultSnapshotMessage{Name: proto.String(strings.Repeat("🌿", app.PollQuestionMaxRunes))}
	for i := 0; i < app.PollOptionMaxCount; i++ {
		oversized.PollVotes = append(oversized.PollVotes, vote(strings.Repeat("🌿", app.PollOptionMaxRunes), 1))
	}
	cases := []struct {
		name     string
		snapshot *waE2E.PollResultSnapshotMessage
	}{
		{"missing-question", &waE2E.PollResultSnapshotMessage{PollVotes: []*waE2E.PollResultSnapshotMessage_PollVote{vote("One", 1)}}},
		{"empty-question", &waE2E.PollResultSnapshotMessage{Name: proto.String(""), PollVotes: []*waE2E.PollResultSnapshotMessage_PollVote{vote("One", 1)}}},
		{"invalid-question-utf8", func() *waE2E.PollResultSnapshotMessage { p := valid("Question"); p.Name = proto.String(string([]byte{0xff})); return p }()},
		{"question-control", valid("Question\ncontinued")},
		{"too-many-options", func() *waE2E.PollResultSnapshotMessage {
			p := valid("Question")
			p.PollVotes = make([]*waE2E.PollResultSnapshotMessage_PollVote, app.PollOptionMaxCount+1)
			for i := range p.PollVotes { p.PollVotes[i] = vote("Option", 0) }
			return p
		}()},
		{"nil-option", func() *waE2E.PollResultSnapshotMessage { p := valid("Question"); p.PollVotes[0] = nil; return p }()},
		{"missing-option-name", func() *waE2E.PollResultSnapshotMessage { p := valid("Question"); p.PollVotes[0].OptionName = nil; return p }()},
		{"option-control", func() *waE2E.PollResultSnapshotMessage { return &waE2E.PollResultSnapshotMessage{Name: proto.String("Question"), PollVotes: []*waE2E.PollResultSnapshotMessage_PollVote{vote("Bad	option", 0)}} }()},
		{"missing-count", func() *waE2E.PollResultSnapshotMessage { p := valid("Question"); p.PollVotes[0].OptionVoteCount = nil; return p }()},
		{"negative-count", validNegativePollResult()},
		{"serialized-projection-too-large", oversized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := messageContent(&events.Message{Message: &waE2E.Message{PollResultSnapshotMessage: tc.snapshot}})
			if content.kind != "unsupported" || content.pollResults != nil {
				t.Fatalf("invalid poll-results snapshot was not rejected: %#v", content)
			}
		})
	}
}

func validNegativePollResult() *waE2E.PollResultSnapshotMessage {
	return &waE2E.PollResultSnapshotMessage{Name: proto.String("Question"), PollVotes: []*waE2E.PollResultSnapshotMessage_PollVote{{OptionName: proto.String("One"), OptionVoteCount: proto.Int64(-1)}}}
}
