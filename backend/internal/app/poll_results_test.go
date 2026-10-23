package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestPollResultsProjectionPersistsAcrossReopenAndMessageList(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	const event = `{"id":"synthetic-poll-results","chat_id":"synthetic-poll-chat","body":"Weekend plan?","timestamp":1234,"status":"received","kind":"poll_results","poll_results":{"question":"Weekend plan?","options":[{"name":"Beach","vote_count":2},{"name":"Hiking","vote_count":0}]}}`
	var message Message
	if err = json.Unmarshal([]byte(event), &message); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), message, "Synthetic poll"); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-poll-chat","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("unexpected message.list result: %s", encoded)
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
		t.Fatalf("poll results changed across persistence/reopen/list: %#v", projection)
	}
}

func TestPollResultsRecordRejectsInvalidProjectionWithoutPersisting(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	oversized := PollResultsProjection{Question: strings.Repeat("🌿", PollQuestionMaxRunes)}
	oversized.Options = make([]PollResultOption, PollOptionMaxCount)
	for i := range oversized.Options {
		oversized.Options[i] = PollResultOption{Name: strings.Repeat("🌿", PollOptionMaxRunes), VoteCount: 1}
	}
	cases := []struct {
		name    string
		results PollResultsProjection
	}{
		{"invalid-question-utf8", PollResultsProjection{Question: string([]byte{0xff}), Options: []PollResultOption{{Name: "One"}}}},
		{"question-control", PollResultsProjection{Question: "Unsafe\nquestion", Options: []PollResultOption{{Name: "One"}}}},
		{"option-control", PollResultsProjection{Question: "Question", Options: []PollResultOption{{Name: "Bad	option"}}}},
		{"negative-count", PollResultsProjection{Question: "Question", Options: []PollResultOption{{Name: "One", VoteCount: -1}}}},
		{"count-exceeds-json-safe-range", PollResultsProjection{Question: "Question", Options: []PollResultOption{{Name: "One", VoteCount: PollVoteCountMax + 1}}}},
		{"too-many-options", PollResultsProjection{Question: "Question", Options: make([]PollResultOption, PollOptionMaxCount+1)}},
		{"serialized-projection-too-large", oversized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "invalid-poll-results-" + tc.name
			message := Message{ID: id, ChatID: "invalid-poll-results-chat", Body: "Poll results", Kind: "poll_results", Status: "received", PollResults: &tc.results}
			if err = a.Record(context.Background(), message, "Synthetic poll"); err == nil {
				t.Fatal("Record accepted an invalid poll-results projection")
			}
			var rows int
			if scanErr := a.db.QueryRow("SELECT count(*) FROM messages WHERE id=?", id).Scan(&rows); scanErr != nil || rows != 0 {
				t.Fatalf("invalid poll results partially changed history: rows=%d err=%v", rows, scanErr)
			}
		})
	}
}

func TestMessageListRejectsCorruptStoredPollResultsProjection(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	results := PollResultsProjection{Question: "Question", Options: []PollResultOption{{Name: "One", VoteCount: 1}}}
	message := Message{ID: "corrupted-poll-results", ChatID: "stored-poll-results-chat", Body: "Question", Kind: "poll_results", Status: "received", PollResults: &results}
	if err = a.Record(context.Background(), message, "Synthetic poll"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("UPDATE message_poll_results SET projection=? WHERE chat_id=? AND message_id=?", `{"question":"Question","options":[{"name":"One","vote_count":1,"voter":"private-sentinel"}]}`, message.ChatID, message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"stored-poll-results-chat","limit":10}`)); err == nil {
		t.Fatal("message.list returned a corrupt stored poll-results projection")
	}
}
