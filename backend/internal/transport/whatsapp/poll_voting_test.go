package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"path/filepath"
	"testing"
	"time"
)

type syntheticPollSecrets struct {
	store.MsgSecretStore
	sender types.JID
}

func (s syntheticPollSecrets) GetMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID) ([]byte, types.JID, error) {
	return bytes.Repeat([]byte{7}, 32), s.sender, ctx.Err()
}

type pollBuilder interface {
	buildPollVote(context.Context, *whatsmeow.Client, app.Message, []string) (*waE2E.Message, error)
}

func TestPollVotingAcceptsRemoteVoteOnLocallyCreatedPoll(t *testing.T) {
	ctx := context.Background()
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	owner := types.NewJID("15550000001", types.DefaultUserServer)
	voter := types.NewJID("15550000002", types.DefaultUserServer)
	chat := types.NewJID("123456", types.GroupServer)
	target := app.Message{ID: "local-poll", ChatID: chat.String(), SenderID: owner.String(), Kind: "poll", Status: "accepted", Outgoing: true, Body: "Pick?", Poll: &app.PollProjection{Question: "Pick?", Options: []string{"A", "B"}}}
	if err = a.Record(ctx, target, "Synthetic poll owner"); err != nil {
		t.Fatal(err)
	}
	adapter := New(a, "")
	client := whatsmeow.NewClient(&store.Device{ID: &voter, MsgSecrets: syntheticPollSecrets{sender: owner}}, nil)
	message, err := adapter.buildPollVote(ctx, client, target, []string{"B"})
	if err != nil {
		t.Fatal(err)
	}
	key := message.GetPollUpdateMessage().GetPollCreationMessageKey()
	if key == nil || !key.GetFromMe() || key.GetID() != target.ID || key.GetRemoteJID() != chat.String() || key.GetParticipant() != owner.String() {
		t.Fatalf("SDK vote does not reference the local poll identity: %+v", key)
	}
	event := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: voter, IsGroup: true}, ID: "remote-vote", Timestamp: time.Now()}, Message: message}
	adapter.event(ctx, client, event)
	result, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"`+chat.String()+`","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].PollResults == nil || page.Messages[0].PollResults.Options[1].VoteCount != 1 {
		t.Fatalf("remote vote on local poll was not aggregated: %s", encoded)
	}
}

