package whatsapp

import (
	"context"
	"database/sql"
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

func TestPollCreationVersionsAreRecognized(t *testing.T) {
	makePoll := func() *waE2E.PollCreationMessage {
		return &waE2E.PollCreationMessage{
			Name: proto.String("Synthetic question"),
			Options: []*waE2E.PollCreationMessage_Option{
				{OptionName: proto.String("Option one"), OptionHash: proto.String("option-hash-sentinel")},
				{OptionName: proto.String("Option two"), OptionHash: proto.String("option-hash-sentinel-two")},
			},
			SelectableOptionsCount: proto.Uint32(1),
			EncKey:                 []byte("encryption-key-sentinel"),
		}
	}
	cases := []struct {
		name    string
		message *waE2E.Message
	}{
		{"v1", &waE2E.Message{PollCreationMessage: makePoll()}},
		{"v2", &waE2E.Message{PollCreationMessageV2: makePoll()}},
		{"v3", &waE2E.Message{PollCreationMessageV3: makePoll()}},
		{"v4", &waE2E.Message{PollCreationMessageV4: &waE2E.FutureProofMessage{Message: &waE2E.Message{PollCreationMessage: makePoll()}}}},
		{"v5", &waE2E.Message{PollCreationMessageV5: makePoll()}},
		{"v6", &waE2E.Message{PollCreationMessageV6: makePoll()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := messageContent(&events.Message{Message: tc.message})
			if content.kind != "poll" || content.body != "Synthetic question" {
				t.Fatalf("poll creation version was not recognized: %#v", content)
			}
		})
	}
}

func TestIncomingPollCreationVersionsExposeOnlyTypedProjection(t *testing.T) {
	makePoll := func(question string, withSelectableCount bool) *waE2E.PollCreationMessage {
		poll := &waE2E.PollCreationMessage{
			Name: proto.String(question),
			Options: []*waE2E.PollCreationMessage_Option{
				{OptionName: proto.String("Option <one>"), OptionHash: proto.String("option-hash-sentinel")},
				{OptionName: proto.String("Option & two"), OptionHash: proto.String("option-hash-sentinel-two")},
			},
			EncKey:        []byte("encryption-key-sentinel"),
			CorrectAnswer: &waE2E.PollCreationMessage_Option{OptionName: proto.String("correct-answer-sentinel"), OptionHash: proto.String("correct-answer-hash-sentinel")},
		}
		if withSelectableCount {
			poll.SelectableOptionsCount = proto.Uint32(1)
		}
		return poll
	}
	cases := []struct {
		name    string
		message *waE2E.Message
		count   bool
	}{
		{"v1", &waE2E.Message{PollCreationMessage: makePoll("Question v1", true)}, true},
		{"v2", &waE2E.Message{PollCreationMessageV2: makePoll("Question v2", false)}, false},
		{"v3", &waE2E.Message{PollCreationMessageV3: makePoll("Question v3", true)}, true},
		{"v4", &waE2E.Message{PollCreationMessageV4: &waE2E.FutureProofMessage{Message: &waE2E.Message{PollCreationMessage: makePoll("Question v4", true)}}}, true},
		{"v5", &waE2E.Message{PollCreationMessageV5: makePoll("Question v5", true)}, true},
		{"v6", &waE2E.Message{PollCreationMessageV6: makePoll("Question v6", true)}, true},
	}
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat, err := types.ParseJID("synthetic-polls@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	for index, tc := range cases {
		adapter.record(context.Background(), &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: chat},
				ID:            "poll-" + tc.name,
				Timestamp:     time.Unix(int64(index+1), 0),
			},
			Message: tc.message,
		}, "Synthetic poll")
	}
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-polls@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{"encryption-key-sentinel", "option-hash-sentinel", "correct-answer-sentinel", "correct-answer-hash-sentinel", "EncKey", "OptionHash", "CorrectAnswer"}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("poll projection leaked protocol or answer data %q: %s", secret, encoded)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(profile, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored, err := db.Query("SELECT projection FROM message_polls ORDER BY message_id")
	if err != nil {
		t.Fatal(err)
	}
	storedCount := 0
	for stored.Next() {
		var projection string
		if err = stored.Scan(&projection); err != nil {
			stored.Close()
			t.Fatal(err)
		}
		storedCount++
		for _, secret := range secrets {
			if strings.Contains(projection, secret) {
				stored.Close()
				t.Fatalf("SQLite poll projection leaked protocol or answer data %q: %s", secret, projection)
			}
		}
	}
	if err = stored.Err(); err != nil {
		stored.Close()
		t.Fatal(err)
	}
	stored.Close()
	if storedCount != len(cases) {
		t.Fatalf("SQLite stored %d poll projections, want %d", storedCount, len(cases))
	}
	var page struct {
		Messages []struct {
			ID   string         `json:"id"`
			Kind string         `json:"kind"`
			Poll map[string]any `json:"poll"`
		} `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != len(cases) {
		t.Fatalf("message.list returned %d poll messages, want %d: %s", len(page.Messages), len(cases), encoded)
	}
	byID := make(map[string]map[string]any, len(page.Messages))
	for _, message := range page.Messages {
		byID[message.ID] = message.Poll
		if message.Kind != "poll" {
			t.Fatalf("message %s has kind %q, want poll", message.ID, message.Kind)
		}
	}
	for _, tc := range cases {
		poll := byID["poll-"+tc.name]
		if poll == nil || poll["question"] != "Question "+tc.name {
			t.Fatalf("%s poll question was not returned as typed content: %#v", tc.name, poll)
		}
		options, ok := poll["options"].([]any)
		if !ok || len(options) != 2 || options[0] != "Option <one>" || options[1] != "Option & two" {
			t.Fatalf("%s poll options were not projected as bounded labels: %#v", tc.name, poll)
		}
		count, present := poll["selectable_options_count"]
		if present != tc.count || (present && count != float64(1)) {
			t.Fatalf("%s selectable option count = %#v, present=%v, want present=%v", tc.name, count, present, tc.count)
		}
	}
}

func TestInvalidPollProjectionFailsClosedAtAdapter(t *testing.T) {
	const fallback = "[Unsupported content — open the official app]"
	valid := func() *waE2E.PollCreationMessage {
		return &waE2E.PollCreationMessage{
			Name:    proto.String("Synthetic question"),
			Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Option one")}},
		}
	}
	cases := []struct {
		name string
		poll *waE2E.PollCreationMessage
	}{
		{"invalid-question-utf8", func() *waE2E.PollCreationMessage { p := valid(); p.Name = proto.String(string([]byte{0xff})); return p }()},
		{"question-too-long", func() *waE2E.PollCreationMessage {
			p := valid()
			p.Name = proto.String(strings.Repeat("q", app.PollQuestionMaxRunes+1))
			return p
		}()},
		{"question-control", func() *waE2E.PollCreationMessage { p := valid(); p.Name = proto.String("line one\nline two"); return p }()},
		{"too-many-options", func() *waE2E.PollCreationMessage {
			p := valid()
			p.Options = make([]*waE2E.PollCreationMessage_Option, app.PollOptionMaxCount+1)
			for i := range p.Options {
				p.Options[i] = &waE2E.PollCreationMessage_Option{OptionName: proto.String("option")}
			}
			return p
		}()},
		{"option-too-long", func() *waE2E.PollCreationMessage {
			p := valid()
			p.Options[0].OptionName = proto.String(strings.Repeat("o", app.PollOptionMaxRunes+1))
			return p
		}()},
		{"option-control", func() *waE2E.PollCreationMessage {
			p := valid()
			p.Options[0].OptionName = proto.String("tab\toption")
			return p
		}()},
		{"missing-option-label", func() *waE2E.PollCreationMessage { p := valid(); p.Options[0].OptionName = nil; return p }()},
		{"selectable-count-zero", func() *waE2E.PollCreationMessage { p := valid(); p.SelectableOptionsCount = proto.Uint32(0); return p }()},
		{"selectable-count-exceeds-options", func() *waE2E.PollCreationMessage { p := valid(); p.SelectableOptionsCount = proto.Uint32(2); return p }()},
		{"serialized-projection-too-large", func() *waE2E.PollCreationMessage {
			p := &waE2E.PollCreationMessage{Name: proto.String(strings.Repeat("🌿", app.PollQuestionMaxRunes))}
			for i := 0; i < app.PollOptionMaxCount; i++ {
				p.Options = append(p.Options, &waE2E.PollCreationMessage_Option{OptionName: proto.String(strings.Repeat("🌿", app.PollOptionMaxRunes))})
			}
			return p
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := messageContent(&events.Message{Message: &waE2E.Message{PollCreationMessage: tc.poll}})
			if content.kind != "unsupported" || content.body != fallback || content.poll != nil {
				t.Fatalf("invalid poll did not fail closed to the safe unsupported preview: %#v", content)
			}
		})
	}
}