func TestHistorySyncPollVoteUpdatesAggregate(t *testing.T) {
	ctx := context.Background()
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	owner := types.NewJID("15550000001", types.DefaultUserServer)
	voter := types.NewJID("15550000002", types.DefaultUserServer)
	chat := types.NewJID("123456", types.GroupServer)
	target := app.Message{ID: "history-poll", ChatID: chat.String(), SenderID: owner.String(), Kind: "poll", Status: "received", Body: "Pick?", Poll: &app.PollProjection{Question: "Pick?", Options: []string{"A", "B"}}}
	if err = a.Record(ctx, target, "Synthetic poll owner"); err != nil {
		t.Fatal(err)
	}
	adapter := New(a, "")
	client := whatsmeow.NewClient(&store.Device{ID: &voter, MsgSecrets: syntheticPollSecrets{sender: owner}}, nil)
	vote, err := adapter.buildPollVote(ctx, client, target, []string{"B"})
	if err != nil {
		t.Fatal(err)
	}
	webMessage := &waWeb.WebMessageInfo{
		Key: &waCommon.MessageKey{
			RemoteJID:   proto.String(chat.String()),
			FromMe:      proto.Bool(false),
			ID:          proto.String("history-vote"),
			Participant: proto.String(voter.String()),
		},
		Participant:      proto.String(voter.String()),
		Message:          vote,
		MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
	}
	adapter.event(ctx, client, &events.HistorySync{Data: &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{{
			ID:   proto.String(chat.String()),
			Name: proto.String("Synthetic poll group"),
			Messages: []*waHistorySync.HistorySyncMsg{{
				Message: webMessage,
			}},
		}},
	}})
	result, err := a.Handle(ctx, "message.list", []byte(`{"chat_id":"`+chat.String()+`","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].PollResults == nil || page.Messages[0].PollResults.Options[1].VoteCount != 1 {
		t.Fatalf("history-sync poll vote was not aggregated: %s", encoded)
	}
}

func TestPollVotingSDKBoundary(t *testing.T) {
	ctx := context.Background()
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	owner := types.NewJID("15550000001", types.DefaultUserServer)
	voter := types.NewJID("15550000002", types.DefaultUserServer)
	chat := types.NewJID("123456", types.GroupServer)
	target := app.Message{ID: "poll", ChatID: chat.String(), SenderID: owner.String(), Kind: "poll", Status: "received", Body: "Pick?", Poll: &app.PollProjection{Question: "Pick?", Options: []string{"A", "B"}}}
	if err = a.Record(ctx, target, "Synthetic"); err != nil {
		t.Fatal(err)
	}
	adapter := New(a, "")
	builder, ok := any(adapter).(pollBuilder)
	if !ok {
		t.Fatal("SDK poll vote builder missing")
	}
	c := whatsmeow.NewClient(&store.Device{ID: &voter, MsgSecrets: syntheticPollSecrets{sender: owner}}, nil)
	message, err := builder.buildPollVote(ctx, c, target, []string{"B"})
	if err != nil {
		t.Fatal(err)
	}
	if message.GetPollUpdateMessage() == nil || len(message.GetPollUpdateMessage().GetVote().GetEncPayload()) == 0 {
		t.Fatal("vote is not encrypted")
	}
	event := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: voter, IsGroup: true}, ID: "vote", Timestamp: time.Now()}, Message: message}
	adapter.event(ctx, c, event)
	result, err := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"`+chat.String()+`","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	json.Unmarshal(data, &page)
	if len(page.Messages) != 1 || page.Messages[0].PollResults == nil || page.Messages[0].PollResults.Options[1].VoteCount != 1 {
		t.Fatalf("SDK update not aggregated: %s", data)
	}
	// Each rejected event must leave the original poll and aggregate unchanged.
	for _, name := range []string{"duplicate", "stale", "unknown-target", "protected", "canceled", "unknown-option", "duplicate-option", "mismatched-from-me"} {
		t.Run(name, func(t *testing.T) {
			labels := []string{"A"}
			if name == "unknown-option" {
				labels = []string{"absent"}
			}
			if name == "duplicate-option" {
				labels = []string{"A", "A"}
			}
			candidate, buildErr := builder.buildPollVote(ctx, c, target, labels)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			candidate.PollUpdateMessage.SenderTimestampMS = proto.Int64(message.PollUpdateMessage.GetSenderTimestampMS() + 1)
			next := &events.Message{Info: event.Info, Message: candidate}
			next.Info.ID = "next-" + name
			callback := ctx
			switch name {
			case "duplicate":
				next = event
			case "stale":
				candidate.PollUpdateMessage.SenderTimestampMS = proto.Int64(1)
			case "unknown-target":
				candidate.PollUpdateMessage.PollCreationMessageKey.ID = proto.String("absent")
			case "protected":
				next.IsEphemeral = true
			case "canceled":
				var cancel context.CancelFunc
				callback, cancel = context.WithCancel(ctx)
				cancel()
			case "mismatched-from-me":
				candidate.PollUpdateMessage.PollCreationMessageKey.FromMe = proto.Bool(true)
				next.Info.Sender = owner
			}
			adapter.event(callback, c, next)
			got, listErr := a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"`+chat.String()+`","limit":10}`))
			if listErr != nil {
				t.Fatal(listErr)
			}
			encoded, _ := json.Marshal(got)
			if !bytes.Equal(data, encoded) {
				t.Fatalf("rejected event changed history: %s", encoded)
			}
		})
	}
	// Tampered encrypted payload must not alter the stored aggregate.
	message.PollUpdateMessage.Vote.EncPayload[0] ^= 1
	event.Info.ID = "tampered"
	adapter.event(ctx, c, event)
	result, err = a.Handle(ctx, "message.list", json.RawMessage(`{"chat_id":"`+chat.String()+`","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(result)
	if !bytes.Equal(data, after) {
		t.Fatal("malformed ciphertext changed history")
	}
}
